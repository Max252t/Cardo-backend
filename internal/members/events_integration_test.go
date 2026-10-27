package members_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/members"
	"github.com/Max252t/Cardo-backend/internal/ops"
)

func (f *fixture) processor() *ops.Processor {
	return ops.NewProcessor(f.store, ops.WithPublisher(f.pub))
}

func (f *fixture) applyOp(t *testing.T, proc *ops.Processor, user uuid.UUID, opType string, payload any) boardcontent.OpOutcome {
	t.Helper()
	op, err := boardcontent.NewOp(opType, uuid.New(), payload)
	require.NoError(t, err)
	out, err := proc.Apply(f.ctx, user, f.board.ID, op)
	require.NoError(t, err)
	return out
}

func (f *fixture) events(t *testing.T) []domain.Event {
	t.Helper()
	page, err := f.store.EventsAfter(f.ctx, f.board.ID, f.owner.ID, 0, 5000)
	require.NoError(t, err)
	return page.Events
}

func (f *fixture) seq(t *testing.T) int64 {
	t.Helper()
	var seq int64
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT seq FROM boards WHERE id = $1`, f.board.ID).Scan(&seq))
	return seq
}

func (f *fixture) publishedCount() int {
	f.pub.mu.Lock()
	defer f.pub.mu.Unlock()
	return len(f.pub.events)
}

func (f *fixture) add(t *testing.T, u domain.User, role domain.Role) {
	t.Helper()
	_, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, u.Email, role)
	require.NoError(t, err)
}

func (f *fixture) roles(t *testing.T) map[uuid.UUID]domain.Role {
	t.Helper()
	list, err := f.svc.List(f.ctx, f.owner.ID, f.board.ID)
	require.NoError(t, err)
	out := make(map[uuid.UUID]domain.Role, len(list))
	for _, m := range list {
		out[m.UserID] = m.Role
	}
	return out
}

func replayMembers(t *testing.T, events []domain.Event, owner uuid.UUID) map[uuid.UUID]domain.Role {
	t.Helper()
	state := map[uuid.UUID]domain.Role{owner: domain.RoleOwner}
	for _, ev := range events {
		switch ev.Type {
		case boardcontent.EventMemberAdded:
			var p boardcontent.MemberAdded
			require.NoError(t, json.Unmarshal(ev.Payload, &p))
			require.NotContains(t, state, p.UserID, "seq %d adds an existing member", ev.Seq)
			state[p.UserID] = p.Role
		case boardcontent.EventMemberRoleChanged:
			var p boardcontent.MemberRoleChanged
			require.NoError(t, json.Unmarshal(ev.Payload, &p))
			require.Contains(t, state, p.UserID, "seq %d changes a missing member", ev.Seq)
			state[p.UserID] = p.Role
		case boardcontent.EventMemberRemoved:
			var p boardcontent.MemberRemoved
			require.NoError(t, json.Unmarshal(ev.Payload, &p))
			require.Contains(t, state, p.UserID, "seq %d removes a missing member", ev.Seq)
			delete(state, p.UserID)
		}
	}
	return state
}

func requireGapFree(t *testing.T, events []domain.Event) {
	t.Helper()
	for i, ev := range events {
		require.Equal(t, int64(i+1), ev.Seq)
	}
}

func TestEachChangeEmitsOneEventAndSeqStaysGapFreeAmongBoardOps(t *testing.T) {
	f := newFixture(t)
	proc := f.processor()
	bob, carol := f.user(t, "bob"), f.user(t, "carol")
	colID := uuid.New()

	require.Nil(t, f.applyOp(t, proc, f.owner.ID, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: colID, Title: "Todo"}).Nack)
	f.add(t, bob, domain.RoleEditor)
	require.Nil(t, f.applyOp(t, proc, bob.ID, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: colID, Title: "by bob"}).Nack)
	_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, domain.RoleViewer)
	require.NoError(t, err)
	denied := f.applyOp(t, proc, bob.ID, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: colID, Title: "denied"})
	require.NotNil(t, denied.Nack)
	assert.Equal(t, boardcontent.ReasonForbidden, denied.Nack.Reason)
	require.Nil(t, f.applyOp(t, proc, f.owner.ID, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: colID, Title: "by owner"}).Nack)
	require.NoError(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID))
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleEditor})
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, carol.ID, created.Token)
	require.NoError(t, err)
	require.NoError(t, f.svc.Remove(f.ctx, carol.ID, f.board.ID, carol.ID))

	events := f.events(t)
	requireGapFree(t, events)
	types := make([]string, len(events))
	for i, ev := range events {
		types[i] = ev.Type
	}
	assert.Equal(t, []string{
		boardcontent.EventColumnCreated,
		boardcontent.EventMemberAdded,
		boardcontent.EventCardCreated,
		boardcontent.EventMemberRoleChanged,
		boardcontent.EventCardCreated,
		boardcontent.EventMemberRemoved,
		boardcontent.EventMemberAdded,
		boardcontent.EventMemberRemoved,
	}, types)
	assert.Equal(t, int64(len(events)), f.seq(t))

	f.pub.mu.Lock()
	published := append([]domain.Event(nil), f.pub.events...)
	f.pub.mu.Unlock()
	require.Len(t, published, len(events))
	for i, ev := range published {
		assert.Equal(t, events[i].Seq, ev.Seq)
		assert.Equal(t, events[i].Type, ev.Type)
		assert.JSONEq(t, string(events[i].Payload), string(ev.Payload))
	}
}

func TestNoOpAndRejectedCallsEmitNothing(t *testing.T) {
	f := newFixture(t)
	bob, stranger := f.user(t, "bob"), f.user(t, "stranger")
	f.add(t, bob, domain.RoleEditor)
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 5})
	require.NoError(t, err)
	seq, published := f.seq(t), f.publishedCount()

	_, err = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, domain.RoleEditor)
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, bob.ID, created.Token)
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, f.owner.ID, created.Token)
	require.NoError(t, err)

	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, bob.Email, domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrAlreadyMember)
	_, err = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, "ghost@example.com", domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.svc.Add(f.ctx, bob.ID, f.board.ID, stranger.Email, domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrForbidden)
	_, err = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, f.owner.ID, domain.RoleViewer)
	require.Error(t, err)
	_, err = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, stranger.ID, domain.RoleViewer)
	require.ErrorIs(t, err, domain.ErrNotFound)
	require.ErrorIs(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, f.owner.ID), domain.ErrForbidden)
	require.ErrorIs(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, stranger.ID), domain.ErrNotFound)
	require.ErrorIs(t, f.svc.Remove(f.ctx, bob.ID, f.board.ID, f.owner.ID), domain.ErrForbidden)
	_, err = f.svc.Accept(f.ctx, stranger.ID, "unknown-token")
	require.ErrorIs(t, err, domain.ErrNotFound)

	assert.Equal(t, seq, f.seq(t), "no-ops and rejections must not bump the board seq")
	assert.Equal(t, published, f.publishedCount(), "nothing is published for them either")
	var uses int
	require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT uses FROM invites WHERE id = $1`, created.ID).Scan(&uses))
	assert.Zero(t, uses, "existing members never consume a use")
}

