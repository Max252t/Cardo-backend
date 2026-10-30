package realtime_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/jwtauth"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func TestSlowClientIsDroppedWithoutStallingFastClientsOrTheHub(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{SendBuffer: 8, WriteWait: 200 * time.Millisecond}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	inst := env.StartInstance(t, "a")

	slow := inst.Dial(t, ann)
	slow.HandshakeSnapshot()
	fast := inst.Dial(t, bob)
	fast.HandshakeSnapshot()
	waitConnections(t, inst, env.Board.ID, 2)

	received := make(chan int64, 1024)
	readErr := make(chan error, 1)
	go func() {
		for {
			msg, err := fast.Next()
			if err != nil {
				readErr <- err
				return
			}
			if msg.Seq > 0 {
				received <- msg.Seq
			}
		}
	}()

	payload := map[string]string{"blob": strings.Repeat("x", 256<<10)}
	var last int64
	for i := 0; i < 400 && inst.Hub.ConnectionCount(env.Board.ID) == 2; i++ {
		ev, err := env.Board.Append(context.Background(), boardcontent.EventCardCreated, payload, ann)
		require.NoError(t, err)
		last = ev.Seq
		select {
		case seq := <-received:
			require.Equal(t, last, seq, "the fast client sees every event in order")
		case err := <-readErr:
			t.Fatalf("fast client was disconnected: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the fast client stopped receiving while a slow client was attached")
		}
	}
	waitConnections(t, inst, env.Board.ID, 1)
	require.Positive(t, last)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = inst.Hub.ConnectionCount(env.Board.ID)
		ev, err := env.Board.Append(context.Background(), boardcontent.EventCardArchived, map[string]string{}, ann)
		assert.NoError(t, err)
		assert.Equal(t, last+1, ev.Seq)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub is blocked")
	}
	assert.Equal(t, last+1, (<-received))
}

func TestOverflowingOneBoardDoesNotAffectAnother(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{SendBuffer: 4, WriteWait: 200 * time.Millisecond}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	mb := multiBoard{boards: map[uuid.UUID]*rttest.Board{env.Board.ID: env.Board}}
	second := addBoard(t, env, mb, ann)
	env.Ops, env.Sync, env.Members = mb, mb, mb
	inst := env.StartInstance(t, "a")

	slow := env.DialBoard(t, inst, env.Board.ID, ann)
	slow.HandshakeSnapshot()
	healthy := env.DialBoard(t, inst, second.ID, ann)
	healthy.HandshakeSnapshot()

	payload := map[string]string{"blob": strings.Repeat("x", 256<<10)}
	for i := 0; i < 400 && inst.Hub.ConnectionCount(env.Board.ID) == 1; i++ {
		_, err := env.Board.Append(context.Background(), boardcontent.EventCardCreated, payload, ann)
		require.NoError(t, err)
	}
	waitConnections(t, inst, env.Board.ID, 0)

	_, err := second.Append(context.Background(), boardcontent.EventCardCreated, map[string]string{}, ann)
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, readEventSeqs(t, healthy, boardcontent.EventCardCreated, 1))
	assert.Equal(t, 1, inst.Hub.ConnectionCount(second.ID))
}

func shortLivedTokens(env *rttest.Env, ttl time.Duration) *jwtauth.Manager {
	return jwtauth.New(jwtauth.Config{
		Secret:   []byte("0123456789abcdef0123456789abcdef"),
		Issuer:   "test",
		Audience: "test",
		TTL:      ttl,
	}, env.Clock)
}

func TestAuthRefreshExtendsTheConnectionLifetimeRepeatedly(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	waitTimers(t, env, 2)

	for range 4 {
		env.Clock.Advance(14 * time.Minute)
		c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: env.IssueToken(t, ann)})
		refreshed := decodeAs[protocol.AuthRefreshed](t, c.Expect(protocol.TypeAuthRefreshed))
		assert.Equal(t, env.Clock.Now().Add(15*time.Minute).Truncate(time.Second).UnixMilli(), refreshed.ExpiresAt)
		waitTimers(t, env, 2)
	}
	opID := sendOp(c, boardcontent.OpCardCreate)
	assert.Equal(t, int64(1), awaitOwnEvent(c, opID).Seq)

	env.Clock.Advance(16 * time.Minute)
	assert.Equal(t, protocol.CloseInvalidToken, c.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestConnectionIsKeptUntilTheLastMomentBeforeExpiry(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	waitTimers(t, env, 2)

	env.Clock.Advance(15*time.Minute - time.Second)
	opID := sendOp(c, boardcontent.OpCardCreate)
	awaitOwnEvent(c, opID)
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))

	env.Clock.Advance(2 * time.Second)
	assert.Equal(t, protocol.CloseInvalidToken, c.ExpectClose().Code)
}

