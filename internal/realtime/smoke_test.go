package realtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func int64Ptr(v int64) *int64 {
	return &v
}

func sendOp(c *rttest.Conn, opType string) uuid.UUID {
	opID := uuid.New()
	c.SendJSON(opType, &opID, map[string]any{"card_id": uuid.New().String()})
	return opID
}

func presenceList(t *testing.T, env protocol.Envelope) []protocol.PresenceUser {
	t.Helper()
	var list protocol.PresenceList
	require.NoError(t, json.Unmarshal(env.Payload, &list))
	return list.Users
}

func TestHandshakeSnapshotWhenLastSeqIsNull(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	_, err := env.Board.Append(context.Background(), protocol.EventCardCreated, map[string]string{"k": "v"}, userID)
	require.NoError(t, err)

	c := inst.Dial(t, userID)
	c.Hello(nil)
	snap := c.Expect(protocol.TypeSyncSnapshot)
	var body protocol.Snapshot
	require.NoError(t, json.Unmarshal(snap.Payload, &body))
	assert.Equal(t, int64(1), body.Seq)
	assert.Equal(t, domain.RoleEditor, body.Board.Role)

	users := presenceList(t, c.Expect(protocol.TypePresenceList))
	require.Len(t, users, 1)
	assert.Equal(t, userID, users[0].ID)
	assert.Equal(t, "Ann", users[0].Name)
	c.Expect(protocol.TypeSyncReady)
}

func TestHandshakeDeltaWhenLogCoversGap(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	for range 3 {
		_, err := env.Board.Append(context.Background(), protocol.EventCardCreated, map[string]string{"k": "v"}, userID)
		require.NoError(t, err)
	}

	c := inst.Dial(t, userID)
	c.Hello(int64Ptr(1))
	delta := c.Expect(protocol.TypeSyncDelta)
	var body protocol.SyncDelta
	require.NoError(t, json.Unmarshal(delta.Payload, &body))
	require.Len(t, body.Events, 2)
	assert.Equal(t, int64(2), body.Events[0].Seq)
	assert.Equal(t, int64(3), body.Events[1].Seq)
	c.Expect(protocol.TypePresenceList)
	c.Expect(protocol.TypeSyncReady)
}

func TestHandshakeEmptyDeltaWhenClientIsCurrent(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	_, err := env.Board.Append(context.Background(), protocol.EventCardCreated, map[string]string{}, userID)
	require.NoError(t, err)

	c := inst.Dial(t, userID)
	c.Hello(int64Ptr(1))
	var body protocol.SyncDelta
	require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeSyncDelta).Payload, &body))
	assert.Empty(t, body.Events)
	c.Expect(protocol.TypePresenceList)
	c.Expect(protocol.TypeSyncReady)
}

func TestHandshakeFallsBackToSnapshotWhenLogIsGoneOrClientIsAhead(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	for range 3 {
		_, err := env.Board.Append(context.Background(), protocol.EventCardCreated, map[string]string{}, userID)
		require.NoError(t, err)
	}
	env.Board.Truncate(2)

	for _, last := range []int64{0, 100} {
		c := inst.Dial(t, userID)
		c.Hello(int64Ptr(last))
		c.Expect(protocol.TypeSyncSnapshot)
		c.Expect(protocol.TypePresenceList)
		c.Expect(protocol.TypeSyncReady)
	}
}

func TestHandshakeFallsBackToSnapshotWhenDeltaIsTooLarge(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{MaxDeltaEvents: 2}
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	for range 3 {
		_, err := env.Board.Append(context.Background(), protocol.EventCardCreated, map[string]string{}, userID)
		require.NoError(t, err)
	}

	c := inst.Dial(t, userID)
	c.Hello(int64Ptr(0))
	c.Expect(protocol.TypeSyncSnapshot)
}

type gatedSync struct {
	realtime.SyncSource
	beforeRead func()
	afterRead  func()
}

