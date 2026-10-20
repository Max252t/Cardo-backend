package realtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

type multiBoard struct {
	boards map[uuid.UUID]*rttest.Board
}

func (m multiBoard) find(boardID uuid.UUID) (*rttest.Board, error) {
	b, ok := m.boards[boardID]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return b, nil
}

func (m multiBoard) BoardMember(ctx context.Context, boardID, userID uuid.UUID) (domain.Member, error) {
	b, err := m.find(boardID)
	if err != nil {
		return domain.Member{}, err
	}
	return b.BoardMember(ctx, boardID, userID)
}

func (m multiBoard) Apply(ctx context.Context, userID, boardID uuid.UUID, op protocol.Op) (ops.Outcome, error) {
	b, err := m.find(boardID)
	if err != nil {
		return ops.Outcome{}, err
	}
	return b.Apply(ctx, userID, boardID, op)
}

func (m multiBoard) Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error) {
	b, err := m.find(boardID)
	if err != nil {
		return domain.BoardSnapshot{}, err
	}
	return b.Snapshot(ctx, userID, boardID)
}

func (m multiBoard) Events(ctx context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error) {
	b, err := m.find(boardID)
	if err != nil {
		return domain.EventPage{}, err
	}
	return b.Events(ctx, userID, boardID, afterSeq)
}

func addBoard(t testing.TB, env *rttest.Env, mb multiBoard, users ...uuid.UUID) *rttest.Board {
	t.Helper()
	b := rttest.NewBoard(uuid.New(), env.NewPublisher(t))
	for _, u := range users {
		b.AddMember(u, "User", domain.RoleEditor)
	}
	mb.boards[b.ID] = b
	return b
}

func TestFanOutDeliversEachEventOnceToEveryClientIncludingTheInitiator(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleViewer)
	cat := rttest.AddUser(env, "Cat", domain.RoleViewer)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")

	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()
	bobConn := b.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	catConn := b.Dial(t, cat)
	catConn.HandshakeSnapshot()

	var ids []uuid.UUID
	for range 3 {
		ids = append(ids, sendOp(annConn, protocol.OpCardCreate))
	}
	for name, c := range map[string]*rttest.Conn{"initiator": annConn, "remote a": bobConn, "remote b": catConn} {
		seqs := readEventSeqs(t, c, "card.create", 3)
		assert.Equal(t, []int64{1, 2, 3}, seqs, name)
	}
	assert.Len(t, ids, 3)
	assert.Equal(t, int64(3), env.Board.Seq())
}

func TestInitiatorReceivesItsOwnEventWithTheOpID(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()
	other := b.Dial(t, ann)
	other.HandshakeSnapshot()

	opID := sendOp(annConn, protocol.OpCardCreate)
	for _, c := range []*rttest.Conn{annConn, other} {
		ev := awaitOwnEvent(c, opID)
		assert.Equal(t, int64(1), ev.Seq)
		assert.Equal(t, "card.create", ev.Type)
		assert.NotZero(t, ev.ServerTS)
	}
}

func TestChannelsAreSubscribedOnFirstJoinAndReleasedOnLastLeavePerInstance(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	board := env.Board.ID
	waitSubscribed(t, a, board, false)
	waitSubscribed(t, b, board, false)

	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()
	waitSubscribed(t, a, board, true)
	waitSubscribed(t, b, board, false)

	bobConn := b.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	waitSubscribed(t, b, board, true)

	require.NoError(t, annConn.Close())
	waitConnections(t, a, board, 0)
	waitSubscribed(t, a, board, false)
	waitSubscribed(t, b, board, true)

	opID := sendOp(bobConn, protocol.OpCardCreate)
	assert.Equal(t, int64(1), awaitOwnEvent(bobConn, opID).Seq)

	again := a.Dial(t, ann)
	again.Hello(int64Ptr(0))
	delta := decodeAs[protocol.SyncDelta](t, again.Expect(protocol.TypeSyncDelta))
	require.Len(t, delta.Events, 1)
	waitSubscribed(t, a, board, true)
}

func TestInstanceThatNoLongerHostsTheBoardReceivesNothing(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	idle := env.StartInstance(t, "idle")
	c := a.Dial(t, ann)
	c.HandshakeSnapshot()

	appendEvents(t, env, ann, 3)
	readEventSeqs(t, c, "card.created", 3)
	assert.False(t, idle.Broker.Subscribed(protocol.BoardChannel(env.Board.ID)))
	assert.Zero(t, idle.Hub.ConnectionCount(env.Board.ID))
}

