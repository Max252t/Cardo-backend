package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/metrics"
)

const inviteSecret = "S3cr3t-Inv1te-Tok3n"

type observedRequest struct {
	method   string
	route    string
	status   int
	duration time.Duration
}

type routeSpy struct {
	mu   sync.Mutex
	seen []observedRequest
}

func (s *routeSpy) ObserveRequest(method, route string, status int, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, observedRequest{method, route, status, d})
}

func (s *routeSpy) all() []observedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observedRequest(nil), s.seen...)
}

func (s *routeSpy) only(t *testing.T) observedRequest {
	t.Helper()
	all := s.all()
	require.Len(t, all, 1)
	return all[0]
}

type acceptingMembers struct {
	httpapi.MemberService
	err error
}

func (m acceptingMembers) Accept(context.Context, uuid.UUID, string) (domain.BoardWithRole, error) {
	return domain.BoardWithRole{}, m.err
}

type metricsHarness struct {
	router *gin.Engine
	spy    *routeSpy
	draing bool
}

func newMetricsHarness(t *testing.T, mutate func(*httpapi.Deps)) *metricsHarness {
	t.Helper()
	e := newEnv(t)
	h := &metricsHarness{spy: &routeSpy{}}
	e.users.get = func(context.Context, uuid.UUID) (domain.User, error) { return domain.User{ID: testUserID}, nil }
	deps := httpapi.Deps{
		Auth:         e.auth,
		Tokens:       e.tokens,
		Users:        e.users,
		Boards:       e.boards,
		Members:      acceptingMembers{},
		Metrics:      h.spy,
		ShuttingDown: func() bool { return h.draing },
		Claims: fakeClaims{parse: func(token string) (domain.AccessClaims, error) {
			if token == "good" {
				return domain.AccessClaims{UserID: testUserID, ExpiresAt: time.Now().Add(time.Hour)}, nil
			}
			return domain.AccessClaims{}, domain.ErrTokenInvalid
		}},
		Sockets: &fakeSockets{
			authorize: func(_ context.Context, userID, _ uuid.UUID) (domain.Member, error) {
				return domain.Member{UserID: userID, Role: domain.RoleEditor}, nil
			},
			serve: func(w http.ResponseWriter, _ *http.Request, _ uuid.UUID, _ domain.Member, _ time.Time) error {
				w.WriteHeader(http.StatusAccepted)
				return nil
			},
		},
	}
	if mutate != nil {
		mutate(&deps)
	}
	router, err := httpapi.NewRouter(deps)
	require.NoError(t, err)
	h.router = router
	return h
}

func (h *metricsHarness) do(t *testing.T, method, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

var authed = map[string]string{"Authorization": "Bearer good"}

func TestMetricsUseRouteTemplateForEveryParametrisedRoute(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	other := uuid.NewString()
	tests := []struct {
		method string
		path   string
		route  string
	}{
		{http.MethodGet, "/healthz", "/healthz"},
		{http.MethodGet, "/readyz", "/readyz"},
		{http.MethodGet, "/api/boards/" + id, "/api/boards/:id"},
		{http.MethodPatch, "/api/boards/" + id, "/api/boards/:id"},
		{http.MethodDelete, "/api/boards/" + id, "/api/boards/:id"},
		{http.MethodGet, "/api/boards/" + id + "/events", "/api/boards/:id/events"},
		{http.MethodGet, "/api/boards/" + id + "/members", "/api/boards/:id/members"},
		{http.MethodPatch, "/api/boards/" + id + "/members/" + other, "/api/boards/:id/members/:userId"},
		{http.MethodDelete, "/api/boards/" + id + "/members/" + other, "/api/boards/:id/members/:userId"},
		{http.MethodPost, "/api/boards/" + id + "/invites", "/api/boards/:id/invites"},
		{http.MethodDelete, "/api/boards/" + id + "/invites/" + other, "/api/boards/:id/invites/:inviteId"},
		{http.MethodPost, "/api/invites/" + inviteSecret + "/accept", "/api/invites/:token/accept"},
		{http.MethodGet, "/ws/boards/" + id, "/ws/boards/:id"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.route, func(t *testing.T) {
			t.Parallel()
			h := newMetricsHarness(t, nil)

			h.do(t, tt.method, tt.path, nil)

			got := h.spy.only(t)
			assert.Equal(t, tt.route, got.route)
			assert.Equal(t, tt.method, got.method)
			assert.NotContains(t, got.route, id)
			assert.NotContains(t, got.route, other)
			assert.NotContains(t, got.route, inviteSecret)
		})
	}
}

func TestMetricsNeverLeakInviteTokenInAnyLabel(t *testing.T) {
	t.Parallel()
	for _, headers := range []map[string]string{nil, authed} {
		h := newMetricsHarness(t, nil)

		rec := h.do(t, http.MethodPost, "/api/invites/"+inviteSecret+"/accept", headers)

		got := h.spy.only(t)
		assert.Equal(t, "/api/invites/:token/accept", got.route)
		assert.NotContains(t, got.method+got.route, inviteSecret)
		if headers != nil {
			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, http.StatusOK, got.status)
		} else {
			assert.Equal(t, http.StatusUnauthorized, got.status)
		}
	}
}

