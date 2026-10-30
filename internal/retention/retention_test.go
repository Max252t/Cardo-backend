package retention_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/retention"
)

type fixedClock struct{ now time.Time }

func (c fixedClock) Now() time.Time { return c.now }

type fakeStore struct {
	mu        sync.Mutex
	eventsAt  []time.Time
	opsAt     []time.Time
	keepMin   int
	batches   []int
	events    retention.Result
	processed retention.Result
	eventsErr error
	calls     chan struct{}
}

func (f *fakeStore) PruneEvents(_ context.Context, olderThan time.Time, keepMin, batch int) (retention.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.eventsAt = append(f.eventsAt, olderThan)
	f.keepMin = keepMin
	f.batches = append(f.batches, batch)
	return f.events, f.eventsErr
}

func (f *fakeStore) PruneProcessedOps(_ context.Context, olderThan time.Time, batch int) (retention.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opsAt = append(f.opsAt, olderThan)
	f.batches = append(f.batches, batch)
	if f.calls != nil {
		select {
		case f.calls <- struct{}{}:
		default:
		}
	}
	return f.processed, nil
}

type metricsSpy struct {
	mu      sync.Mutex
	deleted map[string]int64
	runs    map[string]string
}

func (m *metricsSpy) Ran(table, result string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs == nil {
		m.runs = map[string]string{}
	}
	m.runs[table] = result
}

func (m *metricsSpy) Deleted(table string, n int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleted == nil {
		m.deleted = map[string]int64{}
	}
	m.deleted[table] += n
}

func validConfig() retention.Config {
	return retention.Config{
		Interval:              time.Hour,
		EventsRetention:       7 * 24 * time.Hour,
		EventsKeepMin:         1000,
		ProcessedOpsRetention: 24 * time.Hour,
		BatchSize:             500,
	}
}

func TestRunOncePassesCutoffsFromTheInjectedClock(t *testing.T) {
	now := time.Date(2030, 5, 10, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{events: retention.Result{Deleted: 12}, processed: retention.Result{Deleted: 3}}
	spy := &metricsSpy{}
	svc, err := retention.New(store, validConfig(), retention.WithClock(fixedClock{now: now}), retention.WithMetrics(spy))
	require.NoError(t, err)

	svc.RunOnce(context.Background())

	assert.Equal(t, []time.Time{now.Add(-7 * 24 * time.Hour)}, store.eventsAt)
	assert.Equal(t, []time.Time{now.Add(-24 * time.Hour)}, store.opsAt)
	assert.Equal(t, 1000, store.keepMin)
	assert.Equal(t, []int{500, 500}, store.batches)
	assert.Equal(t, map[string]int64{retention.TableBoardEvents: 12, retention.TableProcessedOps: 3}, spy.deleted)
}

func TestRunOnceContinuesAfterAJobFailure(t *testing.T) {
	store := &fakeStore{eventsErr: errors.New("db down"), processed: retention.Result{Deleted: 4}}
	spy := &metricsSpy{}
	svc, err := retention.New(store, validConfig(), retention.WithMetrics(spy))
	require.NoError(t, err)

	svc.RunOnce(context.Background())

	assert.Len(t, store.opsAt, 1)
	assert.EqualValues(t, 4, spy.deleted[retention.TableProcessedOps])
}

func TestRunStopsWhenContextIsCanceled(t *testing.T) {
	store := &fakeStore{calls: make(chan struct{}, 1)}
	svc, err := retention.New(store, validConfig())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Run(ctx)
		close(done)
	}()
	select {
	case <-store.calls:
	case <-time.After(5 * time.Second):
		t.Fatal("the first run did not start immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was canceled")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	mutations := map[string]func(*retention.Config){
		"interval":          func(c *retention.Config) { c.Interval = 0 },
		"events retention":  func(c *retention.Config) { c.EventsRetention = 0 },
		"ops retention":     func(c *retention.Config) { c.ProcessedOpsRetention = -time.Hour },
		"events shorter":    func(c *retention.Config) { c.EventsRetention = time.Hour },
		"negative keep min": func(c *retention.Config) { c.EventsKeepMin = -1 },
		"batch":             func(c *retention.Config) { c.BatchSize = 0 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			_, err := retention.New(&fakeStore{}, cfg)
			assert.Error(t, err)
		})
	}
	_, err := retention.New(nil, validConfig())
	assert.Error(t, err)
}

func TestRunOnceReportsRunResults(t *testing.T) {
	store := &fakeStore{eventsErr: errors.New("db down"), processed: retention.Result{Skipped: true}}
	spy := &metricsSpy{}
	svc, err := retention.New(store, validConfig(), retention.WithMetrics(spy))
	require.NoError(t, err)

	svc.RunOnce(context.Background())
	assert.Equal(t, retention.ResultFailed, spy.runs[retention.TableBoardEvents])
	assert.Equal(t, retention.ResultSkipped, spy.runs[retention.TableProcessedOps])

	store.eventsErr = nil
	store.processed = retention.Result{}
	svc.RunOnce(context.Background())
	assert.Equal(t, retention.ResultOK, spy.runs[retention.TableBoardEvents])
	assert.Equal(t, retention.ResultOK, spy.runs[retention.TableProcessedOps])
}

func TestConfigRejectsOversizedBatch(t *testing.T) {
	cfg := validConfig()
	cfg.BatchSize = retention.MaxBatchSize + 1
	assert.Error(t, cfg.Validate())
	cfg.BatchSize = retention.MaxBatchSize
	assert.NoError(t, cfg.Validate())
}