func TestBoardsAreIsolatedFromEachOther(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	mb := multiBoard{boards: map[uuid.UUID]*rttest.Board{env.Board.ID: env.Board}}
	second := addBoard(t, env, mb, ann)
	env.Ops, env.Sync, env.Members = mb, mb, mb
	a := env.StartInstance(t, "a")

	first := env.DialBoard(t, a, env.Board.ID, ann)
	first.HandshakeSnapshot()
	other := env.DialBoard(t, a, second.ID, ann)
	other.HandshakeSnapshot()
	waitSubscribed(t, a, env.Board.ID, true)
	waitSubscribed(t, a, second.ID, true)

	_, err := second.Append(context.Background(), protocol.EventCardCreated, map[string]string{}, ann)
	require.NoError(t, err)
	_, err = env.Board.Append(context.Background(), protocol.EventCardArchived, map[string]string{}, ann)
	require.NoError(t, err)
	assert.Equal(t, protocol.EventCardCreated, other.MustNext().Type)
	assert.Equal(t, protocol.EventCardArchived, first.MustNext().Type)

	appendOn := func(b *rttest.Board, n int) {
		for range n {
			_, err := b.Append(context.Background(), protocol.EventCardMoved, map[string]string{}, ann)
			require.NoError(t, err)
		}
	}
	appendOn(env.Board, 2)
	appendOn(second, 1)
	assert.Equal(t, []int64{2, 3}, readEventSeqs(t, first, protocol.EventCardMoved, 3))
	assert.Equal(t, []int64{2}, readEventSeqs(t, other, protocol.EventCardMoved, 2))

	require.NoError(t, other.Close())
	waitConnections(t, a, second.ID, 0)
	waitSubscribed(t, a, second.ID, false)
	waitSubscribed(t, a, env.Board.ID, true)
}

func TestBoardDeletionClosesOnlyThatBoardsConnections(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleOwner)
	mb := multiBoard{boards: map[uuid.UUID]*rttest.Board{env.Board.ID: env.Board}}
	second := addBoard(t, env, mb, ann)
	env.Ops, env.Sync, env.Members = mb, mb, mb
	a := env.StartInstance(t, "a")
	first := env.DialBoard(t, a, env.Board.ID, ann)
	first.HandshakeSnapshot()
	other := env.DialBoard(t, a, second.ID, ann)
	other.HandshakeSnapshot()

	_, err := second.Delete(context.Background(), ann)
	require.NoError(t, err)
	assert.Equal(t, protocol.CloseBoardDeleted, other.ExpectClose().Code)
	waitConnections(t, a, second.ID, 0)
	assert.Equal(t, 1, a.Hub.ConnectionCount(env.Board.ID))

	opID := sendOp(first, protocol.OpCardCreate)
	assert.Equal(t, int64(1), awaitOwnEvent(first, opID).Seq)
}

func dropSeq(channelKind string, seqs ...int64) func(pubsub.Message) bool {
	drop := make(map[int64]bool, len(seqs))
	for _, s := range seqs {
		drop[s] = true
	}
	return func(m pubsub.Message) bool {
		kind, _ := protocol.ParseChannel(m.Channel)
		if (channelKind == "board") != (kind == protocol.ChannelBoard) {
			return false
		}
		var env protocol.Envelope
		if json.Unmarshal(m.Payload, &env) != nil {
			return false
		}
		return drop[env.Seq]
	}
}

func TestDroppedEventLeavesAGapTheClientCanSeeAndRecoverFromByReconnecting(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleViewer)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()
	bobConn := b.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	annConn.Expect(protocol.TypePresenceJoin)

	env.Network.SetDropFilter(dropSeq("board", 2))
	appendEvents(t, env, ann, 3)
	for name, c := range map[string]*rttest.Conn{"local": annConn, "remote": bobConn} {
		assert.Equal(t, []int64{1, 3}, readEventSeqs(t, c, "card.created", 3), name)
	}

	env.Network.SetDropFilter(nil)
	page, err := env.Board.Events(context.Background(), bob, env.Board.ID, 1)
	require.NoError(t, err)
	require.Len(t, page.Events, 2)
	assert.Equal(t, int64(2), page.Events[0].Seq)
	assert.Equal(t, int64(3), page.Events[1].Seq)

	require.NoError(t, bobConn.Close())
	waitConnections(t, b, env.Board.ID, 0)
	again := b.Dial(t, bob)
	again.Hello(int64Ptr(1))
	delta := decodeAs[protocol.SyncDelta](t, again.Expect(protocol.TypeSyncDelta))
	var seqs []int64
	for _, ev := range delta.Events {
		seqs = append(seqs, ev.Seq)
	}
	assert.Equal(t, []int64{2, 3}, seqs)
	again.Expect(protocol.TypePresenceList)
	again.Expect(protocol.TypeSyncReady)

	appendEvents(t, env, ann, 1)
	assert.Equal(t, []int64{4}, readEventSeqs(t, again, "card.created", 4))
}