func TestMetricsInviteAcceptFailureStatusIsRecorded(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, func(d *httpapi.Deps) {
		d.Members = acceptingMembers{err: domain.ErrNotFound}
	})

	h.do(t, http.MethodPost, "/api/invites/"+inviteSecret+"/accept", authed)

	got := h.spy.only(t)
	assert.Equal(t, http.StatusNotFound, got.status)
	assert.Equal(t, "/api/invites/:token/accept", got.route)
}

func TestMetricsReportUnmatchedForUnknownPaths(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)
	paths := []string{
		"/", "/nowhere", "/api", "/api/", "/api/unknown/" + inviteSecret, "/api/boards/" + uuid.NewString() + "/unknown",
		"/ws/other/" + uuid.NewString(), "/admin/" + inviteSecret, "/healthz/extra", "/metricz",
	}
	for _, p := range paths {
		rec := h.do(t, http.MethodGet, p, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, p)
	}

	seen := h.spy.all()
	require.Len(t, seen, len(paths))
	for i, o := range seen {
		assert.Equal(t, "unmatched", o.route, paths[i])
		assert.Equal(t, http.StatusNotFound, o.status, paths[i])
	}
}

func TestMetricsReportUnmatchedForWrongMethodOnKnownPath(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	h.do(t, http.MethodDelete, "/healthz", nil)
	h.do(t, http.MethodPost, "/readyz", nil)

	for _, o := range h.spy.all() {
		assert.Equal(t, "unmatched", o.route)
		assert.Equal(t, http.StatusMethodNotAllowed, o.status)
	}
	assert.Len(t, h.spy.all(), 2)
}

func TestMetricsPassTheRawMethodForTheCollectorToBound(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	h.do(t, "BREW", "/healthz", nil)

	got := h.spy.only(t)
	assert.Equal(t, "BREW", got.method)
	assert.Equal(t, "unmatched", got.route)
}

func TestMetricsRecordStatusLabels(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, func(d *httpapi.Deps) {
		d.Readiness = map[string]httpapi.Checker{"db": failingChecker{}}
	})

	h.do(t, http.MethodGet, "/healthz", nil)
	h.do(t, http.MethodGet, "/readyz", nil)
	h.do(t, http.MethodGet, "/api/me", nil)
	h.do(t, http.MethodGet, "/api/me", authed)
	h.do(t, http.MethodGet, "/nowhere", nil)
	h.draing = true
	h.do(t, http.MethodGet, "/readyz", nil)

	statuses := make([]int, 0, 6)
	for _, o := range h.spy.all() {
		statuses = append(statuses, o.status)
	}
	assert.Equal(t, []int{
		http.StatusOK,
		http.StatusServiceUnavailable,
		http.StatusUnauthorized,
		http.StatusOK,
		http.StatusNotFound,
		http.StatusServiceUnavailable,
	}, statuses)
}

type failingChecker struct{}

func (failingChecker) Ping(context.Context) error { return errors.New("down") }

func TestMetricsRecordPanicAsServerError(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, func(d *httpapi.Deps) {
		d.Users = &fakeUsers{get: func(context.Context, uuid.UUID) (domain.User, error) { panic("boom") }}
	})

	rec := h.do(t, http.MethodGet, "/api/me", authed)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	got := h.spy.only(t)
	assert.Equal(t, http.StatusInternalServerError, got.status)
	assert.Equal(t, "/api/me", got.route)
}

func TestMetricsRecordBadRequestAndUnauthorizedAborts(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/auth/login", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)

	got := h.spy.only(t)
	assert.Equal(t, rec.Code, got.status)
	assert.GreaterOrEqual(t, got.status, 400)
	assert.Less(t, got.status, 500)
	assert.Equal(t, "/api/auth/login", got.route)
}

func TestMetricsObserveExactlyOncePerRequestWithNonNegativeDuration(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)
	for _, p := range []string{"/healthz", "/api/me", "/nowhere", "/api/boards/" + uuid.NewString()} {
		h.do(t, http.MethodGet, p, nil)
	}

	seen := h.spy.all()
	require.Len(t, seen, 4)
	for _, o := range seen {
		assert.GreaterOrEqual(t, o.duration, time.Duration(0))
	}
}

func TestMetricsCountConcurrentRequestsExactly(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)
	const n = 50

	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.router.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
		}()
	}
	wg.Wait()

	assert.Len(t, h.spy.all(), n)
}

