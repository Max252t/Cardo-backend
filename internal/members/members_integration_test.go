package members_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/members"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type recordingPublisher struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *recordingPublisher) Publish(_ context.Context, events []domain.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, events...)
	return nil
}

func (r *recordingPublisher) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.events))
	for _, ev := range r.events {
		out = append(out, ev.Type)
	}
	return out
}

type fixture struct {
	store *storage.Store
	svc   *members.Service
	clock *fakeClock
	pub   *recordingPublisher
	owner domain.User
	board domain.BoardWithRole
	ctx   context.Context
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := testutil.NewStore(t)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	boardID, err := uuid.NewV7()
	require.NoError(t, err)
	board, err := store.CreateBoardWithOwner(ctx, domain.Board{ID: boardID, Title: "Roadmap", OwnerID: owner.ID})
	require.NoError(t, err)
	clock := &fakeClock{now: time.Now()}
	pub := &recordingPublisher{}
	return &fixture{
		store: store,
		svc:   members.NewService(store, members.WithClock(clock), members.WithPublisher(pub)),
		clock: clock,
		pub:   pub,
		owner: owner,
		board: board,
		ctx:   ctx,
	}
}

func (f *fixture) user(t *testing.T, prefix string) domain.User {
	t.Helper()
	return testutil.CreateUser(t, f.store, testutil.UniqueEmail(prefix))
}

func (f *fixture) eventTypes(t *testing.T) []string {
	t.Helper()
	page, err := f.store.EventsAfter(f.ctx, f.board.ID, f.owner.ID, 0, 100)
	require.NoError(t, err)
	out := make([]string, 0, len(page.Events))
	for _, ev := range page.Events {
		out = append(out, ev.Type)
	}
	return out
}

func TestOwnerAddsChangesAndRemovesMembersWithEvents(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")

	m, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, bob.Email, domain.RoleViewer)
	require.NoError(t, err)
	assert.Equal(t, bob.ID, m.UserID)
	assert.Equal(t, domain.RoleViewer, m.Role)
	assert.Equal(t, bob.DisplayName, m.DisplayName)

	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, bob.Email, domain.RoleEditor)
	require.ErrorIs(t, err, domain.ErrAlreadyMember)
	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, f.owner.Email, domain.RoleEditor)
	require.ErrorIs(t, err, domain.ErrAlreadyMember)
	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, "nobody@example.com", domain.RoleEditor)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, bob.Email, domain.RoleOwner)
	var vErr *domain.ValidationError
	require.ErrorAs(t, err, &vErr)

	m, err = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, domain.RoleEditor)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleEditor, m.Role)
	m, err = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, domain.RoleEditor)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleEditor, m.Role)

	_, err = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, f.owner.ID, domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, uuid.New(), domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrNotFound)

	require.NoError(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID))
	require.ErrorIs(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID), domain.ErrNotFound)

	want := []string{boardcontent.EventMemberAdded, boardcontent.EventMemberRoleChanged, boardcontent.EventMemberRemoved}
	assert.Equal(t, want, f.eventTypes(t))
	assert.Equal(t, want, f.pub.types())

	snap, err := f.store.Snapshot(f.ctx, f.board.ID, f.owner.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), snap.Seq)
	require.Len(t, snap.Members, 1)
	assert.Equal(t, f.owner.ID, snap.Members[0].UserID)
}

