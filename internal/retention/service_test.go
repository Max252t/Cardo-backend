package retention_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/retention"
)

type scriptedStore struct {
	mu          sync.Mutex
	eventCalls  atomic.Int32
	opCalls     atomic.Int32
	eventCutoff []time.Time
	events      func(ctx context.Context) (retention.Result, error)
	ops         func(ctx context.Context) (retention.Result, error)
}

func (s *scriptedStore) PruneEvents(ctx context.Context, olderThan time.Time, _, _ int) (retention.Result, error) {
	s.eventCalls.Add(1)
	s.mu.Lock()
	s.eventCutoff = append(s.eventCutoff, olderThan)
	s.mu.Unlock()
	if s.events == nil {
		return retention.Result{}, nil
	}
	return s.events(ctx)
}

func (s *scriptedStore) PruneProcessedOps(ctx context.Context, _ time.Time, _ int) (retention.Result, error) {
	s.opCalls.Add(1)
	if s.ops == nil {
		return retention.Result{}, nil
	}
	return s.ops(ctx)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger(buf *lockedBuffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type steppingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppingClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestRunRepeatsOnTheConfiguredInterval(t *testing.T) {
	store := &scriptedStore{}
	cfg := validConfig()
	cfg.Interval = 10 * time.Millisecond
	svc, err := retention.New(store, cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool { return store.opCalls.Load() >= 3 }, 10*time.Second, 5*time.Millisecond)
	cancel()
	<-done
	assert.GreaterOrEqual(t, store.eventCalls.Load(), int32(3))
}

type gatedWaiter struct {
	waits chan time.Duration
	allow chan struct{}
}

func newGatedWaiter() *gatedWaiter {
	return &gatedWaiter{waits: make(chan time.Duration, 16), allow: make(chan struct{})}
}

func (w *gatedWaiter) Wait(ctx context.Context, d time.Duration) error {
	w.waits <- d
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.allow:
		return nil
	}
}

func TestRunRunsOnceImmediatelyThenWaitsForTheInterval(t *testing.T) {
	store := &scriptedStore{}
	waiter := newGatedWaiter()
	svc, err := retention.New(store, validConfig(), retention.WithWaiter(waiter))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	assert.Equal(t, validConfig().Interval, <-waiter.waits)
	assert.EqualValues(t, 1, store.opCalls.Load())
	assert.EqualValues(t, 1, store.eventCalls.Load())

	waiter.allow <- struct{}{}
	<-waiter.waits
	assert.EqualValues(t, 2, store.opCalls.Load())
	assert.EqualValues(t, 2, store.eventCalls.Load())
	cancel()
	<-done
}

func TestRunDoesNotStartWhenContextIsAlreadyCanceled(t *testing.T) {
	store := &scriptedStore{}
	svc, err := retention.New(store, validConfig())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	assert.LessOrEqual(t, store.eventCalls.Load(), int32(1))
}

func TestRunStopsDuringABlockedJob(t *testing.T) {
	started := make(chan struct{}, 1)
	store := &scriptedStore{events: func(ctx context.Context) (retention.Result, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return retention.Result{Deleted: 7}, ctx.Err()
	}}
	buf := &lockedBuffer{}
	spy := &metricsSpy{}
	svc, err := retention.New(store, validConfig(), retention.WithLogger(debugLogger(buf)), retention.WithMetrics(spy))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}

	assert.EqualValues(t, 7, spy.deleted[retention.TableBoardEvents], "work done before the cancellation is still counted")
	assert.Contains(t, buf.String(), "retention job interrupted")
	assert.NotContains(t, buf.String(), "level=ERROR")
}

func TestRunOnceUsesTheCurrentTimeOfTheClockOnEveryRun(t *testing.T) {
	clock := &steppingClock{now: time.Date(2031, 3, 1, 0, 0, 0, 0, time.UTC)}
	store := &scriptedStore{}
	svc, err := retention.New(store, validConfig(), retention.WithClock(clock))
	require.NoError(t, err)

	svc.RunOnce(context.Background())
	clock.Advance(48 * time.Hour)
	svc.RunOnce(context.Background())

	require.Len(t, store.eventCutoff, 2)
	assert.Equal(t, 48*time.Hour, store.eventCutoff[1].Sub(store.eventCutoff[0]))
	assert.Equal(t, clock.Now().Add(-7*24*time.Hour), store.eventCutoff[1])
}