func TestIdleConnectionIsClosedAtTokenExpiry(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	waitTimers(t, env, 2)
	env.Clock.Advance(16 * time.Minute)
	assert.Equal(t, protocol.CloseInvalidToken, c.ExpectClose().Code)
}

func TestAuthRefreshRejections(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour, MaxProtocolViolations: 100}
	})
	short := shortLivedTokens(env, time.Minute)
	expired, _, err := short.Issue(ann)
	require.NoError(t, err)
	foreignKey, _, err := jwtauth.New(jwtauth.Config{
		Secret: []byte("another-secret-another-secret-12"), Issuer: "test", Audience: "test", TTL: time.Hour,
	}, env.Clock).Issue(ann)
	require.NoError(t, err)
	otherAudience, _, err := jwtauth.New(jwtauth.Config{
		Secret: []byte("0123456789abcdef0123456789abcdef"), Issuer: "test", Audience: "elsewhere", TTL: time.Hour,
	}, env.Clock).Issue(ann)
	require.NoError(t, err)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	waitTimers(t, env, 2)

	env.Clock.Advance(2 * time.Minute)
	tests := []struct {
		name  string
		token string
	}{
		{"expired token", expired},
		{"signed with another key", foreignKey},
		{"issued for another audience", otherAudience},
		{"token of another user", env.IssueToken(t, uuid.New())},
		{"garbage", "garbage"},
		{"whitespace", "   "},
	}
	for _, tt := range tests {
		c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: tt.token})
		assert.Equal(t, protocol.CodeInvalidToken, errorCode(t, c.MustNext()), tt.name)
	}

	env.Clock.Advance(14 * time.Minute)
	assert.Equal(t, protocol.CloseInvalidToken, c.ExpectClose().Code, "rejected refreshes must not extend the connection")
}

func TestAuthRefreshWithValidTokenAfterRejectionStillWorks(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	waitTimers(t, env, 2)

	c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: "garbage"})
	assert.Equal(t, protocol.CodeInvalidToken, errorCode(t, c.MustNext()))
	env.Clock.Advance(10 * time.Minute)
	c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: env.IssueToken(t, ann)})
	c.Expect(protocol.TypeAuthRefreshed)
	waitTimers(t, env, 2)
	env.Clock.Advance(10 * time.Minute)
	opID := sendOp(c, boardcontent.OpCardCreate)
	awaitOwnEvent(c, opID)
}

func TestAuthRefreshAppliesToEveryDeviceOnlyForTheConnectionThatSentIt(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	refreshed := inst.Dial(t, ann)
	refreshed.HandshakeSnapshot()
	stale := inst.Dial(t, ann)
	stale.HandshakeSnapshot()
	waitTimers(t, env, 4)

	env.Clock.Advance(10 * time.Minute)
	refreshed.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: env.IssueToken(t, ann)})
	refreshed.Expect(protocol.TypeAuthRefreshed)
	waitTimers(t, env, 4)

	env.Clock.Advance(6 * time.Minute)
	assert.Equal(t, protocol.CloseInvalidToken, stale.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 1)
	opID := sendOp(refreshed, boardcontent.OpCardCreate)
	awaitOwnEvent(refreshed, opID)
}

