package members_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/members"
)

var base64URLToken = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func (f *fixture) inviteRow(t *testing.T, id uuid.UUID) (uses int, revoked bool, expiresAt time.Time) {
	t.Helper()
	require.NoError(t, f.store.Pool().QueryRow(f.ctx,
		`SELECT uses, revoked_at IS NOT NULL, expires_at FROM invites WHERE id = $1`, id).Scan(&uses, &revoked, &expiresAt))
	return uses, revoked, expiresAt
}

func (f *fixture) pinClock() {
	f.clock.mu.Lock()
	defer f.clock.mu.Unlock()
	f.clock.now = f.clock.now.Truncate(time.Second)
}

func TestInviteTokensAreStrongBase64URLAndUnique(t *testing.T) {
	f := newFixture(t)
	seen := map[string]struct{}{}
	for range 150 {
		created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
		require.NoError(t, err)
		token := created.Token
		assert.Regexp(t, base64URLToken, token)
		assert.NotContains(t, token, "=")
		raw, err := base64.RawURLEncoding.DecodeString(token)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(raw)*8, 256, "at least 256 bits of entropy")
		_, dup := seen[token]
		require.False(t, dup)
		seen[token] = struct{}{}
	}
}

func TestOnlyTheTokenHashIsStoredAnywhereInTheDatabase(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleEditor, MaxUses: 2})
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.NoError(t, err)
	revoked, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	require.NoError(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, revoked.ID))
	_, err = f.svc.Accept(f.ctx, f.user(t, "late").ID, revoked.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable)

	var stored string
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT token_hash FROM invites WHERE id = $1`, created.ID).Scan(&stored))
	assert.Equal(t, members.HashToken(created.Token), stored)
	assert.Regexp(t, `^[0-9a-f]{64}$`, stored)
	assert.NotEqual(t, created.Token, stored)

	rows, err := f.store.Pool().Query(f.ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public'`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.NotEmpty(t, tables)
	for _, token := range []string{created.Token, revoked.Token} {
		for _, table := range tables {
			var hits int
			query := fmt.Sprintf(`SELECT count(*) FROM %s t WHERE row_to_json(t)::text LIKE '%%' || $1 || '%%'`, pgx.Identifier{table}.Sanitize())
			require.NoError(t, f.store.Pool().QueryRow(f.ctx, query, token).Scan(&hits))
			assert.Zero(t, hits, "the raw token must not appear in table %s", table)
		}
	}
}

func TestHashTokenIsSHA256Hex(t *testing.T) {
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", members.HashToken("abc"))
	assert.NotEqual(t, members.HashToken("abc"), members.HashToken("abd"))
}