func TestMemberEventPayloadShapes(t *testing.T) {
	f := newFixture(t)
	bob, carol := f.user(t, "bob"), f.user(t, "carol")
	f.add(t, bob, domain.RoleViewer)
	_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, domain.RoleEditor)
	require.NoError(t, err)
	created, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	_, err = f.svc.Accept(f.ctx, carol.ID, created.Token)
	require.NoError(t, err)
	require.NoError(t, f.svc.Remove(f.ctx, carol.ID, f.board.ID, carol.ID))
	require.NoError(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID))

	events := f.events(t)
	require.Len(t, events, 5)
	want := []struct {
		eventType string
		actor     uuid.UUID
		payload   string
	}{
		{boardcontent.EventMemberAdded, f.owner.ID, fmt.Sprintf(`{"user_id":%q,"display_name":%q,"avatar_color":%q,"role":"viewer","actor_id":%q}`, bob.ID, bob.DisplayName, bob.AvatarColor, f.owner.ID)},
		{boardcontent.EventMemberRoleChanged, f.owner.ID, fmt.Sprintf(`{"user_id":%q,"role":"editor","actor_id":%q}`, bob.ID, f.owner.ID)},
		{boardcontent.EventMemberAdded, carol.ID, fmt.Sprintf(`{"user_id":%q,"display_name":%q,"avatar_color":%q,"role":"viewer","actor_id":%q}`, carol.ID, carol.DisplayName, carol.AvatarColor, carol.ID)},
		{boardcontent.EventMemberRemoved, carol.ID, fmt.Sprintf(`{"user_id":%q,"actor_id":%q}`, carol.ID, carol.ID)},
		{boardcontent.EventMemberRemoved, f.owner.ID, fmt.Sprintf(`{"user_id":%q,"actor_id":%q}`, bob.ID, f.owner.ID)},
	}
	for i, w := range want {
		assert.Equal(t, w.eventType, events[i].Type, i)
		assert.JSONEq(t, w.payload, string(events[i].Payload), i)
		require.NotNil(t, events[i].ActorID, i)
		assert.Equal(t, w.actor, *events[i].ActorID, i)
		assert.Nil(t, events[i].OpID, "member events are not tied to client ops")
		assert.NotContains(t, string(events[i].Payload), "email")
		assert.NotContains(t, string(events[i].Payload), "token")
	}
}