func TestMembershipRulesForNonOwners(t *testing.T) {
	f := newFixture(t)
	editor, viewer, stranger := f.user(t, "editor"), f.user(t, "viewer"), f.user(t, "stranger")
	_, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, editor.Email, domain.RoleEditor)
	require.NoError(t, err)
	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, viewer.Email, domain.RoleViewer)
	require.NoError(t, err)

	_, err = f.svc.Add(f.ctx, editor.ID, f.board.ID, stranger.Email, domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.svc.Add(f.ctx, editor.ID, f.board.ID, "unknown@example.com", domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrForbidden, "non owners must not learn whether an email is registered")
	_, err = f.svc.ChangeRole(f.ctx, editor.ID, f.board.ID, viewer.ID, domain.RoleEditor)
	require.ErrorIs(t, err, domain.ErrForbidden)
	require.ErrorIs(t, f.svc.Remove(f.ctx, editor.ID, f.board.ID, viewer.ID), domain.ErrForbidden)
	require.ErrorIs(t, f.svc.Remove(f.ctx, stranger.ID, f.board.ID, viewer.ID), domain.ErrNotFound)
	require.ErrorIs(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, f.owner.ID), domain.ErrForbidden, "the owner cannot leave")
	require.ErrorIs(t, f.svc.Remove(f.ctx, editor.ID, f.board.ID, f.owner.ID), domain.ErrForbidden)

	list, err := f.svc.List(f.ctx, viewer.ID, f.board.ID)
	require.NoError(t, err)
	assert.Len(t, list, 3)
	_, err = f.svc.List(f.ctx, stranger.ID, f.board.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)

	require.NoError(t, f.svc.Remove(f.ctx, editor.ID, f.board.ID, editor.ID), "a member can leave")
	list, err = f.svc.List(f.ctx, viewer.ID, f.board.ID)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestInviteLifecycle(t *testing.T) {
	f := newFixture(t)
	ann, bob, carol := f.user(t, "ann"), f.user(t, "bob"), f.user(t, "carol")

	_, err := f.svc.CreateInvite(f.ctx, ann.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.ErrorIs(t, err, domain.ErrNotFound)

	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleEditor})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(created.Token), 43)
	assert.Equal(t, 1, created.MaxUses)
	assert.WithinDuration(t, f.clock.Now().Add(members.DefaultInviteTTL), created.ExpiresAt, time.Second)
	assert.NotEqual(t, created.Token, created.TokenHash)

	var stored string
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT token_hash FROM invites WHERE id = $1`, created.ID).Scan(&stored))
	assert.Equal(t, members.HashToken(created.Token), stored)

	joined, err := f.svc.Accept(f.ctx, ann.ID, created.Token)
	require.NoError(t, err)
	assert.Equal(t, f.board.ID, joined.ID)
	assert.Equal(t, domain.RoleEditor, joined.Role)

	again, err := f.svc.Accept(f.ctx, ann.ID, created.Token)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleEditor, again.Role, "an existing member gets an idempotent success")

	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable, "the single use was consumed by the first member")

	_, err = f.svc.Accept(f.ctx, carol.ID, "not-a-real-token")
	require.ErrorIs(t, err, domain.ErrNotFound)

	assert.Equal(t, []string{boardcontent.EventMemberAdded}, f.eventTypes(t))
}

func TestInviteRevokeExpiryAndMultipleUses(t *testing.T) {
	f := newFixture(t)
	ann, bob, carol, dave := f.user(t, "ann"), f.user(t, "bob"), f.user(t, "carol"), f.user(t, "dave")

	multi, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 2, ExpiresIn: 3600})
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, ann.ID, multi.Token)
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, bob.ID, multi.Token)
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, carol.ID, multi.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable)

	expiring, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, ExpiresIn: 3600})
	require.NoError(t, err)
	f.clock.Advance(2 * time.Hour)
	_, err = f.svc.Accept(f.ctx, carol.ID, expiring.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable)

	revoked, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	active, err := f.svc.ListInvites(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, revoked.ID, active[0].ID)
	require.NoError(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, revoked.ID))
	require.NoError(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, revoked.ID))
	require.ErrorIs(t, f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, uuid.New()), domain.ErrNotFound)
	require.ErrorIs(t, f.svc.RevokeInvite(f.ctx, ann.ID, f.board.ID, revoked.ID), domain.ErrForbidden)
	_, err = f.svc.Accept(f.ctx, dave.ID, revoked.Token)
	require.ErrorIs(t, err, domain.ErrInviteUnavailable)

	active, err = f.svc.ListInvites(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	assert.Empty(t, active)
}

func TestInviteInputLimits(t *testing.T) {
	f := newFixture(t)
	var vErr *domain.ValidationError
	for _, in := range []members.InviteInput{
		{Role: domain.RoleOwner},
		{Role: domain.Role("admin")},
		{Role: domain.RoleViewer, ExpiresIn: members.MaxInviteTTLSeconds + 1},
		{Role: domain.RoleViewer, ExpiresIn: -1},
		{Role: domain.RoleViewer, ExpiresIn: 9223372036854775807},
		{Role: domain.RoleViewer, ExpiresIn: -9223372036854775808},
		{Role: domain.RoleViewer, MaxUses: members.MaxInviteMaxUses + 1},
		{Role: domain.RoleViewer, MaxUses: -1},
	} {
		_, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, in)
		require.ErrorAs(t, err, &vErr, "%+v", in)
	}
}

func TestConcurrentAcceptsNeverExceedMaxUses(t *testing.T) {
	f := newFixture(t)
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 3})
	require.NoError(t, err)
	users := make([]domain.User, 8)
	for i := range users {
		users[i] = f.user(t, "racer")
	}

	var wg sync.WaitGroup
	results := make([]error, len(users))
	for i, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = f.svc.Accept(f.ctx, u.ID, created.Token)
		}()
	}
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, domain.ErrInviteUnavailable)
		}
	}
	assert.Equal(t, 3, succeeded)
	list, err := f.svc.List(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	assert.Len(t, list, 4)
}