func TestInviteGeneratorsAreInjectableAndTheirFailuresAbortCreation(t *testing.T) {
	f := newFixture(t)
	fixedID := uuid.New()
	svc := members.NewService(f.store, members.WithClock(f.clock),
		members.WithIDGenerator(func() (uuid.UUID, error) { return fixedID, nil }),
		members.WithTokenGenerator(func() (string, error) { return "fixed-token", nil }))
	created, err := svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	assert.Equal(t, fixedID, created.ID)
	assert.Equal(t, "fixed-token", created.Token)
	assert.Equal(t, members.HashToken("fixed-token"), created.TokenHash)

	boom := errors.New("entropy exhausted")
	failingToken := members.NewService(f.store, members.WithTokenGenerator(func() (string, error) { return "", boom }))
	_, err = failingToken.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.ErrorIs(t, err, boom)
	failingID := members.NewService(f.store, members.WithIDGenerator(func() (uuid.UUID, error) { return uuid.Nil, boom }))
	_, err = failingID.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.ErrorIs(t, err, boom)

	var count int
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM invites`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestInviteExpiryBoundaryFollowsTheClock(t *testing.T) {
	f := newFixture(t)
	f.pinClock()
	justBefore, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, ExpiresIn: time.Hour})
	require.NoError(t, err)
	atExpiry, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, ExpiresIn: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, f.clock.Now().Add(time.Hour), justBefore.ExpiresAt)
	ann, bob := f.user(t, "ann"), f.user(t, "bob")

	f.clock.Advance(time.Hour - time.Microsecond)
	_, err = f.svc.Accept(f.ctx, ann.ID, justBefore.Token)
	require.NoError(t, err, "one microsecond before expiry the invite still works")

	f.clock.Advance(time.Microsecond)
	_, err = f.svc.Accept(f.ctx, bob.ID, atExpiry.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable, "at the expiry instant the invite is already dead")

	f.clock.Advance(24 * time.Hour)
	uses, _, _ := f.inviteRow(t, atExpiry.ID)
	assert.Zero(t, uses, "an expired attempt consumes nothing")
}

func TestExpiredInvitesDisappearFromTheActiveList(t *testing.T) {
	f := newFixture(t)
	short, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, ExpiresIn: time.Hour})
	require.NoError(t, err)
	long, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleEditor, ExpiresIn: 48 * time.Hour})
	require.NoError(t, err)

	active, err := f.svc.ListInvites(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	require.Len(t, active, 2)
	assert.Equal(t, []uuid.UUID{short.ID, long.ID}, []uuid.UUID{active[0].ID, active[1].ID}, "ordered by expiry")

	f.clock.Advance(2 * time.Hour)
	active, err = f.svc.ListInvites(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, long.ID, active[0].ID)
}

func TestExhaustedAndRevokedInvitesAreNotListedAsActive(t *testing.T) {
	f := newFixture(t)
	single, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	revoked, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	partial, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 3})
	require.NoError(t, err)

	_, err = f.svc.Accept(f.ctx, f.user(t, "a").ID, single.Token)
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, f.user(t, "b").ID, partial.Token)
	require.NoError(t, err)
	require.NoError(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, revoked.ID))

	active, err := f.svc.ListInvites(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, partial.ID, active[0].ID)
	assert.Equal(t, 1, active[0].Uses)
	assert.Equal(t, 3, active[0].MaxUses)
}

func TestMaxUsesHoldsUnderManyRacers(t *testing.T) {
	f := newFixture(t)
	const racers = 24
	const maxUses = 7
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: maxUses})
	require.NoError(t, err)
	users := make([]domain.User, racers)
	for i := range users {
		users[i] = f.user(t, "racer")
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, racers)
	for i := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = f.svc.Accept(f.ctx, users[i].ID, created.Token)
		}()
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, domain.ErrInviteUnavailable)
		}
	}
	assert.Equal(t, maxUses, succeeded)
	uses, _, _ := f.inviteRow(t, created.ID)
	assert.Equal(t, maxUses, uses)
	assert.Len(t, f.roles(t), maxUses+1)

	events := f.events(t)
	requireGapFree(t, events)
	assert.Len(t, events, maxUses)
	assert.Equal(t, maxUses, f.publishedCountOf(boardcontent.EventMemberAdded))
	assert.Equal(t, replayMembers(t, events, f.owner.ID), f.roles(t))
}

func TestOneUserAcceptingConcurrentlyConsumesASingleUse(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleEditor, MaxUses: 5})
	require.NoError(t, err)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]domain.BoardWithRole, 10)
	errs := make([]error, 10)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.svc.Accept(f.ctx, bob.ID, created.Token)
		}()
	}
	close(start)
	wg.Wait()

	for i := range results {
		require.NoError(t, errs[i])
		assert.Equal(t, domain.RoleEditor, results[i].Role)
		assert.Equal(t, f.board.ID, results[i].ID)
	}
	uses, _, _ := f.inviteRow(t, created.ID)
	assert.Equal(t, 1, uses)
	assert.Len(t, f.events(t), 1)
}

func TestAcceptByExistingMembersNeverChangesTheirRoleOrConsumesAUse(t *testing.T) {
	f := newFixture(t)
	editor, viewer := f.user(t, "editor"), f.user(t, "viewer")
	f.add(t, editor, domain.RoleEditor)
	f.add(t, viewer, domain.RoleViewer)
	toEditor, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleEditor, MaxUses: 3})
	require.NoError(t, err)
	toViewer, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 3})
	require.NoError(t, err)
	seq := f.seq(t)

	got, err := f.svc.Accept(f.ctx, viewer.ID, toEditor.Token)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleViewer, got.Role, "an invite can never upgrade an existing member")
	got, err = f.svc.Accept(f.ctx, editor.ID, toViewer.Token)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleEditor, got.Role, "an invite can never downgrade an existing member")
	got, err = f.svc.Accept(f.ctx, f.owner.ID, toViewer.Token)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleOwner, got.Role, "the owner stays the owner")
	assert.Equal(t, f.board.ID, got.ID)

	assert.Equal(t, domain.RoleViewer, f.roles(t)[viewer.ID])
	assert.Equal(t, domain.RoleEditor, f.roles(t)[editor.ID])
	assert.Equal(t, domain.RoleOwner, f.roles(t)[f.owner.ID])
	for _, inv := range []domain.NewInvite{toEditor, toViewer} {
		uses, _, _ := f.inviteRow(t, inv.ID)
		assert.Zero(t, uses)
	}
	assert.Equal(t, seq, f.seq(t))
}

func TestMemberCanReuseARevokedOrExpiredInviteIdempotently(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 2, ExpiresIn: time.Hour})
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.NoError(t, err)

	require.NoError(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, created.ID))
	f.clock.Advance(2 * time.Hour)
	got, err := f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleViewer, got.Role)

	_, err = f.svc.Accept(f.ctx, f.user(t, "carol").ID, created.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable)
}

func TestRejoiningAfterRemovalConsumesAnotherUse(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 2})
	require.NoError(t, err)

	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.NoError(t, err)
	require.NoError(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID))
	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.NoError(t, err)
	uses, _, _ := f.inviteRow(t, created.ID)
	assert.Equal(t, 2, uses)

	require.NoError(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID))
	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable, "the invite is exhausted even for a returning user")
}

func TestRevokingKeepsTheFirstRevocationTimeAndIsScopedToTheBoard(t *testing.T) {
	f := newFixture(t)
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)

	require.NoError(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, created.ID))
	var first time.Time
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT revoked_at FROM invites WHERE id = $1`, created.ID).Scan(&first))
	f.clock.Advance(time.Hour)
	require.NoError(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, created.ID))
	var second time.Time
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT revoked_at FROM invites WHERE id = $1`, created.ID).Scan(&second))
	assert.True(t, first.Equal(second), "revoking twice must not move the revocation time")

	otherOwner := f.user(t, "other-owner")
	otherBoard, err := f.store.CreateBoardWithOwner(f.ctx, domain.Board{ID: uuid.New(), Title: "Other", OwnerID: otherOwner.ID})
	require.NoError(t, err)
	foreign, err := f.svc.CreateInvite(f.ctx, otherOwner.ID, otherBoard.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	require.ErrorIs(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, foreign.ID), domain.ErrNotFound, "an invite of another board cannot be revoked through this one")
	_, revoked, _ := f.inviteRow(t, foreign.ID)
	assert.False(t, revoked)
	require.ErrorIs(t, f.svc.RevokeInvite(f.ctx, otherOwner.ID, f.board.ID, created.ID), domain.ErrNotFound)

	listed, err := f.svc.ListInvites(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	assert.Empty(t, listed, "invites of other boards are never listed")
}

func TestAcceptingAnInviteOfADeletedBoardIsNotFound(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 3})
	require.NoError(t, err)
	_, err = boards.NewService(f.store).Delete(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)

	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.svc.Accept(f.ctx, f.owner.ID, created.Token)
	require.ErrorIs(t, err, domain.ErrNotFound, "even the owner gets nothing back for a deleted board")
	uses, _, _ := f.inviteRow(t, created.ID)
	assert.Zero(t, uses)
	var memberCount int
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM board_members WHERE board_id = $1`, f.board.ID).Scan(&memberCount))
	assert.Equal(t, 1, memberCount)
}