func TestSnapshotMembersMatchTheReplayedEventLogAfterMixedSequences(t *testing.T) {
	f := newFixture(t)
	proc := f.processor()
	users := make([]domain.User, 6)
	for i := range users {
		users[i] = f.user(t, fmt.Sprintf("u%d", i))
	}
	colID := uuid.New()
	require.Nil(t, f.applyOp(t, proc, f.owner.ID, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: colID, Title: "Todo"}).Nack)

	invite, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 10})
	require.NoError(t, err)
	steps := []func() error{
		func() error {
			_, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, users[0].Email, domain.RoleEditor)
			return err
		},
		func() error { _, err := f.svc.Accept(f.ctx, users[1].ID, invite.Token); return err },
		func() error { _, err := f.svc.Accept(f.ctx, users[2].ID, invite.Token); return err },
		func() error {
			_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, users[1].ID, domain.RoleEditor)
			return err
		},
		func() error { return f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, users[0].ID) },
		func() error {
			_, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, users[0].Email, domain.RoleViewer)
			return err
		},
		func() error { return f.svc.Remove(f.ctx, users[2].ID, f.board.ID, users[2].ID) },
		func() error { _, err := f.svc.Accept(f.ctx, users[2].ID, invite.Token); return err },
		func() error {
			_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, users[2].ID, domain.RoleEditor)
			return err
		},
		func() error {
			_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, users[0].ID, domain.RoleEditor)
			return err
		},
		func() error { _, err := f.svc.Accept(f.ctx, users[3].ID, invite.Token); return err },
		func() error { return f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, users[3].ID) },
		func() error {
			_, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, users[4].Email, domain.RoleEditor)
			return err
		},
		func() error { return f.svc.RevokeInvite(f.ctx, f.owner.ID, f.board.ID, invite.ID) },
		func() error { _, err := f.svc.Accept(f.ctx, users[5].ID, invite.Token); return err },
	}
	for i, step := range steps {
		err := step()
		if i == len(steps)-1 {
			require.ErrorIs(t, err, domain.ErrInviteUnavailable)
			continue
		}
		require.NoError(t, err, "step %d", i)
		out := f.applyOp(t, proc, f.owner.ID, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: colID, Title: fmt.Sprintf("after step %d", i)})
		require.Nil(t, out.Nack)
	}

	events := f.events(t)
	requireGapFree(t, events)
	replayed := replayMembers(t, events, f.owner.ID)

	snap, err := f.store.Snapshot(f.ctx, f.board.ID, f.owner.ID)
	require.NoError(t, err)
	fromSnapshot := make(map[uuid.UUID]domain.Role, len(snap.Members))
	for _, m := range snap.Members {
		fromSnapshot[m.UserID] = m.Role
	}
	assert.Equal(t, replayed, fromSnapshot)
	assert.Equal(t, replayed, f.roles(t))
	assert.Equal(t, int64(len(events)), snap.Seq)
	assert.Equal(t, map[uuid.UUID]domain.Role{
		f.owner.ID:  domain.RoleOwner,
		users[0].ID: domain.RoleEditor,
		users[1].ID: domain.RoleEditor,
		users[2].ID: domain.RoleEditor,
		users[4].ID: domain.RoleEditor,
	}, fromSnapshot)
}

