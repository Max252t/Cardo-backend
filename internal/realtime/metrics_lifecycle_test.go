package realtime_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/metrics"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

var quietConfig = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}

func startMetered(t *testing.T, cfg func(*rttest.Env)) (*rttest.Env, uuid.UUID, *rttest.Instance, *metricsRecorder) {
	t.Helper()
	env := rttest.NewEnv(t)
	if cfg != nil {
		cfg(env)
	}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })
	return env, ann, inst, rec
}

func awaitClosed(t *testing.T, rec *metricsRecorder, n int) []int {
	t.Helper()
	require.Eventually(t, func() bool {
		_, closed, _ := rec.snapshot()
		return len(closed) >= n
	}, 5*time.Second, 5*time.Millisecond)
	_, closed, _ := rec.snapshot()
	return closed
}

func countCode(codes []int, code int) int {
	n := 0
	for _, c := range codes {
		if c == code {
			n++
		}
	}
	return n
}

func advanceUntilClosed(t *testing.T, env *rttest.Env, rec *metricsRecorder, step time.Duration) []int {
	t.Helper()
	require.Eventually(t, func() bool {
		env.Clock.Advance(step)
		_, closed, _ := rec.snapshot()
		return len(closed) > 0
	}, 5*time.Second, 5*time.Millisecond)
	_, closed, _ := rec.snapshot()
	return closed
}

func TestMetricsRecordAccessRevokedClose(t *testing.T) {
	t.Parallel()
	env, ann, inst, rec := startMetered(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	inst.Hub.CloseUser(env.Board.ID, ann, protocol.CloseAccessRevoked, "revoked")

	assert.Equal(t, protocol.CloseAccessRevoked, c.ExpectClose().Code)
	assert.Equal(t, []int{protocol.CloseAccessRevoked}, awaitClosed(t, rec, 1))
}

func TestMetricsRecordBoardDeletedClose(t *testing.T) {
	t.Parallel()
	env, ann, inst, rec := startMetered(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	_, err := env.Board.Delete(context.Background(), ann)
	require.NoError(t, err)

	assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code)
	assert.Equal(t, []int{protocol.CloseBoardDeleted}, awaitClosed(t, rec, 1))
}

func TestMetricsRecordBoardDeletedFromApplyClose(t *testing.T) {
	t.Parallel()
	env, ann, inst, rec := startMetered(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	env.Board.FailNextApply(domain.ErrNotFound)

	sendOp(c, boardcontent.OpCardCreate)

	assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code)
	assert.Equal(t, []int{protocol.CloseBoardDeleted}, awaitClosed(t, rec, 1))
}

func TestMetricsRecordProtocolErrorClose(t *testing.T) {
	t.Parallel()
	_, ann, inst, rec := startMetered(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	c.SendRaw([]byte(`not json`))
	c.MustNext()
	c.SendRaw([]byte(`{"v":1,"type":"nonsense"}`))
	c.MustNext()

	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
	assert.Equal(t, []int{protocol.CloseProtocolError}, awaitClosed(t, rec, 1))
}

func TestMetricsRecordRateLimitedClose(t *testing.T) {
	t.Parallel()
	_, ann, inst, rec := startMetered(t, func(e *rttest.Env) {
		e.Config = realtime.Config{OpsPerSecond: 1, OpsBurst: 1, RateAbuseLimit: 5, RateAbuseWindow: 10 * time.Second}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	sendOp(c, boardcontent.OpCardCreate)
	c.Expect("card.create")
	for range 5 {
		sendOp(c, boardcontent.OpCardCreate)
		c.MustNext()
	}

	assert.Equal(t, protocol.CloseRateLimited, c.ExpectClose().Code)
	assert.Equal(t, []int{protocol.CloseRateLimited}, awaitClosed(t, rec, 1))
}

func TestMetricsRecordHelloTimeoutAsProtocolError(t *testing.T) {
	t.Parallel()
	env, ann, inst, rec := startMetered(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 10 * time.Second}
	})
	c := inst.Dial(t, ann)

	closed := advanceUntilClosed(t, env, rec, time.Second)

	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
	assert.Equal(t, []int{protocol.CloseProtocolError}, closed)
}

func TestMetricsRecordTokenExpiryClose(t *testing.T) {
	t.Parallel()
	env, ann, inst, rec := startMetered(t, func(e *rttest.Env) { e.Config = quietConfig })
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	closed := advanceUntilClosed(t, env, rec, time.Minute)

	assert.Equal(t, protocol.CloseInvalidToken, c.ExpectClose().Code)
	assert.Equal(t, []int{protocol.CloseInvalidToken}, closed)
}

func TestMetricsRecordHandshakeFailureAsInternalErrorWithoutResync(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	env.Sync = syncHooks{inner: env.Board, beforeSnapshot: func() error { return errors.New("db down") }}
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })
	c := inst.Dial(t, ann)

	c.Hello(nil)
	c.MustNext()

	assert.Equal(t, websocket.CloseInternalServerErr, c.ExpectClose().Code)
	assert.Equal(t, []int{websocket.CloseInternalServerErr}, awaitClosed(t, rec, 1))
	opened, _, resyncs := rec.snapshot()
	assert.Equal(t, 1, opened)
	assert.Empty(t, resyncs)
}

