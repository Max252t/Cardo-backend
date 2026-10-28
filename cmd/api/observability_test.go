package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

type stubPublisher struct {
	err   error
	calls int
}

func (s *stubPublisher) Publish(context.Context, []domain.Event) error {
	s.calls++
	return s.err
}

func scrapeHandler(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestDisabledObservabilityProvidesNoRecordersAndNoHandler(t *testing.T) {
	t.Parallel()
	obs := newObservability(config.Config{MetricsEnabled: false})

	assert.Nil(t, obs.ops)
	assert.Nil(t, obs.realtime)
	assert.Nil(t, obs.http)
	assert.Nil(t, obs.handler)
	require.NotNil(t, obs.wrap)
}

func TestDisabledObservabilityLeavesThePublisherUntouched(t *testing.T) {
	t.Parallel()
	obs := newObservability(config.Config{MetricsEnabled: false})
	inner := &stubPublisher{err: errors.New("boom")}

	wrapped := obs.wrap(inner)

	assert.Same(t, inner, wrapped.(*stubPublisher))
	require.Error(t, wrapped.Publish(context.Background(), nil))
	assert.Equal(t, 1, inner.calls)
}

func TestEnabledObservabilityProvidesEveryRecorderAndHandler(t *testing.T) {
	t.Parallel()
	obs := newObservability(config.Config{MetricsEnabled: true})

	assert.NotNil(t, obs.ops)
	assert.NotNil(t, obs.realtime)
	assert.NotNil(t, obs.http)
	assert.NotNil(t, obs.handler)
	require.NotNil(t, obs.wrap)
}

func TestEnabledObservabilitySharesOneRegistryAcrossRecorders(t *testing.T) {
	t.Parallel()
	obs := newObservability(config.Config{MetricsEnabled: true})

	obs.ops.ObserveOp("op.card.create", "applied", time.Millisecond)
	obs.realtime.ConnectionOpened()
	obs.realtime.Resync("delta")
	obs.http.ObserveRequest(http.MethodGet, "/healthz", http.StatusOK, time.Millisecond)
	failing := obs.wrap(&stubPublisher{err: errors.New("boom")})
	require.Error(t, failing.Publish(context.Background(), nil))

	body := scrapeHandler(t, obs.handler)
	assert.Contains(t, body, `ops_total{result="applied",type="op.card.create"} 1`)
	assert.Contains(t, body, "ws_connections 1")
	assert.Contains(t, body, `resync_total{kind="delta"} 1`)
	assert.Contains(t, body, `http_request_duration_seconds_count{method="GET",route="/healthz",status="200"} 1`)
	assert.Contains(t, body, "event_publish_errors_total 1")
}

func TestEnabledObservabilityWrapsThePublisher(t *testing.T) {
	t.Parallel()
	obs := newObservability(config.Config{MetricsEnabled: true})
	inner := &stubPublisher{}

	wrapped := obs.wrap(inner)

	_, same := wrapped.(*stubPublisher)
	assert.False(t, same)
	require.NoError(t, wrapped.Publish(context.Background(), nil))
	assert.Equal(t, 1, inner.calls)
	assert.Contains(t, scrapeHandler(t, obs.handler), "event_publish_errors_total 0")
}

func TestEachObservabilityInstanceHasItsOwnRegistry(t *testing.T) {
	t.Parallel()
	a := newObservability(config.Config{MetricsEnabled: true})
	b := newObservability(config.Config{MetricsEnabled: true})

	a.realtime.ConnectionOpened()

	assert.Contains(t, scrapeHandler(t, a.handler), "ws_connections 1")
	assert.Contains(t, scrapeHandler(t, b.handler), "ws_connections 0")
}

type unusedAuth struct{ httpapi.AuthService }

type unusedTokens struct{ httpapi.TokenVerifier }

type unusedUsers struct{ httpapi.UserService }

type unusedBoards struct{ httpapi.BoardService }

func routerFor(t *testing.T, obs observability) http.Handler {
	t.Helper()
	r, err := httpapi.NewRouter(httpapi.Deps{
		Auth:           unusedAuth{},
		Tokens:         unusedTokens{},
		Users:          unusedUsers{},
		Boards:         unusedBoards{},
		Metrics:        obs.http,
		MetricsHandler: obs.handler,
	})
	require.NoError(t, err)
	return r
}

func TestMetricsRouteFollowsTheMetricsEnabledFlag(t *testing.T) {
	t.Parallel()
	enabled := routerFor(t, newObservability(config.Config{MetricsEnabled: true}))
	disabled := routerFor(t, newObservability(config.Config{MetricsEnabled: false}))

	rec := httptest.NewRecorder()
	enabled.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "go_goroutines")

	rec = httptest.NewRecorder()
	disabled.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRouterRecordsRequestsIntoTheEnabledRegistry(t *testing.T) {
	t.Parallel()
	obs := newObservability(config.Config{MetricsEnabled: true})
	router := routerFor(t, obs)

	for _, path := range []string{"/healthz", "/nowhere/secret"} {
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
	}

	body := scrapeHandler(t, obs.handler)
	assert.Contains(t, body, `http_request_duration_seconds_count{method="GET",route="/healthz",status="200"} 1`)
	assert.Contains(t, body, `http_request_duration_seconds_count{method="GET",route="unmatched",status="404"} 1`)
	assert.NotContains(t, body, "secret")
}

func TestDisabledRouterStillServesRequests(t *testing.T) {
	t.Parallel()
	router := routerFor(t, newObservability(config.Config{MetricsEnabled: false}))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
}

var _ boardevents.Publisher = (*stubPublisher)(nil)