func TestPublishedEventsAreCommittedAndOrderedBySeq(t *testing.T) {
	f := newFixture(t)
	var checks int
	var mu sync.Mutex
	svc := members.NewService(f.store, members.WithClock(f.clock), members.WithPublisher(publisherFunc(func(ctx context.Context, events []domain.Event) error {
		require.Len(t, events, 1, "one event per change")
		var found int
		require.NoError(t, f.store.Pool().QueryRow(ctx, `SELECT count(*) FROM board_events WHERE board_id = $1 AND seq = $2 AND type = $3`,
			events[0].BoardID, events[0].Seq, events[0].Type).Scan(&found))
		assert.Equal(t, 1, found, "an event is visible in the log before it is published")
		mu.Lock()
		checks++
		mu.Unlock()
		return nil
	})))
	bob := f.user(t, "bob")
	_, err := svc.Add(f.ctx, f.owner.ID, f.board.ID, bob.Email, domain.RoleViewer)
	require.NoError(t, err)
	_, err = svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, domain.RoleEditor)
	require.NoError(t, err)
	require.NoError(t, svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID))
	assert.Equal(t, 3, checks)
}

type publisherFunc func(ctx context.Context, events []domain.Event) error

func (p publisherFunc) Publish(ctx context.Context, events []domain.Event) error {
	return p(ctx, events)
}

func TestFailedPublishDoesNotFailTheChange(t *testing.T) {
	f := newFixture(t)
	svc := members.NewService(f.store, members.WithPublisher(publisherFunc(func(context.Context, []domain.Event) error {
		return fmt.Errorf("redis down")
	})))
	bob := f.user(t, "bob")
	m, err := svc.Add(f.ctx, f.owner.ID, f.board.ID, bob.Email, domain.RoleViewer)
	require.NoError(t, err)
	assert.Equal(t, bob.ID, m.UserID)
	assert.Equal(t, domain.RoleViewer, f.roles(t)[bob.ID])
}

func TestRoleChangeRacingAnOpDecidesByTheDatabase(t *testing.T) {
	f := newFixture(t)
	proc := f.processor()
	bob := f.user(t, "bob")
	f.add(t, bob, domain.RoleEditor)
	colID := uuid.New()
	require.Nil(t, f.applyOp(t, proc, f.owner.ID, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: colID, Title: "Todo"}).Nack)

	const racers = 40
	start := make(chan struct{})
	var wg sync.WaitGroup
	outcomes := make([]boardcontent.OpOutcome, racers)
	errs := make([]error, racers)
	prepared := make([]boardcontent.Op, racers)
	for i := range prepared {
		op, err := boardcontent.NewOp(boardcontent.OpCardCreate, uuid.New(), boardcontent.CardCreate{CardID: uuid.New(), ColumnID: colID, Title: fmt.Sprintf("c%d", i)})
		require.NoError(t, err)
		prepared[i] = op
	}
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			outcomes[i], errs[i] = proc.Apply(f.ctx, bob.ID, f.board.ID, prepared[i])
		}()
	}
	var changeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, changeErr = f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, domain.RoleViewer)
	}()
	close(start)
	wg.Wait()
	require.NoError(t, changeErr)

	events := f.events(t)
	requireGapFree(t, events)
	var changedAt int64
	bobCards := 0
	for _, ev := range events {
		switch ev.Type {
		case boardcontent.EventMemberRoleChanged:
			changedAt = ev.Seq
		case boardcontent.EventCardCreated:
			bobCards++
			assert.Zero(t, changedAt, "no card by the downgraded user may land after the role change (seq %d)", ev.Seq)
		}
	}
	require.NotZero(t, changedAt)

	succeeded := 0
	for i := range racers {
		require.NoError(t, errs[i])
		if outcomes[i].Event != nil {
			succeeded++
			assert.Less(t, outcomes[i].Event.Seq, changedAt)
			continue
		}
		require.NotNil(t, outcomes[i].Nack)
		assert.Equal(t, boardcontent.ReasonForbidden, outcomes[i].Nack.Reason)
	}
	assert.Equal(t, succeeded, bobCards)

	after := f.applyOp(t, proc, bob.ID, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: colID, Title: "late"})
	require.NotNil(t, after.Nack)
	assert.Equal(t, boardcontent.ReasonForbidden, after.Nack.Reason)
}

