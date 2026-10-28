package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/metrics"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func sample(t *testing.T, c *metrics.Collector, series string) float64 {
	t.Helper()
	for _, line := range strings.Split(scrape(t, c), "\n") {
		if !strings.HasPrefix(line, series+" ") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, series+" ")), 64)
		require.NoError(t, err)
		return v
	}
	t.Fatalf("series %q not found", series)
	return 0
}

func hasSeries(t *testing.T, c *metrics.Collector, series string) bool {
	t.Helper()
	for _, line := range strings.Split(scrape(t, c), "\n") {
		if strings.HasPrefix(line, series+" ") {
			return true
		}
	}
	return false
}

func countSeries(t *testing.T, c *metrics.Collector, prefix string) int {
	t.Helper()
	n := 0
	for _, line := range strings.Split(scrape(t, c), "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

func TestMetricTypesAndNames(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	c.ConnectionOpened()
	c.ConnectionClosed(protocol.CloseNormal)
	c.Resync("delta")
	c.ObserveOp("card.create", "applied", time.Millisecond)
	c.ObserveRequest("GET", "/healthz", 200, time.Millisecond)

	body := scrape(t, c)
	for _, want := range []string{
		"# TYPE ws_connections gauge",
		"# TYPE ws_closed_total counter",
		"# TYPE resync_total counter",
		"# TYPE ops_total counter",
		"# TYPE op_apply_duration_seconds histogram",
		"# TYPE event_publish_errors_total counter",
		"# TYPE http_request_duration_seconds histogram",
	} {
		assert.Contains(t, body, want)
	}
}

func TestUnlabelledMetricsStartAtZero(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	assert.Equal(t, 0.0, sample(t, c, "ws_connections"))
	assert.Equal(t, 0.0, sample(t, c, "event_publish_errors_total"))
	assert.Equal(t, 0.0, sample(t, c, "op_apply_duration_seconds_count"))
	assert.False(t, hasSeries(t, c, `ws_closed_total{code="1001"}`))
}

func TestConnectionGaugeGoesUpAndDown(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	for range 3 {
		c.ConnectionOpened()
	}
	assert.Equal(t, 3.0, sample(t, c, "ws_connections"))
	c.ConnectionClosed(protocol.CloseGoingAway)
	assert.Equal(t, 2.0, sample(t, c, "ws_connections"))
	c.ConnectionClosed(12345)
	c.ConnectionClosed(protocol.CloseNormal)
	assert.Equal(t, 0.0, sample(t, c, "ws_connections"))
	c.ConnectionOpened()
	assert.Equal(t, 1.0, sample(t, c, "ws_connections"))
}

func TestKnownCloseCodesKeepTheirOwnLabel(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	known := []int{
		protocol.CloseNormal, protocol.CloseGoingAway, 1006, 1009, 1011,
		protocol.CloseTryAgainLater, protocol.CloseInvalidToken, protocol.CloseAccessRevoked,
		protocol.CloseBoardDeleted, protocol.CloseRateLimited, protocol.CloseSlowClient, protocol.CloseProtocolError,
	}
	for _, code := range known {
		c.ConnectionOpened()
		c.ConnectionClosed(code)
	}
	for _, code := range known {
		assert.Equal(t, 1.0, sample(t, c, fmt.Sprintf(`ws_closed_total{code="%d"}`, code)), code)
	}
	assert.False(t, hasSeries(t, c, `ws_closed_total{code="other"}`))
}

func TestUnknownCloseCodesCollapseIntoOther(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	unknown := []int{0, -1, 1, 999, 1002, 1005, 3000, 4000, 4002, 4401, 4999, 5000, 65535}
	for _, code := range unknown {
		c.ConnectionOpened()
		c.ConnectionClosed(code)
	}
	assert.Equal(t, float64(len(unknown)), sample(t, c, `ws_closed_total{code="other"}`))
	assert.Equal(t, 1, countSeries(t, c, "ws_closed_total{"))
}

func TestCloseCodeCardinalityIsBounded(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	for code := -50; code < 6000; code++ {
		c.ConnectionOpened()
		c.ConnectionClosed(code)
	}
	assert.LessOrEqual(t, countSeries(t, c, "ws_closed_total{"), 13)
	assert.Equal(t, 0.0, sample(t, c, "ws_connections"))
}

func TestObserveOpCountsByTypeAndResult(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	c.ObserveOp("card.create", "applied", time.Millisecond)
	c.ObserveOp("card.create", "applied", time.Millisecond)
	c.ObserveOp("card.create", "version_conflict", time.Millisecond)
	c.ObserveOp("column.rename", "applied", time.Millisecond)
	c.ObserveOp("unknown", "validation", time.Millisecond)

	assert.Equal(t, 2.0, sample(t, c, `ops_total{result="applied",type="card.create"}`))
	assert.Equal(t, 1.0, sample(t, c, `ops_total{result="version_conflict",type="card.create"}`))
	assert.Equal(t, 1.0, sample(t, c, `ops_total{result="applied",type="column.rename"}`))
	assert.Equal(t, 1.0, sample(t, c, `ops_total{result="validation",type="unknown"}`))
	assert.Equal(t, 4, countSeries(t, c, "ops_total{"))
}

func TestObserveOpFeedsDurationHistogram(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	c.ObserveOp("card.create", "applied", 3*time.Millisecond)
	c.ObserveOp("card.move", "not_found", 40*time.Millisecond)
	c.ObserveOp("card.move", "applied", 2*time.Second)

	assert.Equal(t, 3.0, sample(t, c, "op_apply_duration_seconds_count"))
	assert.InDelta(t, 2.043, sample(t, c, "op_apply_duration_seconds_sum"), 1e-9)
	assert.Equal(t, 1.0, sample(t, c, `op_apply_duration_seconds_bucket{le="0.005"}`))
	assert.Equal(t, 2.0, sample(t, c, `op_apply_duration_seconds_bucket{le="0.05"}`))
	assert.Equal(t, 2.0, sample(t, c, `op_apply_duration_seconds_bucket{le="1"}`))
	assert.Equal(t, 3.0, sample(t, c, `op_apply_duration_seconds_bucket{le="+Inf"}`))
}

func TestResyncCountsByKind(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	c.Resync("delta")
	c.Resync("delta")
	c.Resync("snapshot")

	assert.Equal(t, 2.0, sample(t, c, `resync_total{kind="delta"}`))
	assert.Equal(t, 1.0, sample(t, c, `resync_total{kind="snapshot"}`))
}

func TestObserveRequestLabelsMethodRouteAndStatus(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	c.ObserveRequest("POST", "/api/invites/:token/accept", 200, 10*time.Millisecond)
	c.ObserveRequest("POST", "/api/invites/:token/accept", 404, 10*time.Millisecond)
	c.ObserveRequest("GET", "unmatched", 404, 10*time.Millisecond)

	assert.Equal(t, 1.0, sample(t, c, `http_request_duration_seconds_count{method="POST",route="/api/invites/:token/accept",status="200"}`))
	assert.Equal(t, 1.0, sample(t, c, `http_request_duration_seconds_count{method="POST",route="/api/invites/:token/accept",status="404"}`))
	assert.Equal(t, 1.0, sample(t, c, `http_request_duration_seconds_count{method="GET",route="unmatched",status="404"}`))
	assert.InDelta(t, 0.01, sample(t, c, `http_request_duration_seconds_sum{method="GET",route="unmatched",status="404"}`), 1e-9)
}

func TestKnownHTTPMethodsKeepTheirLabel(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	for _, m := range []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		c.ObserveRequest(m, "/x", 200, time.Millisecond)
		assert.Equal(t, 1.0, sample(t, c, fmt.Sprintf(`http_request_duration_seconds_count{method="%s",route="/x",status="200"}`, m)), m)
	}
}

func TestUnknownHTTPMethodsCollapseIntoOther(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	for _, m := range []string{"BREW", "get", "Post", "", "CONNECT", "TRACE", "PROPFIND", "X-ATTACKER-1", "X-ATTACKER-2"} {
		c.ObserveRequest(m, "unmatched", 404, time.Millisecond)
	}
	assert.Equal(t, 9.0, sample(t, c, `http_request_duration_seconds_count{method="other",route="unmatched",status="404"}`))
	assert.Equal(t, 1, countSeries(t, c, "http_request_duration_seconds_count{"))
}

func TestRegistriesAreIsolatedBetweenCollectors(t *testing.T) {
	t.Parallel()
	a := metrics.New()
	b := metrics.New()
	a.ConnectionOpened()
	a.Resync("delta")
	a.ObserveOp("card.create", "applied", time.Millisecond)
	_ = a.InstrumentPublisher(&countingPublisher{err: errors.New("x")}).Publish(context.Background(), nil)

	assert.Equal(t, 1.0, sample(t, a, "ws_connections"))
	assert.Equal(t, 1.0, sample(t, a, "event_publish_errors_total"))
	assert.Equal(t, 0.0, sample(t, b, "ws_connections"))
	assert.Equal(t, 0.0, sample(t, b, "event_publish_errors_total"))
	assert.False(t, hasSeries(t, b, `resync_total{kind="delta"}`))
	assert.False(t, hasSeries(t, b, `ops_total{result="applied",type="card.create"}`))
}

func TestManyCollectorsCanCoexist(t *testing.T) {
	t.Parallel()
	assert.NotPanics(t, func() {
		for range 5 {
			metrics.New().ConnectionOpened()
		}
	})
}

func TestScrapeIncludesRuntimeMetrics(t *testing.T) {
	t.Parallel()
	body := scrape(t, metrics.New())
	assert.Contains(t, body, "go_goroutines")
	assert.Contains(t, body, "go_memstats_alloc_bytes")
}

func TestHandlerServesPrometheusTextFormat(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	metrics.New().Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	assert.Contains(t, rec.Body.String(), "# HELP ws_connections")
}

func TestConcurrentUpdatesAreExact(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	const workers, each = 16, 200
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				c.ConnectionOpened()
				c.ObserveOp("card.create", "applied", time.Microsecond)
				c.ObserveRequest("GET", "/x", 200, time.Microsecond)
				c.Resync("delta")
				c.ConnectionClosed(protocol.CloseGoingAway)
			}
		}()
	}
	wg.Wait()

	total := float64(workers * each)
	assert.Equal(t, 0.0, sample(t, c, "ws_connections"))
	assert.Equal(t, total, sample(t, c, `ws_closed_total{code="1001"}`))
	assert.Equal(t, total, sample(t, c, `ops_total{result="applied",type="card.create"}`))
	assert.Equal(t, total, sample(t, c, "op_apply_duration_seconds_count"))
	assert.Equal(t, total, sample(t, c, `resync_total{kind="delta"}`))
	assert.Equal(t, total, sample(t, c, `http_request_duration_seconds_count{method="GET",route="/x",status="200"}`))
}