func TestApplyReportingBoardNotFoundClosesWith4004(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	env.Board.FailNextApply(domain.ErrNotFound)
	sendOp(c, boardcontent.OpCardCreate)
	assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestApplyReportingForbiddenIsANack(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	env.Board.FailNextApply(domain.ErrForbidden)
	opID := sendOp(c, boardcontent.OpCardCreate)
	nack := c.MustNext()
	assert.Equal(t, boardcontent.ReasonForbidden, nackReason(t, nack))
	assert.Equal(t, opID, *nack.OpID)
	next := sendOp(c, boardcontent.OpCardCreate)
	assert.Equal(t, int64(1), awaitOwnEvent(c, next).Seq)
}

func TestApplyValidationErrorBecomesValidationNack(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	env.Board.FailNextApply(domain.NewValidationError("title", "is required"))
	sendOp(c, boardcontent.OpCardCreate)
	nack := c.MustNext()
	assert.Equal(t, boardcontent.ReasonValidation, nackReason(t, nack))
	assert.Contains(t, decodeAs[boardcontent.Nack](t, nack).Message, "title")
}

func TestInternalNackNeverLeaksTheUnderlyingError(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	env.Board.FailNextApply(errors.New("pq: password authentication failed for user cardo"))
	sendOp(c, boardcontent.OpCardCreate)
	nack := c.MustNext()
	assert.Equal(t, boardcontent.ReasonInternal, nackReason(t, nack))
	assert.NotContains(t, string(nack.Payload), "pq:")
}

func TestBoardDeletedClosesEveryConnectionOnEveryInstance(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	owner := rttest.AddUser(env, "Owner", domain.RoleOwner)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	var conns []*rttest.Conn
	for _, inst := range []*rttest.Instance{a, a, b, b, b} {
		c := inst.Dial(t, owner)
		c.HandshakeSnapshot()
		conns = append(conns, c)
	}
	waitConnections(t, a, env.Board.ID, 2)
	waitConnections(t, b, env.Board.ID, 3)

	ev, err := env.Board.Delete(context.Background(), owner)
	require.NoError(t, err)
	for i, c := range conns {
		deleted := drainUntil(c, boardcontent.EventBoardDeleted)
		assert.Equal(t, ev.Seq, deleted.Seq, i)
		assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code, i)
	}
	waitConnections(t, a, env.Board.ID, 0)
	waitConnections(t, b, env.Board.ID, 0)
	waitSubscribed(t, a, env.Board.ID, false)
	waitSubscribed(t, b, env.Board.ID, false)

	_, status, dialErr := a.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, owner), nil)
	require.Error(t, dialErr)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestBoardDeletedWhileAClientIsHandshaking(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, nil)
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	env.Sync = syncHooks{
		inner: env.Board,
		beforeSnapshot: func() error {
			once.Do(func() { close(reached) })
			<-release
			return nil
		},
	}
	inst := env.StartInstance(t, "late")
	c := inst.Dial(t, ann)
	c.Hello(nil)
	<-reached
	_, err := env.Board.Delete(context.Background(), ann)
	require.NoError(t, err)
	close(release)
	assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestCloseBoardCloseUserAndSetRole(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleViewer)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	annPhone := inst.Dial(t, ann)
	annPhone.HandshakeSnapshot()
	annLaptop := inst.Dial(t, ann)
	annLaptop.HandshakeSnapshot()
	bobConn := inst.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	waitConnections(t, inst, env.Board.ID, 3)

	sendOp(annPhone, boardcontent.OpCardCreate)
	assert.Equal(t, boardcontent.ReasonForbidden, nackReason(t, drainNack(annPhone)))
	env.Board.AddMember(ann, "Ann", domain.RoleEditor)
	inst.Hub.SetRole(env.Board.ID, ann, domain.RoleEditor)
	for _, c := range []*rttest.Conn{annPhone, annLaptop} {
		opID := sendOp(c, boardcontent.OpCardCreate)
		assert.NotNil(t, awaitOwnEvent(c, opID).OpID)
	}
	inst.Hub.SetRole(env.Board.ID, uuid.New(), domain.RoleViewer)
	inst.Hub.SetRole(uuid.New(), bob, domain.RoleViewer)
	opID := sendOp(bobConn, boardcontent.OpCardCreate)
	awaitOwnEvent(bobConn, opID)

	inst.Hub.CloseUser(env.Board.ID, ann, protocol.CloseAccessRevoked, "removed from the board")
	assert.Equal(t, protocol.CloseAccessRevoked, annPhone.ExpectClose().Code)
	assert.Equal(t, protocol.CloseAccessRevoked, annLaptop.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 1)

	inst.Hub.CloseUser(uuid.New(), bob, protocol.CloseAccessRevoked, "wrong board")
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))

	inst.Hub.CloseBoard(env.Board.ID, protocol.CloseGoingAway, "maintenance")
	assert.Equal(t, protocol.CloseGoingAway, bobConn.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 0)
	waitSubscribed(t, inst, env.Board.ID, false)
	inst.Hub.CloseBoard(env.Board.ID, protocol.CloseGoingAway, "again")
}

func TestCloseReasonIsTruncatedToTheControlFrameLimit(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	inst.Hub.CloseBoard(env.Board.ID, protocol.CloseGoingAway, strings.Repeat("r", 500))
	closeErr := c.ExpectClose()
	assert.Equal(t, protocol.CloseGoingAway, closeErr.Code)
	assert.LessOrEqual(t, len(closeErr.Text), 123)
}