func TestInviteManagementOnDeletedOrForeignBoards(t *testing.T) {
	f := newFixture(t)
	editor, stranger := f.user(t, "editor"), f.user(t, "stranger")
	f.add(t, editor, domain.RoleEditor)

	_, err := f.svc.CreateInvite(f.ctx, editor.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.svc.CreateInvite(f.ctx, stranger.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.svc.ListInvites(f.ctx, editor.ID, f.board.ID)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.svc.ListInvites(f.ctx, stranger.ID, f.board.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)

	_, err = boards.NewService(f.store).Delete(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	_, err = f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.svc.ListInvites(f.ctx, f.owner.ID, f.board.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.ErrorIs(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, uuid.New()), domain.ErrNotFound)
}

func TestInviteLimitBoundariesAndDefaults(t *testing.T) {
	f := newFixture(t)
	f.pinClock()
	defaults, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	assert.Equal(t, members.DefaultInviteMaxUses, defaults.MaxUses)
	assert.Equal(t, 1, defaults.MaxUses)
	assert.Equal(t, f.clock.Now().Add(7*24*time.Hour), defaults.ExpiresAt)

	accepted := []members.InviteInput{
		{Role: domain.RoleViewer, MaxUses: 1},
		{Role: domain.RoleEditor, MaxUses: members.MaxInviteMaxUses},
		{Role: domain.RoleViewer, ExpiresIn: members.MaxInviteTTL},
		{Role: domain.RoleViewer, ExpiresIn: time.Second},
		{Role: domain.RoleViewer, ExpiresIn: time.Nanosecond},
	}
	for _, in := range accepted {
		created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, in)
		require.NoError(t, err, "%+v", in)
		if in.MaxUses != 0 {
			assert.Equal(t, in.MaxUses, created.MaxUses)
		}
	}
	assert.Equal(t, 30*24*time.Hour, members.MaxInviteTTL)
	assert.Equal(t, 100, members.MaxInviteMaxUses)

	rejected := []members.InviteInput{
		{Role: domain.RoleViewer, ExpiresIn: members.MaxInviteTTL + time.Nanosecond},
		{Role: domain.RoleViewer, ExpiresIn: -time.Nanosecond},
		{Role: domain.RoleViewer, MaxUses: members.MaxInviteMaxUses + 1},
		{Role: domain.RoleViewer, MaxUses: -1},
		{Role: domain.RoleOwner},
		{},
		{Role: domain.Role("Viewer")},
	}
	var before int
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM invites`).Scan(&before))
	for _, in := range rejected {
		_, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, in)
		var vErr *domain.ValidationError
		require.ErrorAs(t, err, &vErr, "%+v", in)
	}
	var after int
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM invites`).Scan(&after))
	assert.Equal(t, before, after)
}