func TestMetricsRecordSlowClientCloseDuringHandshake(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PendingLimit: 2}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	observer := rttest.AddUser(env, "Obs", domain.RoleViewer)
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
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "late", func(d *realtime.Deps) { d.Metrics = rec })
	obs := inst.Dial(t, observer)
	obs.Hello(int64Ptr(0))
	obs.Expect(protocol.TypeSyncDelta)
	obs.Expect(protocol.TypePresenceList)
	obs.Expect(protocol.TypeSyncReady)
	c := inst.Dial(t, ann)
	c.Hello(nil)
	<-reached
	appendEvents(t, env, ann, 4)
	readEventSeqs(t, obs, boardcontent.EventCardCreated, 4)
	close(release)

	assert.Equal(t, protocol.CloseSlowClient, c.ExpectClose().Code)
	closed := awaitClosed(t, rec, 1)
	assert.Equal(t, []int{protocol.CloseSlowClient}, closed)
}

func TestMetricsRecordSilentClientAsNormalClose(t *testing.T) {
	t.Parallel()
	_, ann, inst, rec := startMetered(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PongWait: 300 * time.Millisecond, PingInterval: 1000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	w := watch(t, c, false)

	select {
	case err := <-w.done:
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
	case <-time.After(eventually):
		t.Fatal("connection was not closed after the pong wait")
	}

	assert.Equal(t, []int{protocol.CloseNormal}, awaitClosed(t, rec, 1))
}

func TestMetricsRecordClientInitiatedCloseCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload []byte
		want    int
	}{
		{"normal", websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), websocket.CloseNormalClosure},
		{"going away", websocket.FormatCloseMessage(websocket.CloseGoingAway, "navigating"), websocket.CloseGoingAway},
		{"application code", websocket.FormatCloseMessage(4999, "custom"), 4999},
		{"no status", nil, websocket.CloseNoStatusReceived},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ann, inst, rec := startMetered(t, nil)
			c := inst.Dial(t, ann)
			c.HandshakeSnapshot()

			require.NoError(t, c.WriteControl(websocket.CloseMessage, tt.payload, time.Now().Add(time.Second)))

			assert.Equal(t, []int{tt.want}, awaitClosed(t, rec, 1))
		})
	}
}

func TestMetricsRecordAbruptDisconnectBeforeHello(t *testing.T) {
	t.Parallel()
	_, ann, inst, rec := startMetered(t, nil)
	c := inst.Dial(t, ann)

	require.NoError(t, c.UnderlyingConn().Close())

	assert.Equal(t, []int{realtime.CloseAbnormal}, awaitClosed(t, rec, 1))
	opened, _, resyncs := rec.snapshot()
	assert.Equal(t, 1, opened)
	assert.Empty(t, resyncs)
}

func TestMetricsRecordShutdownAsGoingAwayForEveryClient(t *testing.T) {
	t.Parallel()
	_, ann, inst, rec := startMetered(t, nil)
	conns := make([]*rttest.Conn, 3)
	for i := range conns {
		conns[i] = inst.Dial(t, ann)
		conns[i].HandshakeSnapshot()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- inst.Hub.Shutdown(ctx) }()

	for _, c := range conns {
		assert.Equal(t, protocol.CloseGoingAway, c.ExpectClose().Code)
	}
	require.NoError(t, <-shutdownDone)
	assert.Equal(t, []int{protocol.CloseGoingAway, protocol.CloseGoingAway, protocol.CloseGoingAway}, awaitClosed(t, rec, 3))
}