func TestSkippedJobsAreNotCountedAndLogAtDebugLevel(t *testing.T) {
	store := &scriptedStore{
		events: func(context.Context) (retention.Result, error) { return retention.Result{Skipped: true}, nil },
		ops:    func(context.Context) (retention.Result, error) { return retention.Result{Skipped: true}, nil },
	}
	buf := &lockedBuffer{}
	spy := &metricsSpy{}
	svc, err := retention.New(store, validConfig(), retention.WithLogger(debugLogger(buf)), retention.WithMetrics(spy))
	require.NoError(t, err)

	svc.RunOnce(context.Background())

	assert.Empty(t, spy.deleted)
	out := buf.String()
	assert.Contains(t, out, "retention job skipped")
	assert.NotContains(t, out, "retention job finished")
	assert.NotContains(t, out, "level=ERROR")
}

func TestZeroDeletionsAreNotReportedToMetricsButAreLogged(t *testing.T) {
	buf := &lockedBuffer{}
	spy := &metricsSpy{}
	svc, err := retention.New(&scriptedStore{}, validConfig(), retention.WithLogger(debugLogger(buf)), retention.WithMetrics(spy))
	require.NoError(t, err)

	svc.RunOnce(context.Background())

	assert.Empty(t, spy.deleted)
	assert.Contains(t, buf.String(), "retention job finished")
	assert.Contains(t, buf.String(), "deleted=0")
}

func TestFailedJobLogsTableCountAndErrorAndStillCountsPartialWork(t *testing.T) {
	store := &scriptedStore{
		events: func(context.Context) (retention.Result, error) {
			return retention.Result{Deleted: 5}, errors.New("connection reset")
		},
		ops: func(context.Context) (retention.Result, error) { return retention.Result{Deleted: 2}, nil },
	}
	buf := &lockedBuffer{}
	spy := &metricsSpy{}
	svc, err := retention.New(store, validConfig(), retention.WithLogger(debugLogger(buf)), retention.WithMetrics(spy))
	require.NoError(t, err)

	svc.RunOnce(context.Background())

	assert.Equal(t, map[string]int64{retention.TableBoardEvents: 5, retention.TableProcessedOps: 2}, spy.deleted)
	out := buf.String()
	assert.Contains(t, out, "retention job failed")
	assert.Contains(t, out, "table=board_events")
	assert.Contains(t, out, "connection reset")
	assert.Contains(t, out, "table=processed_ops")
	assert.Contains(t, out, "retention job finished")
}

func TestRunOnceStillRunsProcessedOpsWhenContextIsCanceled(t *testing.T) {
	store := &scriptedStore{events: func(ctx context.Context) (retention.Result, error) {
		return retention.Result{}, ctx.Err()
	}}
	svc, err := retention.New(store, validConfig())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	svc.RunOnce(ctx)

	assert.EqualValues(t, 1, store.eventCalls.Load())
	assert.EqualValues(t, 1, store.opCalls.Load())
}

func TestConfigValidationBoundaries(t *testing.T) {
	valid := map[string]func(*retention.Config){
		"equal retentions":     func(c *retention.Config) { c.EventsRetention = c.ProcessedOpsRetention },
		"keep min zero":        func(c *retention.Config) { c.EventsKeepMin = 0 },
		"batch one":            func(c *retention.Config) { c.BatchSize = 1 },
		"smallest interval":    func(c *retention.Config) { c.Interval = time.Nanosecond },
		"events one ns longer": func(c *retention.Config) { c.EventsRetention = c.ProcessedOpsRetention + time.Nanosecond },
	}
	for name, mutate := range valid {
		t.Run("valid "+name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			assert.NoError(t, cfg.Validate())
		})
	}
	invalid := map[string]func(*retention.Config){
		"events one ns shorter": func(c *retention.Config) { c.EventsRetention = c.ProcessedOpsRetention - time.Nanosecond },
		"negative interval":     func(c *retention.Config) { c.Interval = -time.Second },
		"negative batch":        func(c *retention.Config) { c.BatchSize = -1 },
	}
	for name, mutate := range invalid {
		t.Run("invalid "+name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			assert.Error(t, cfg.Validate())
		})
	}
}

func TestConfigValidationReportsEveryProblem(t *testing.T) {
	err := retention.Config{}.Validate()
	require.Error(t, err)
	for _, part := range []string{retention.FieldInterval, retention.FieldEventsRetention, retention.FieldProcessedOpsRetention, retention.FieldBatchSize} {
		assert.ErrorContains(t, err, part)
	}
}

func TestNilOptionsKeepDefaults(t *testing.T) {
	store := &scriptedStore{events: func(context.Context) (retention.Result, error) { return retention.Result{Deleted: 1}, nil }}
	svc, err := retention.New(store, validConfig(), retention.WithClock(nil), retention.WithLogger(nil), retention.WithMetrics(nil))
	require.NoError(t, err)
	assert.NotPanics(t, func() { svc.RunOnce(context.Background()) })
}