type countingPublisher struct {
	mu     sync.Mutex
	calls  int
	events []domain.Event
	ctx    context.Context
	err    error
}

func (p *countingPublisher) Publish(ctx context.Context, events []domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.events = events
	p.ctx = ctx
	return p.err
}

type ctxKey struct{}

func TestInstrumentPublisherPassesEventsAndContextThrough(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	inner := &countingPublisher{}
	pub := c.InstrumentPublisher(inner)
	events := []domain.Event{{Seq: 1, Type: "card.created"}, {Seq: 2, Type: "card.moved"}}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	require.NoError(t, pub.Publish(ctx, events))

	assert.Equal(t, 1, inner.calls)
	assert.Equal(t, events, inner.events)
	assert.Equal(t, "marker", inner.ctx.Value(ctxKey{}))
	assert.Equal(t, 0.0, sample(t, c, "event_publish_errors_total"))
}

func TestInstrumentPublisherReturnsTheOriginalError(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	boom := errors.New("redis down")
	pub := c.InstrumentPublisher(&countingPublisher{err: boom})

	err := pub.Publish(context.Background(), []domain.Event{{Seq: 1}})

	assert.Same(t, boom, err)
	assert.Equal(t, 1.0, sample(t, c, "event_publish_errors_total"))
}

func TestInstrumentPublisherCountsOnlyFailures(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	ok := c.InstrumentPublisher(&countingPublisher{})
	bad := c.InstrumentPublisher(&countingPublisher{err: errors.New("x")})

	for range 5 {
		require.NoError(t, ok.Publish(context.Background(), []domain.Event{{Seq: 1}}))
	}
	for range 3 {
		require.Error(t, bad.Publish(context.Background(), nil))
	}

	assert.Equal(t, 3.0, sample(t, c, "event_publish_errors_total"))
}

func TestInstrumentPublisherCountsContextErrors(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	pub := c.InstrumentPublisher(&countingPublisher{err: context.Canceled})

	err := pub.Publish(context.Background(), nil)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1.0, sample(t, c, "event_publish_errors_total"))
}