func TestDroppedMessagesNeverDuplicateOrReorderTheRest(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	c := a.Dial(t, ann)
	c.HandshakeSnapshot()

	env.Network.SetDropFilter(dropSeq("board", 2, 3, 5, 8))
	appendEvents(t, env, ann, 10)
	assert.Equal(t, []int64{1, 4, 6, 7, 9, 10}, readEventSeqs(t, c, "card.created", 10))
}

func TestInitiatorRecoversALostConfirmationByResendingTheOp(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	env.Network.SetDropFilter(dropSeq("board", 1))
	opID := uuid.New()
	c.SendJSON(protocol.OpCardCreate, &opID, map[string]any{"card_id": uuid.NewString()})
	require.Eventually(t, func() bool { return env.Board.ApplyCount() == 1 }, eventually, 5*time.Millisecond)
	env.Network.SetDropFilter(nil)

	c.SendJSON(protocol.OpCardCreate, &opID, map[string]any{"card_id": uuid.NewString()})
	ev := awaitOwnEvent(c, opID)
	assert.Equal(t, int64(1), ev.Seq)
	assert.Equal(t, 2, env.Board.ApplyCount())
	assert.Equal(t, int64(1), env.Board.Seq(), "the resend does not create a second event")
}

func TestDroppedPresenceMessageIsHealedByThePresenceList(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()

	env.Network.SetDropFilter(func(m pubsub.Message) bool {
		kind, _ := protocol.ParseChannel(m.Channel)
		return kind == protocol.ChannelPresence
	})
	bobConn := b.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	env.Network.SetDropFilter(nil)

	late := a.Dial(t, ann)
	late.Hello(nil)
	late.Expect(protocol.TypeSyncSnapshot)
	names := make([]string, 0)
	for _, u := range presenceList(t, late.Expect(protocol.TypePresenceList)) {
		names = append(names, u.Name)
	}
	assert.Equal(t, []string{"Ann", "Bob"}, names)
	late.Expect(protocol.TypeSyncReady)

	opID := sendOp(bobConn, protocol.OpCardCreate)
	for {
		msg := annConn.MustNext()
		require.NotEqual(t, protocol.TypePresenceJoin, msg.Type, "the dropped join is never delivered later")
		if msg.OpID != nil && *msg.OpID == opID {
			break
		}
	}
}

func TestMalformedAndUnknownBrokerMessagesAreIgnored(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	rogue := env.Network.NewBroker()
	t.Cleanup(func() { _ = rogue.Close() })

	ctx := context.Background()
	require.NoError(t, rogue.Publish(ctx, protocol.BoardChannel(env.Board.ID), []byte("not json")))
	require.NoError(t, rogue.Publish(ctx, protocol.PresenceChannel(env.Board.ID), []byte("{")))
	require.NoError(t, rogue.Publish(ctx, protocol.BoardChannel(env.Board.ID), []byte(`{"v":1,"type":"card.created","seq":"x"}`)))

	appendEvents(t, env, ann, 1)
	assert.Equal(t, []int64{1}, readEventSeqs(t, c, "card.created", 1))
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
}

func TestHubDispatchIgnoresUnknownChannelsAndBoardsWithoutClients(t *testing.T) {
	t.Parallel()
	_, _, inst := newEditorEnv(t, nil)
	assert.NotPanics(t, func() {
		inst.Hub.Dispatch("mystery:channel", []byte(`{}`))
		inst.Hub.Dispatch(protocol.BoardChannel(uuid.New()), []byte(`{"v":1,"type":"card.created","seq":1}`))
		inst.Hub.Dispatch(protocol.PresenceChannel(uuid.New()), []byte(`{"v":1,"type":"presence.join","payload":{}}`))
		inst.Hub.Dispatch("", nil)
	})
}