func TestRemovalRacingAnOpLeavesNoOpsAfterTheRemoval(t *testing.T) {
	f := newFixture(t)
	proc := f.processor()
	bob := f.user(t, "bob")
	f.add(t, bob, domain.RoleEditor)
	colID := uuid.New()
	require.Nil(t, f.applyOp(t, proc, f.owner.ID, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: colID, Title: "Todo"}).Nack)

	const racers = 30
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range racers {
		op, err := boardcontent.NewOp(boardcontent.OpCardCreate, uuid.New(), boardcontent.CardCreate{CardID: uuid.New(), ColumnID: colID, Title: fmt.Sprintf("c%d", i)})
		require.NoError(t, err)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _ = proc.Apply(f.ctx, bob.ID, f.board.ID, op)
		}()
	}
	var removeErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		removeErr = f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID)
	}()
	close(start)
	wg.Wait()
	require.NoError(t, removeErr)

	events := f.events(t)
	requireGapFree(t, events)
	removed := false
	for _, ev := range events {
		if ev.Type == boardcontent.EventMemberRemoved {
			removed = true
			continue
		}
		if ev.Type == boardcontent.EventCardCreated {
			assert.False(t, removed, "a removed member's op must not commit after the removal")
		}
	}
	require.True(t, removed)
	var nacks int
	require.NoError(t, f.store.Pool().QueryRow(f.ctx,
		`SELECT count(*) FROM processed_ops WHERE board_id = $1 AND user_id = $2 AND outcome->>'kind' = 'nack'`, f.board.ID, bob.ID).Scan(&nacks))
	assert.Zero(t, nacks, "forbidden nacks from non members are not persisted")
}

func TestRemovalRacingAnAcceptKeepsTheLogAndTheRosterConsistent(t *testing.T) {
	for round := range 6 {
		t.Run(fmt.Sprintf("round %d", round), func(t *testing.T) {
			f := newFixture(t)
			bob := f.user(t, "bob")
			invite, err := f.svc.CreateInvite(f.ctx, f.owner.ID, f.board.ID, members.InviteInput{Role: domain.RoleViewer, MaxUses: 5})
			require.NoError(t, err)
			_, err = f.svc.Accept(f.ctx, bob.ID, invite.Token)
			require.NoError(t, err)

			start := make(chan struct{})
			var wg sync.WaitGroup
			var removeErr, acceptErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				removeErr = f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, acceptErr = f.svc.Accept(f.ctx, bob.ID, invite.Token)
			}()
			close(start)
			wg.Wait()

			require.NoError(t, acceptErr)
			require.NoError(t, removeErr)
			events := f.events(t)
			requireGapFree(t, events)
			replayed := replayMembers(t, events, f.owner.ID)
			assert.Equal(t, replayed, f.roles(t))

			var uses int
			require.NoError(t, f.store.Pool().QueryRow(f.ctx, `SELECT uses FROM invites WHERE id = $1`, invite.ID).Scan(&uses))
			adds := 0
			for _, ev := range events {
				if ev.Type == boardcontent.EventMemberAdded {
					adds++
				}
			}
			assert.Equal(t, adds, uses, "every member.added from an invite consumed exactly one use")
			_, stillMember := f.roles(t)[bob.ID]
			assert.Equal(t, adds == 2, stillMember, "bob is a member only if the accept ran after the removal")
		})
	}
}