func TestCustomEventHooksRunForMatchingEventsOnly(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	type hit struct {
		board uuid.UUID
		seq   int64
	}
	var mu sync.Mutex
	var hits []hit
	hits1 := make(chan struct{}, 8)
	inst := env.StartInstanceWith(t, "a", nil,
		realtime.WithEventHook(boardcontent.EventBoardRenamed, func(h *realtime.Hub, boardID uuid.UUID, ev protocol.Envelope) {
			mu.Lock()
			hits = append(hits, hit{boardID, ev.Seq})
			mu.Unlock()
			hits1 <- struct{}{}
		}),
		realtime.WithEventHook(boardcontent.EventBoardRenamed, func(h *realtime.Hub, boardID uuid.UUID, ev protocol.Envelope) {
			hits1 <- struct{}{}
		}),
	)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	appendEvents(t, env, ann, 1)
	_, err := env.Board.Append(context.Background(), boardcontent.EventBoardRenamed, boardcontent.BoardRenamed{Title: "New", ActorID: ann}, ann)
	require.NoError(t, err)
	<-hits1
	<-hits1
	mu.Lock()
	assert.Equal(t, []hit{{env.Board.ID, 2}}, hits)
	mu.Unlock()
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID), "a custom hook does not close connections")
}

func TestDefaultBoardDeletedHookStillRunsAlongsideCustomHooks(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleOwner)
	called := make(chan struct{}, 1)
	inst := env.StartInstanceWith(t, "a", nil,
		realtime.WithEventHook(boardcontent.EventBoardDeleted, func(*realtime.Hub, uuid.UUID, protocol.Envelope) { called <- struct{}{} }),
	)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	_, err := env.Board.Delete(context.Background(), ann)
	require.NoError(t, err)
	<-called
	assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code)
}

func TestNewHubRequiresItsDependencies(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	full := realtime.Deps{
		Bus: env.Network.NewBroker(), Presence: nopPresence{}, Ops: env.Board, Sync: env.Board, Members: env.Board, Tokens: env.Tokens,
	}
	hub, err := realtime.NewHub(full, realtime.Config{})
	require.NoError(t, err)
	assert.NotNil(t, hub)

	mutations := map[string]func(*realtime.Deps){
		"bus":      func(d *realtime.Deps) { d.Bus = nil },
		"presence": func(d *realtime.Deps) { d.Presence = nil },
		"ops":      func(d *realtime.Deps) { d.Ops = nil },
		"sync":     func(d *realtime.Deps) { d.Sync = nil },
		"members":  func(d *realtime.Deps) { d.Members = nil },
		"tokens":   func(d *realtime.Deps) { d.Tokens = nil },
	}
	for name, mutate := range mutations {
		d := full
		mutate(&d)
		_, err := realtime.NewHub(d, realtime.Config{})
		assert.Error(t, err, name)
	}
}

type nopPresence struct{}

func (nopPresence) Join(context.Context, uuid.UUID, domain.PresenceUser) error    { return nil }
func (nopPresence) Leave(context.Context, uuid.UUID, uuid.UUID) error             { return nil }
func (nopPresence) Touch(context.Context, uuid.UUID, []domain.PresenceUser) error { return nil }
func (nopPresence) Sweep(context.Context, uuid.UUID) error                        { return nil }
func (nopPresence) Online(context.Context, uuid.UUID) ([]domain.PresenceUser, error) {
	return nil, nil
}

func TestPresenceListIsAnEmptyArrayWhenPresenceStoreFails(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Presence = failingPresence{} })
	c := inst.Dial(t, ann)
	c.Hello(nil)
	c.Expect(protocol.TypeSyncSnapshot)
	list := c.Expect(protocol.TypePresenceList)
	assert.JSONEq(t, `{"users":[]}`, string(list.Payload))
	c.Expect(protocol.TypeSyncReady)
}

type failingPresence struct{ nopPresence }

func (failingPresence) Join(context.Context, uuid.UUID, domain.PresenceUser) error {
	return errors.New("redis down")
}

func (failingPresence) Online(context.Context, uuid.UUID) ([]domain.PresenceUser, error) {
	return nil, errors.New("redis down")
}

func TestConnectionFailsWith1013WhenTheBrokerCannotSubscribe(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Bus = failingBus{inner: d.Bus} })
	conn, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, _, readErr := conn.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, readErr, &closeErr)
	assert.Equal(t, protocol.CloseTryAgainLater, closeErr.Code)
	assert.Zero(t, inst.Hub.ConnectionCount(env.Board.ID))
}

type failingBus struct {
	inner realtime.Bus
}

func (failingBus) Subscribe(context.Context, ...string) error {
	return errors.New("redis down")
}

func (b failingBus) Unsubscribe(ctx context.Context, channels ...string) error {
	return b.inner.Unsubscribe(ctx, channels...)
}