func TestMetricsFirstCloseReasonWinsOverTransportError(t *testing.T) {
	t.Parallel()
	env, ann, inst, rec := startMetered(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	inst.Hub.CloseUser(env.Board.ID, ann, protocol.CloseAccessRevoked, "revoked")
	require.NoError(t, c.UnderlyingConn().Close())

	assert.Equal(t, []int{protocol.CloseAccessRevoked}, awaitClosed(t, rec, 1))
	time.Sleep(50 * time.Millisecond)
	_, closed, _ := rec.snapshot()
	assert.Len(t, closed, 1, "a connection is reported closed exactly once")
}

func TestMetricsOpenedAndClosedBalanceAfterMixedChurn(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	cara := rttest.AddUser(env, "Cara", domain.RoleEditor)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })
	const each = 4

	var graceful, abrupt, revoked []*rttest.Conn
	for range each {
		for _, g := range []struct {
			user uuid.UUID
			list *[]*rttest.Conn
		}{{ann, &graceful}, {bob, &abrupt}, {cara, &revoked}} {
			c := inst.Dial(t, g.user)
			c.HandshakeSnapshot()
			*g.list = append(*g.list, c)
		}
	}
	waitConnections(t, inst, env.Board.ID, 3*each)
	opened, closedNow, _ := rec.snapshot()
	assert.Equal(t, 3*each, opened)
	assert.Empty(t, closedNow)

	for _, c := range graceful {
		require.NoError(t, c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
	}
	for _, c := range abrupt {
		require.NoError(t, c.UnderlyingConn().Close())
	}
	inst.Hub.CloseUser(env.Board.ID, cara, protocol.CloseAccessRevoked, "revoked")

	for _, c := range revoked {
		assert.Equal(t, protocol.CloseAccessRevoked, c.ExpectClose().Code)
	}
	closed := awaitClosed(t, rec, 3*each)
	waitConnections(t, inst, env.Board.ID, 0)
	assert.Len(t, closed, 3*each)
	assert.Equal(t, each, countCode(closed, protocol.CloseNormal))
	assert.Equal(t, each, countCode(closed, realtime.CloseAbnormal))
	assert.Equal(t, each, countCode(closed, protocol.CloseAccessRevoked))
}

func TestMetricsIgnoreRejectedUpgrades(t *testing.T) {
	t.Parallel()
	env, _, inst, rec := startMetered(t, nil)

	_, status, err := inst.TryDial(env.Board.ID, "Bearer garbage", nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, status)
	_, status, err = inst.TryDial(env.Board.ID, "", nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, status)
	stranger := uuid.New()
	_, status, err = inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, stranger), nil)
	require.Error(t, err)
	assert.GreaterOrEqual(t, status, 400)

	opened, closed, resyncs := rec.snapshot()
	assert.Zero(t, opened)
	assert.Empty(t, closed)
	assert.Empty(t, resyncs)
}

func TestMetricsIgnoreConnectionRefusedBySubscribeFailure(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) {
		d.Bus = failingBus{inner: d.Bus}
		d.Metrics = rec
	})
	conn, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	_, _, readErr := conn.ReadMessage()
	var closeErr *websocket.CloseError
	require.ErrorAs(t, readErr, &closeErr)
	assert.Equal(t, protocol.CloseTryAgainLater, closeErr.Code)

	opened, closed, _ := rec.snapshot()
	assert.Zero(t, opened)
	assert.Empty(t, closed)
}

func TestMetricsIgnoreConnectionsRefusedDuringShutdown(t *testing.T) {
	t.Parallel()
	env, ann, inst, rec := startMetered(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, inst.Hub.Shutdown(ctx))

	conn, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
	if err == nil {
		t.Cleanup(func() { _ = conn.Close() })
		_, _, readErr := conn.ReadMessage()
		var closeErr *websocket.CloseError
		require.ErrorAs(t, readErr, &closeErr)
		assert.Equal(t, protocol.CloseGoingAway, closeErr.Code)
	}

	opened, closed, _ := rec.snapshot()
	assert.Zero(t, opened)
	assert.Empty(t, closed)
}

func TestMetricsResyncKindFollowsHandshakeOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t testing.TB, env *rttest.Env, user uuid.UUID)
		lastSeq *int64
		want    string
	}{
		{"no last seq", func(testing.TB, *rttest.Env, uuid.UUID) {}, nil, realtime.ResyncSnapshot},
		{"empty board at zero", func(testing.TB, *rttest.Env, uuid.UUID) {}, int64Ptr(0), realtime.ResyncDelta},
		{"behind the head", func(t testing.TB, env *rttest.Env, user uuid.UUID) { appendEvents(t, env, user, 3) }, int64Ptr(1), realtime.ResyncDelta},
		{"up to date", func(t testing.TB, env *rttest.Env, user uuid.UUID) { appendEvents(t, env, user, 3) }, int64Ptr(3), realtime.ResyncDelta},
		{"events pruned", func(t testing.TB, env *rttest.Env, user uuid.UUID) {
			appendEvents(t, env, user, 5)
			env.Board.Truncate(3)
		}, int64Ptr(1), realtime.ResyncSnapshot},
		{"prune edge", func(t testing.TB, env *rttest.Env, user uuid.UUID) {
			appendEvents(t, env, user, 6)
			env.Board.Truncate(3)
		}, int64Ptr(3), realtime.ResyncDelta},
		{"client ahead", func(t testing.TB, env *rttest.Env, user uuid.UUID) { appendEvents(t, env, user, 2) }, int64Ptr(50), realtime.ResyncSnapshot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, ann, inst, rec := startMetered(t, nil)
			tt.prepare(t, env, ann)
			c := inst.Dial(t, ann)

			c.Hello(tt.lastSeq)
			c.MustNext()
			c.Expect(protocol.TypePresenceList)
			c.Expect(protocol.TypeSyncReady)

			_, _, resyncs := rec.snapshot()
			assert.Equal(t, []string{tt.want}, resyncs)
		})
	}
}

func TestMetricsResyncIsCountedOncePerHandshake(t *testing.T) {
	t.Parallel()
	_, ann, inst, rec := startMetered(t, nil)

	for range 3 {
		c := inst.Dial(t, ann)
		c.HandshakeSnapshot()
	}
	d := inst.Dial(t, ann)
	d.Hello(int64Ptr(0))
	d.MustNext()
	d.Expect(protocol.TypePresenceList)
	d.Expect(protocol.TypeSyncReady)

	_, _, resyncs := rec.snapshot()
	assert.Equal(t, 3, countKind(resyncs, realtime.ResyncSnapshot))
	assert.Equal(t, 1, countKind(resyncs, realtime.ResyncDelta))
	assert.Len(t, resyncs, 4)
}

func TestMetricsResyncIsNotCountedForInvalidHello(t *testing.T) {
	t.Parallel()
	_, ann, inst, rec := startMetered(t, nil)
	c := inst.Dial(t, ann)

	c.SendRaw([]byte(`{"v":1,"type":"hello","payload":{"last_seq":-5}}`))
	c.MustNext()

	_, _, resyncs := rec.snapshot()
	assert.Empty(t, resyncs)
}

func countKind(kinds []string, kind string) int {
	return len(slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return k != kind }))
}

func TestMetricsInstancesReportIndependently(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	recA, recB := &metricsRecorder{}, &metricsRecorder{}
	a := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = recA })
	b := env.StartInstanceWith(t, "b", func(d *realtime.Deps) { d.Metrics = recB })

	a.Dial(t, ann).HandshakeSnapshot()
	b.Dial(t, ann).HandshakeSnapshot()
	b.Dial(t, ann).HandshakeSnapshot()

	openedA, _, _ := recA.snapshot()
	openedB, _, _ := recB.snapshot()
	assert.Equal(t, 1, openedA)
	assert.Equal(t, 2, openedB)
}

func TestHubWithoutMetricsDependencyRuns(t *testing.T) {
	t.Parallel()
	_, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)

	c.HandshakeSnapshot()

	require.NoError(t, c.UnderlyingConn().Close())
}

func scrapeBody(t *testing.T, c *metrics.Collector) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestPrometheusCollectorTracksRealtimeLifecycle(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	collector := metrics.New()
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = collector })

	snapshotConn := inst.Dial(t, ann)
	snapshotConn.HandshakeSnapshot()
	deltaConn := inst.Dial(t, ann)
	deltaConn.Hello(int64Ptr(0))
	deltaConn.MustNext()
	deltaConn.Expect(protocol.TypePresenceList)
	deltaConn.Expect(protocol.TypeSyncReady)

	body := scrapeBody(t, collector)
	assert.Contains(t, body, "ws_connections 2")
	assert.Contains(t, body, `resync_total{kind="snapshot"} 1`)
	assert.Contains(t, body, `resync_total{kind="delta"} 1`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- inst.Hub.Shutdown(ctx) }()
	assert.Equal(t, protocol.CloseGoingAway, snapshotConn.ExpectClose().Code)
	assert.Equal(t, protocol.CloseGoingAway, deltaConn.ExpectClose().Code)
	require.NoError(t, <-shutdownDone)

	require.Eventually(t, func() bool {
		body := scrapeBody(t, collector)
		return slices.Contains(splitLines(body), `ws_closed_total{code="1001"} 2`) && slices.Contains(splitLines(body), "ws_connections 0")
	}, 5*time.Second, 10*time.Millisecond)
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := range len(s) {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	return append(lines, s[start:])
}
