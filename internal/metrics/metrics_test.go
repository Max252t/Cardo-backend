package metrics_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/metrics"
)

func scrape(t *testing.T, c *metrics.Collector) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestCollectorExposesRequiredMetrics(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	c.ConnectionOpened()
	c.ConnectionOpened()
	c.ConnectionClosed(1001)
	c.ConnectionClosed(4242)
	c.Resync("delta")
	c.ObserveOp("card.create", "applied", 5*time.Millisecond)
	c.ObserveOp("card.create", "version_conflict", time.Millisecond)
	c.ObserveRequest("GET", "/api/boards/:board_id", 200, time.Millisecond)
	c.ObserveRequest("BREW", "unmatched", 404, time.Millisecond)

	body := scrape(t, c)
	for _, want := range []string{
		"ws_connections 0",
		`ws_closed_total{code="1001"} 1`,
		`ws_closed_total{code="other"} 1`,
		`resync_total{kind="delta"} 1`,
		`ops_total{result="applied",type="card.create"} 1`,
		`ops_total{result="version_conflict",type="card.create"} 1`,
		`op_apply_duration_seconds_count{type="card.create"} 2`,
		`http_request_duration_seconds_count{method="GET",route="/api/boards/:board_id",status="200"} 1`,
		`http_request_duration_seconds_count{method="other",route="unmatched",status="404"} 1`,
		"go_goroutines",
		"event_publish_errors_total 0",
	} {
		assert.Contains(t, body, want)
	}
}

func TestInstrumentPublisherCountsFailures(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	boom := errors.New("boom")
	failing := c.InstrumentPublisher(failingPublisher{err: boom})
	require.ErrorIs(t, failing.Publish(context.Background(), nil), boom)
	require.NoError(t, c.InstrumentPublisher(failingPublisher{}).Publish(context.Background(), nil))

	assert.True(t, strings.Contains(scrape(t, c), "event_publish_errors_total 1"))
}

type failingPublisher struct{ err error }

func (f failingPublisher) Publish(context.Context, []domain.Event) error { return f.err }

func TestRetentionDeletedCountsByTable(t *testing.T) {
	t.Parallel()
	c := metrics.New()
	c.Deleted("board_events", 5)
	c.Deleted("board_events", 2)
	c.Deleted("processed_ops", 3)

	body := scrape(t, c)
	assert.Contains(t, body, `retention_deleted_total{table="board_events"} 7`)
	assert.Contains(t, body, `retention_deleted_total{table="processed_ops"} 3`)
}