func (g gatedSync) Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error) {
	if g.beforeRead != nil {
		g.beforeRead()
	}
	snap, err := g.SyncSource.Snapshot(ctx, userID, boardID)
	if g.afterRead != nil {
		g.afterRead()
	}
	return snap, err
}

func TestOpIsConfirmedThroughFanOutOnBothInstances(t *testing.T) {
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

	opID := sendOp(annConn, protocol.OpCardCreate)
	for _, c := range []*rttest.Conn{annConn, bobConn} {
		ev := c.Expect("card.create")
		assert.Equal(t, int64(1), ev.Seq)
		require.NotNil(t, ev.OpID)
		assert.Equal(t, opID, *ev.OpID)
	}
}

func TestReplayedOpIsSentDirectlyToInitiator(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	opID := uuid.New()
	c.SendJSON(protocol.OpCardCreate, &opID, map[string]any{"card_id": uuid.New().String()})
	first := c.Expect("card.create")
	c.SendJSON(protocol.OpCardCreate, &opID, map[string]any{"card_id": uuid.New().String()})
	again := c.Expect("card.create")
	assert.Equal(t, first.Seq, again.Seq)
	assert.Equal(t, 2, env.Board.ApplyCount())
	assert.Equal(t, int64(1), env.Board.Seq())
}

func TestViewerOpIsNackedWithoutReachingProcessor(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	viewer := rttest.AddUser(env, "Vic", domain.RoleViewer)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, viewer)
	c.HandshakeSnapshot()

	opID := sendOp(c, protocol.OpCardArchive)
	nack := c.Expect(protocol.TypeNack)
	require.NotNil(t, nack.OpID)
	assert.Equal(t, opID, *nack.OpID)
	var body protocol.Nack
	require.NoError(t, json.Unmarshal(nack.Payload, &body))
	assert.Equal(t, protocol.ReasonForbidden, body.Reason)
	assert.Zero(t, env.Board.ApplyCount())
}

func TestInfrastructureErrorBecomesInternalNack(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	for _, failure := range []error{errors.New("db down"), domain.ErrBusy} {
		env.Board.FailNextApply(failure)
		sendOp(c, protocol.OpCardArchive)
		var body protocol.Nack
		require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeNack).Payload, &body))
		assert.Equal(t, protocol.ReasonInternal, body.Reason)
	}
}

func TestOpBeforeSyncReadyIsProtocolErrorAndRepeatClosesConnection(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)

	sendOp(c, protocol.OpCardCreate)
	var body protocol.ErrorPayload
	require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeError).Payload, &body))
	assert.Equal(t, protocol.CodeProtocolError, body.Code)

	c.SendRaw([]byte(`{"v":1,"type":"nope"}`))
	c.Expect(protocol.TypeError)
	closeErr := c.ExpectClose()
	assert.Equal(t, protocol.CloseProtocolError, closeErr.Code)
	assert.Zero(t, env.Board.ApplyCount())
}

func TestProtocolViolationKindsAreAnsweredWithError(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{MaxProtocolViolations: 100}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	for _, raw := range []string{
		`not json`,
		`{"v":2,"type":"hello"}`,
		`{"v":1,"type":"op.unknown.thing","op_id":"` + uuid.NewString() + `"}`,
		`{"v":1,"type":"op.card.archive"}`,
		`{"v":1,"type":"hello","payload":{"last_seq":null}}`,
	} {
		c.SendRaw([]byte(raw))
		var body protocol.ErrorPayload
		require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeError).Payload, &body), raw)
		assert.Equal(t, protocol.CodeProtocolError, body.Code, raw)
	}
}

func TestRateLimitedOpsAreNackedAndAbuseClosesConnection(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{OpsBurst: 2, RateAbuseLimit: 3}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	for range 2 {
		sendOp(c, protocol.OpCardCreate)
		c.Expect("card.create")
	}
	for range 3 {
		sendOp(c, protocol.OpCardCreate)
		var body protocol.Nack
		require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeNack).Payload, &body))
		assert.Equal(t, protocol.ReasonRateLimited, body.Reason)
	}
	assert.Equal(t, protocol.CloseRateLimited, c.ExpectClose().Code)
	assert.Equal(t, 2, env.Board.ApplyCount())
}