func TestDoubleRemoveRemovesOnce(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	f.add(t, bob, domain.RoleEditor)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if i%2 == 0 {
				errs[i] = f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, bob.ID)
			} else {
				errs[i] = f.svc.Remove(f.ctx, bob.ID, f.board.ID, bob.ID)
			}
		}()
	}
	close(start)
	wg.Wait()

	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		assert.Contains(t, []error{domain.ErrNotFound, domain.ErrForbidden}, err)
	}
	assert.Equal(t, 1, ok)
	removed := 0
	for _, ev := range f.events(t) {
		if ev.Type == boardcontent.EventMemberRemoved {
			removed++
		}
	}
	assert.Equal(t, 1, removed)
	assert.Equal(t, 1, f.publishedCountOf(boardcontent.EventMemberRemoved))
}

func (f *fixture) publishedCountOf(eventType string) int {
	f.pub.mu.Lock()
	defer f.pub.mu.Unlock()
	n := 0
	for _, ev := range f.pub.events {
		if ev.Type == eventType {
			n++
		}
	}
	return n
}

func TestConcurrentAddOfTheSameEmailAddsOnce(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = f.svc.Add(f.ctx, f.owner.ID, f.board.ID, bob.Email, domain.RoleViewer)
		}()
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
		} else {
			assert.ErrorIs(t, err, domain.ErrAlreadyMember)
		}
	}
	assert.Equal(t, 1, ok)
	assert.Len(t, f.events(t), 1)
}

func TestConcurrentRoleChangesEndWithTheLastLoggedRole(t *testing.T) {
	f := newFixture(t)
	bob := f.user(t, "bob")
	f.add(t, bob, domain.RoleViewer)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range 12 {
		role := domain.RoleEditor
		if i%2 == 0 {
			role = domain.RoleViewer
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.ChangeRole(f.ctx, f.owner.ID, f.board.ID, bob.ID, role)
			assert.NoError(t, err)
		}()
	}
	close(start)
	wg.Wait()

	events := f.events(t)
	requireGapFree(t, events)
	assert.Equal(t, replayMembers(t, events, f.owner.ID)[bob.ID], f.roles(t)[bob.ID])
	for i := 1; i < len(events); i++ {
		var prev, cur boardcontent.MemberRoleChanged
		if events[i-1].Type != boardcontent.EventMemberRoleChanged {
			continue
		}
		require.NoError(t, json.Unmarshal(events[i-1].Payload, &prev))
		require.NoError(t, json.Unmarshal(events[i].Payload, &cur))
		assert.NotEqual(t, prev.Role, cur.Role, "a change event is only logged when the role actually changes")
	}
}

func (f *fixture) eventTypesBySeq(t *testing.T) []domain.Event {
	t.Helper()
	rows, err := f.store.Pool().Query(f.ctx, `SELECT seq, type FROM board_events WHERE board_id = $1 ORDER BY seq`, f.board.ID)
	require.NoError(t, err)
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var ev domain.Event
		require.NoError(t, rows.Scan(&ev.Seq, &ev.Type))
		out = append(out, ev)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestMemberChangesRacingBoardDeletionNeverFollowTheDeleteEvent(t *testing.T) {
	f := newFixture(t)
	boardSvc := boards.NewService(f.store)
	users := make([]domain.User, 10)
	for i := range users {
		users[i] = f.user(t, fmt.Sprintf("u%d", i))
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, u.Email, domain.RoleViewer)
			if err != nil {
				assert.ErrorIs(t, err, domain.ErrNotFound)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, err := boardSvc.Delete(f.ctx, f.owner.ID, f.board.ID)
		assert.NoError(t, err)
	}()
	close(start)
	wg.Wait()

	events := f.eventTypesBySeq(t)
	deletedAt := int64(0)
	for i, ev := range events {
		assert.Equal(t, int64(i+1), ev.Seq)
		if ev.Type == domain.EventBoardDeleted {
			deletedAt = ev.Seq
		} else if deletedAt != 0 {
			t.Errorf("event %s at seq %d follows board.deleted at %d", ev.Type, ev.Seq, deletedAt)
		}
	}
	require.NotZero(t, deletedAt)

	_, err := f.svc.Add(f.ctx, f.owner.ID, f.board.ID, users[0].Email, domain.RoleViewer)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, f.svc.Remove(f.ctx, f.owner.ID, f.board.ID, users[0].ID), domain.ErrNotFound)
}