func TestOwnerRulesAcrossEveryEntryPoint(t *testing.T) {
	f := newFixture(t)
	editor, viewer := f.user(t, "editor"), f.user(t, "viewer")
	f.add(t, editor, domain.RoleEditor)
	f.add(t, viewer, domain.RoleViewer)
	seq := f.seq(t)

	require.ErrorIs(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, f.owner.ID), domain.ErrForbidden, "the owner cannot leave")
	require.ErrorIs(t, f.svc.Remove(f.ctx, editor.ID, f.board.ID, f.owner.ID), domain.ErrForbidden)
	require.ErrorIs(t, f.svc.Remove(f.ctx, viewer.ID, f.board.ID, f.owner.ID), domain.ErrForbidden)
	for _, role := range []domain.Role{domain.RoleOwner, domain.RoleEditor, domain.RoleViewer} {
		_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, f.owner.ID, role)
		var vErr *domain.ValidationError
		require.Error(t, err, role)
		if role != domain.RoleOwner {
			require.ErrorAs(t, err, &vErr, "the owner cannot be demoted to %s", role)
		}
	}
	_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, editor.ID, domain.RoleOwner)
	var vErr *domain.ValidationError
	require.ErrorAs(t, err, &vErr, "ownership cannot be handed out through a role change")
	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, f.user(t, "x").Email, domain.RoleOwner)
	require.ErrorAs(t, err, &vErr)
	_, err = f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleOwner})
	require.ErrorAs(t, err, &vErr)
	_, err = f.svc.ChangeRole(f.ctx, editor.ID, f.board.ID, f.owner.ID, domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrForbidden)

	var owners int
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT count(*) FROM board_members WHERE board_id = $1 AND role = 'owner'`, f.board.ID).Scan(&owners))
	assert.Equal(t, 1, owners, "a board has exactly one owner")
	assert.Equal(t, seq, f.seq(t), "none of the rejected attempts logged an event")
}

func TestEditorsAndViewersCanLeaveOnTheirOwn(t *testing.T) {
	for _, role := range []domain.Role{domain.RoleEditor, domain.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			f := newFixture(t)
			leaver, other := f.user(t, "leaver"), f.user(t, "other")
			f.add(t, leaver, role)
			f.add(t, other, domain.RoleViewer)

			require.NoError(t, f.svc.Remove(f.ctx, leaver.ID, f.board.ID, leaver.ID))
			events := f.events(t)
			last := events[len(events)-1]
			assert.Equal(t, boardcontent.EventMemberRemoved, last.Type)
			require.NotNil(t, last.ActorID)
			assert.Equal(t, leaver.ID, *last.ActorID)
			var payload boardcontent.MemberRemoved
			require.NoError(t, json.Unmarshal(last.Payload, &payload))
			assert.Equal(t, leaver.ID, payload.UserID)
			assert.Equal(t, leaver.ID, payload.ActorID)

			_, err := f.svc.List(f.ctx, leaver.ID, f.board.ID)
			require.ErrorIs(t, err, domain.ErrNotFound)
			_, err = f.store.Snapshot(f.ctx, f.board.ID, leaver.ID)
			require.ErrorIs(t, err, domain.ErrNotFound)
			require.ErrorIs(t, f.svc.Remove(f.ctx, leaver.ID, f.board.ID, leaver.ID), domain.ErrNotFound, "leaving twice")
			require.ErrorIs(t, f.svc.Remove(f.ctx, leaver.ID, f.board.ID, other.ID), domain.ErrNotFound, "a former member has no rights")
			assert.NotContains(t, f.roles(t), leaver.ID)
			assert.Contains(t, f.roles(t), other.ID)
		})
	}
}

func TestNonOwnersCannotRemoveOthersAndTheErrorDoesNotDependOnTheTarget(t *testing.T) {
	f := newFixture(t)
	editor, viewer, stranger := f.user(t, "editor"), f.user(t, "viewer"), f.user(t, "stranger")
	f.add(t, editor, domain.RoleEditor)
	f.add(t, viewer, domain.RoleViewer)

	for name, actor := range map[string]uuid.UUID{"editor": editor.ID, "viewer": viewer.ID} {
		for targetName, target := range map[string]uuid.UUID{"owner": f.owner.ID, "other member": viewer.ID, "non member": stranger.ID, "unknown id": uuid.New()} {
			if actor == target {
				continue
			}
			err := f.svc.Remove(f.ctx, actor, f.board.ID, target)
			require.ErrorIs(t, err, domain.ErrForbidden, "%s removing %s", name, targetName)
		}
	}
	for targetName, target := range map[string]uuid.UUID{"owner": f.owner.ID, "member": viewer.ID, "unknown id": uuid.New()} {
		err := f.svc.Remove(f.ctx, stranger.ID, f.board.ID, target)
		require.ErrorIs(t, err, domain.ErrNotFound, "a stranger removing %s", targetName)
	}
	assert.Len(t, f.roles(t), 3)
}