func TestConnectionIsRejectedBeforeUpgrade(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	stranger := uuid.New()
	inst := env.StartInstance(t, "a")
	boardID := env.Board.ID

	cases := []struct {
		name   string
		board  uuid.UUID
		auth   string
		header http.Header
		status int
	}{
		{"missing token", boardID, "", nil, http.StatusUnauthorized},
		{"garbage token", boardID, "Bearer garbage", nil, http.StatusUnauthorized},
		{"not a member", boardID, "Bearer " + env.IssueToken(t, stranger), nil, http.StatusNotFound},
		{"unknown board", uuid.New(), "Bearer " + env.IssueToken(t, ann), nil, http.StatusNotFound},
		{"foreign origin", boardID, "Bearer " + env.IssueToken(t, ann), http.Header{"Origin": {"https://evil.example"}}, http.StatusForbidden},
	}
	for _, tc := range cases {
		conn, status, err := inst.TryDial(tc.board, tc.auth, tc.header)
		require.Error(t, err, tc.name)
		assert.Nil(t, conn, tc.name)
		assert.Equal(t, tc.status, status, tc.name)
	}
}

func TestOriginAllowlistAndMissingOrigin(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{AllowedOrigins: []string{"https://app.example"}}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	authz := "Bearer " + env.IssueToken(t, ann)

	conn, _, err := inst.TryDial(env.Board.ID, authz, http.Header{"Origin": {"https://app.example"}})
	require.NoError(t, err)
	_ = conn.Close()
	conn, _, err = inst.TryDial(env.Board.ID, authz, nil)
	require.NoError(t, err)
	_ = conn.Close()
	_, status, err := inst.TryDial(env.Board.ID, authz, http.Header{"Origin": {"https://other.example"}})
	require.Error(t, err)
	assert.Equal(t, http.StatusForbidden, status)
}

func TestBoardDeletedClosesAllConnectionsWithEvent(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleOwner)
	bob := rttest.AddUser(env, "Bob", domain.RoleViewer)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()
	bobConn := b.Dial(t, bob)
	bobConn.HandshakeSnapshot()

	_, err := env.Board.Delete(context.Background(), ann)
	require.NoError(t, err)
	for _, c := range []*rttest.Conn{annConn, bobConn} {
		assert.Equal(t, protocol.EventBoardDeleted, drainUntil(c, protocol.EventBoardDeleted).Type)
		assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code)
	}
}

func drainUntil(c *rttest.Conn, msgType string) protocol.Envelope {
	for {
		env := c.MustNext()
		if env.Type == msgType {
			return env
		}
	}
}

func TestRenamedEventIsBroadcastWithoutClosing(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleOwner)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	_, err := env.Board.Append(context.Background(), protocol.EventBoardRenamed, protocol.BoardRenamed{Title: "New", ActorID: ann}, ann)
	require.NoError(t, err)
	ev := c.Expect(protocol.EventBoardRenamed)
	assert.Equal(t, int64(1), ev.Seq)
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")
}

func TestHubCloseUserAndRoleUpdate(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	annConn := inst.Dial(t, ann)
	annConn.HandshakeSnapshot()
	bobConn := inst.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	annConn.Expect(protocol.TypePresenceJoin)

	inst.Hub.SetRole(env.Board.ID, ann, domain.RoleViewer)
	sendOp(annConn, protocol.OpCardCreate)
	var body protocol.Nack
	require.NoError(t, json.Unmarshal(annConn.Expect(protocol.TypeNack).Payload, &body))
	assert.Equal(t, protocol.ReasonForbidden, body.Reason)

	inst.Hub.CloseUser(env.Board.ID, bob, protocol.CloseAccessRevoked, "removed")
	assert.Equal(t, protocol.CloseAccessRevoked, bobConn.ExpectClose().Code)
	assert.Eventually(t, func() bool { return inst.Hub.ConnectionCount(env.Board.ID) == 1 }, 5*time.Second, 10*time.Millisecond)
}