func TestMetricsRecordUpgradeRequestsThatWereNotHijacked(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"websocket", "WebSocket", "WEBSOCKET"} {
		h := newMetricsHarness(t, nil)

		h.do(t, http.MethodGet, "/ws/boards/"+uuid.NewString(), map[string]string{
			"Upgrade":       value,
			"Connection":    "Upgrade",
			"Authorization": "Bearer good",
		})

		assert.Len(t, h.spy.all(), 1, value)
	}
}

func TestMetricsRecordWebSocketRouteWithoutUpgradeHeader(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	h.do(t, http.MethodGet, "/ws/boards/"+uuid.NewString(), authed)

	got := h.spy.only(t)
	assert.Equal(t, "/ws/boards/:id", got.route)
}

func TestMetricsOnlyUpgradeToWebSocketIsSkipped(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	h.do(t, http.MethodGet, "/healthz", map[string]string{"Upgrade": "h2c"})

	assert.Len(t, h.spy.all(), 1)
}

func TestMetricsSkipRealWebSocketSessions(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, func(d *httpapi.Deps) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		d.Sockets = &fakeSockets{
			authorize: func(_ context.Context, userID, _ uuid.UUID) (domain.Member, error) {
				return domain.Member{UserID: userID, Role: domain.RoleEditor}, nil
			},
			serve: func(w http.ResponseWriter, r *http.Request, _ uuid.UUID, _ domain.Member, _ time.Time) error {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return err
				}
				go func() {
					_, _, _ = conn.ReadMessage()
					_ = conn.Close()
				}()
				return nil
			},
		}
	})
	srv := httptest.NewServer(h.router)
	defer srv.Close()

	header := http.Header{"Authorization": []string{"Bearer good"}}
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/ws/boards/"+uuid.NewString(), header)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("x")))
	_ = conn.Close()

	healthReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/healthz", nil)
	require.NoError(t, err)
	httpResp, err := srv.Client().Do(healthReq)
	require.NoError(t, err)
	_ = httpResp.Body.Close()

	seen := h.spy.all()
	require.Len(t, seen, 1)
	assert.Equal(t, "/healthz", seen[0].route)
}

func TestRouterDoesNotServeMetrics(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	rec := h.do(t, http.MethodGet, "/metrics", nil)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "unmatched", h.spy.only(t).route)
}

func TestTrailingSlashIsNotRedirectedAndIsMeteredAsUnmatched(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	rec := h.do(t, http.MethodPost, "/api/invites/"+inviteSecret+"/accept/", authed)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Header().Get("Location"))
	got := h.spy.only(t)
	assert.Equal(t, "unmatched", got.route)
	assert.Equal(t, http.StatusNotFound, got.status)
}

func TestFailedWebSocketUpgradeIsMetered(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, nil)

	h.do(t, http.MethodGet, "/ws/boards/"+uuid.NewString(), map[string]string{"Upgrade": "websocket"})

	got := h.spy.only(t)
	assert.NotEqual(t, http.StatusOK, got.status)
}

func TestRouterWithoutMetricsDependencyServesRequests(t *testing.T) {
	t.Parallel()
	h := newMetricsHarness(t, func(d *httpapi.Deps) { d.Metrics = nil })

	assert.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/healthz", nil).Code)
	assert.Equal(t, http.StatusNotFound, h.do(t, http.MethodGet, "/nowhere", nil).Code)
}

func TestPrometheusCollectorBehindTheRouter(t *testing.T) {
	t.Parallel()
	collector := metrics.New()
	h := newMetricsHarness(t, func(d *httpapi.Deps) {
		d.Metrics = collector
	})

	h.do(t, http.MethodGet, "/healthz", nil)
	h.do(t, http.MethodPost, "/api/invites/"+inviteSecret+"/accept", authed)
	h.do(t, http.MethodGet, "/api/boards/"+uuid.NewString()+"/events", nil)
	h.do(t, "BREW", "/secret/"+inviteSecret, nil)
	h.do(t, http.MethodGet, "/ws/boards/"+uuid.NewString(), map[string]string{"Upgrade": "websocket", "Authorization": "Bearer good"})
	rec := httptest.NewRecorder()
	collector.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`http_request_duration_seconds_count{method="GET",route="/healthz",status="200"} 1`,
		`http_request_duration_seconds_count{method="POST",route="/api/invites/:token/accept",status="200"} 1`,
		`http_request_duration_seconds_count{method="GET",route="/api/boards/:id/events",status="401"} 1`,
		`http_request_duration_seconds_count{method="other",route="unmatched",status="404"} 1`,
	} {
		assert.Contains(t, body, want)
	}
	assert.NotContains(t, body, inviteSecret)
	assert.Contains(t, body, `route="/ws/boards/:id"`)
}
