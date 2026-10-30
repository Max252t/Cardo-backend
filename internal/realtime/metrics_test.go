package realtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

type metricsRecorder struct {
	mu      sync.Mutex
	opened  int
	closed  []int
	resyncs []string
	refused []int
	nacks   []string
}

func (r *metricsRecorder) ConnectionRefused(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused = append(r.refused, code)
}

func (r *metricsRecorder) OpRejected(opType, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nacks = append(r.nacks, opType+"/"+reason)
}

func (r *metricsRecorder) ConnectionOpened() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened++
}

func (r *metricsRecorder) ConnectionClosed(code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = append(r.closed, code)
}

func (r *metricsRecorder) Resync(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resyncs = append(r.resyncs, kind)
}

func (r *metricsRecorder) snapshot() (int, []int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.opened, append([]int(nil), r.closed...), append([]string(nil), r.resyncs...)
}

func TestMetricsRecordConnectionLifecycleAndResync(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })

	snapshotConn := inst.Dial(t, userID)
	snapshotConn.HandshakeSnapshot()
	deltaConn := inst.Dial(t, userID)
	deltaConn.Hello(int64Ptr(0))
	deltaConn.Expect(protocol.TypeSyncDelta)
	deltaConn.Expect(protocol.TypePresenceList)
	deltaConn.Expect(protocol.TypeSyncReady)

	opened, _, resyncs := rec.snapshot()
	assert.Equal(t, 2, opened)
	assert.ElementsMatch(t, []string{realtime.ResyncSnapshot, realtime.ResyncDelta}, resyncs)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = inst.Hub.Shutdown(ctx) }()
	assert.Equal(t, protocol.CloseGoingAway, snapshotConn.ExpectClose().Code)
	assert.Equal(t, protocol.CloseGoingAway, deltaConn.ExpectClose().Code)
	require.Eventually(t, func() bool {
		_, closed, _ := rec.snapshot()
		return len(closed) == 2
	}, 5*time.Second, 10*time.Millisecond)
	_, closed, _ := rec.snapshot()
	assert.Equal(t, []int{protocol.CloseGoingAway, protocol.CloseGoingAway}, closed)
}

func TestMetricsRecordAbnormalClientDisconnect(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })

	c := inst.Dial(t, userID)
	c.HandshakeSnapshot()
	require.NoError(t, c.Conn.UnderlyingConn().Close())
	require.Eventually(t, func() bool {
		_, closed, _ := rec.snapshot()
		return len(closed) == 1
	}, 5*time.Second, 10*time.Millisecond)
	_, closed, _ := rec.snapshot()
	assert.Equal(t, []int{protocol.CloseAbnormal}, closed)
}

func TestMetricsCountLocallyRejectedOps(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	viewer := rttest.AddUser(env, "Vic", domain.RoleViewer)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })

	c := inst.Dial(t, viewer)
	c.HandshakeSnapshot()
	sendOp(c, boardcontent.OpCardCreate)
	c.Expect(protocol.TypeNack)

	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, []string{boardcontent.OpCardCreate + "/" + boardcontent.ReasonForbidden}, rec.nacks)
}

func TestMetricsCountRefusedConnectionsWithoutOpeningThem(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, inst.Hub.Shutdown(ctx))

	c := inst.Dial(t, userID)
	assert.Equal(t, protocol.CloseGoingAway, c.ExpectClose().Code)

	opened, closed, _ := rec.snapshot()
	assert.Zero(t, opened)
	assert.Empty(t, closed)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	assert.Equal(t, []int{protocol.CloseGoingAway}, rec.refused)
}

func TestMetricsRecordOversizedMessageAsTooBig(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	userID := rttest.AddUser(env, "Ann", domain.RoleEditor)
	rec := &metricsRecorder{}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) { d.Metrics = rec })

	c := inst.Dial(t, userID)
	c.HandshakeSnapshot()
	c.SendRaw(make([]byte, realtime.DefaultMaxMessageBytes+1024))

	require.Eventually(t, func() bool {
		_, closed, _ := rec.snapshot()
		return len(closed) == 1
	}, 5*time.Second, 10*time.Millisecond)
	_, closed, _ := rec.snapshot()
	assert.Equal(t, []int{protocol.CloseMessageTooBig}, closed)
}