func waitTimers(t *testing.T, env *rttest.Env, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return env.Clock.ActiveTimers() >= n }, 5*time.Second, 5*time.Millisecond)
}

func TestTokenExpiryClosesConnectionAndRefreshExtendsIt(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	waitTimers(t, env, 2)

	env.Clock.Advance(10 * time.Minute)
	c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: env.IssueToken(t, ann)})
	refreshed := c.Expect(protocol.TypeAuthRefreshed)
	var body protocol.AuthRefreshed
	require.NoError(t, json.Unmarshal(refreshed.Payload, &body))
	assert.Equal(t, env.Clock.Now().Add(15*time.Minute).Truncate(time.Second).UnixMilli(), body.ExpiresAt)
	waitTimers(t, env, 2)

	env.Clock.Advance(10 * time.Minute)
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")
	waitTimers(t, env, 2)

	env.Clock.Advance(6 * time.Minute)
	assert.Equal(t, protocol.CloseInvalidToken, c.ExpectClose().Code)
}

func TestRefreshWithTokenOfAnotherUserIsRejected(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: env.IssueToken(t, uuid.New())})
	var body protocol.ErrorPayload
	require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeError).Payload, &body))
	assert.Equal(t, protocol.CodeInvalidToken, body.Code)

	c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: "garbage"})
	require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeError).Payload, &body))
	assert.Equal(t, protocol.CodeInvalidToken, body.Code)
}

func TestPresenceJoinLeaveAcrossInstancesCountsDevicesOnce(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")

	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()

	bobPhone := b.Dial(t, bob)
	bobPhone.Hello(nil)
	bobPhone.Expect(protocol.TypeSyncSnapshot)
	users := presenceList(t, bobPhone.Expect(protocol.TypePresenceList))
	assert.Len(t, users, 2)
	bobPhone.Expect(protocol.TypeSyncReady)

	var join protocol.PresenceJoin
	require.NoError(t, json.Unmarshal(annConn.Expect(protocol.TypePresenceJoin).Payload, &join))
	assert.Equal(t, bob, join.User.ID)
	assert.Equal(t, "Bob", join.User.Name)
	assert.NotEmpty(t, join.User.AvatarColor)

	bobTablet := a.Dial(t, bob)
	bobTablet.HandshakeSnapshot()
	_ = bobTablet.Close()
	require.Eventually(t, func() bool { return a.Hub.ConnectionCount(env.Board.ID) == 1 }, 5*time.Second, 10*time.Millisecond)
	assertNoPresenceLeave(t, annConn, bobPhone)

	_ = bobPhone.Close()
	var leave protocol.PresenceLeave
	require.NoError(t, json.Unmarshal(drainUntil(annConn, protocol.TypePresenceLeave).Payload, &leave))
	assert.Equal(t, bob, leave.UserID)
}

func TestBoardChannelsAreSubscribedOnlyWhileClientsAreConnected(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	channel := protocol.BoardChannel(env.Board.ID)
	assert.False(t, inst.Broker.Subscribed(channel))

	first := inst.Dial(t, ann)
	first.HandshakeSnapshot()
	second := inst.Dial(t, ann)
	second.HandshakeSnapshot()
	assert.True(t, inst.Broker.Subscribed(channel))
	assert.True(t, inst.Broker.Subscribed(protocol.PresenceChannel(env.Board.ID)))

	_ = first.Close()
	assert.Eventually(t, func() bool { return inst.Hub.ConnectionCount(env.Board.ID) == 1 }, 5*time.Second, 10*time.Millisecond)
	assert.True(t, inst.Broker.Subscribed(channel))

	_ = second.Close()
	assert.Eventually(t, func() bool { return !inst.Broker.Subscribed(channel) }, 5*time.Second, 10*time.Millisecond)
}

func TestEventsPublishedDuringHandshakeAreDeliveredOnceAndNeverDuplicated(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	ctx := context.Background()
	appendEvent := func() {
		_, err := env.Board.Append(ctx, protocol.EventCardCreated, map[string]string{}, userID)
		require.NoError(t, err)
	}
	var once sync.Once
	env.Sync = gatedSync{
		SyncSource: env.Board,
		beforeRead: func() { once.Do(appendEvent) },
		afterRead:  appendEvent,
	}
	inst := env.StartInstance(t, "a")

	c := inst.Dial(t, userID)
	c.Hello(nil)
	var snap protocol.Snapshot
	require.NoError(t, json.Unmarshal(c.Expect(protocol.TypeSyncSnapshot).Payload, &snap))
	assert.Equal(t, int64(1), snap.Seq)
	c.Expect(protocol.TypePresenceList)

	var delivered []int64
	ready := false
	for !ready || len(delivered) == 0 {
		msg := c.MustNext()
		switch msg.Type {
		case protocol.EventCardCreated:
			delivered = append(delivered, msg.Seq)
		case protocol.TypeSyncReady:
			ready = true
		}
	}
	assert.Equal(t, []int64{2}, delivered)
}

func assertNoPresenceLeave(t *testing.T, observer, other *rttest.Conn) {
	t.Helper()
	sendOp(other, protocol.OpCardCreate)
	for {
		msg := observer.MustNext()
		require.NotEqual(t, protocol.TypePresenceLeave, msg.Type)
		if msg.Type == "card.create" {
			return
		}
	}
}

func TestMissingHelloClosesConnection(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{HelloTimeout: time.Minute, PingInterval: 1000 * time.Hour}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	waitTimers(t, env, 3)

	env.Clock.Advance(time.Minute)
	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
}

func TestMissedPongsCloseConnection(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PingInterval: 30 * time.Second, PongWait: 60 * time.Second}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	waitTimers(t, env, 2)

	pings := make(chan struct{}, 8)
	c.SetPingHandler(func(string) error {
		pings <- struct{}{}
		return nil
	})
	closed := make(chan error, 1)
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				closed <- err
				return
			}
		}
	}()

	env.Clock.Advance(30 * time.Second)
	select {
	case <-pings:
	case <-time.After(5 * time.Second):
		t.Fatal("no ping received")
	}
	env.Clock.Advance(30 * time.Second)
	select {
	case err := <-closed:
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
		assert.Equal(t, protocol.CloseGoingAway, closeErr.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("connection was not closed after missing pongs")
	}
}

func TestShutdownClosesConnectionsWithGoingAway(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- inst.Hub.Shutdown(ctx) }()
	assert.Equal(t, protocol.CloseGoingAway, c.ExpectClose().Code)
	require.NoError(t, <-shutdownDone)

	late, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
	if err == nil {
		defer func() { _ = late.Close() }()
		_, _, readErr := late.ReadMessage()
		var closeErr *websocket.CloseError
		require.ErrorAs(t, readErr, &closeErr)
		assert.Equal(t, protocol.CloseGoingAway, closeErr.Code)
	}
}

func TestOversizedMessageDropsConnection(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{MaxMessageBytes: 256}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	c.SendRaw(make([]byte, 1024))
	_, err := c.Next()
	require.Error(t, err)
	assert.Eventually(t, func() bool { return inst.Hub.ConnectionCount(env.Board.ID) == 0 }, 5*time.Second, 10*time.Millisecond)
}

func TestSlowClientIsDisconnectedWhenSendBufferOverflows(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{SendBuffer: 8, WriteWait: 200 * time.Millisecond}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	payload := map[string]string{"blob": string(make([]byte, 256<<10))}
	assert.Eventually(t, func() bool {
		_, err := env.Board.Append(context.Background(), protocol.EventCardCreated, payload, ann)
		require.NoError(t, err)
		return inst.Hub.ConnectionCount(env.Board.ID) == 0
	}, 20*time.Second, time.Millisecond)
}
