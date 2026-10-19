package httpapi_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

func TestNewRouterRequiresDependencies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	full := httpapi.Deps{Auth: e.auth, Tokens: e.tokens, Users: e.users, Boards: e.boards}

	tests := []struct {
		name string
		mut  func(*httpapi.Deps)
	}{
		{"no auth", func(d *httpapi.Deps) { d.Auth = nil }},
		{"no tokens", func(d *httpapi.Deps) { d.Tokens = nil }},
		{"no users", func(d *httpapi.Deps) { d.Users = nil }},
		{"no boards", func(d *httpapi.Deps) { d.Boards = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := full
			tt.mut(&d)
			r, err := httpapi.NewRouter(d)
			assert.Error(t, err)
			assert.Nil(t, r)
		})
	}

	r, err := httpapi.NewRouter(full)
	require.NoError(t, err)
	assert.NotNil(t, r)
}

func TestNewRouterRejectsInvalidTrustedProxies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	_, err := httpapi.NewRouter(httpapi.Deps{
		Auth: e.auth, Tokens: e.tokens, Users: e.users, Boards: e.boards,
		TrustedProxies: []string{"not-an-ip"},
	})
	assert.Error(t, err)

	_, err = httpapi.NewRouter(httpapi.Deps{
		Auth: e.auth, Tokens: e.tokens, Users: e.users, Boards: e.boards,
		TrustedProxies: []string{"10.0.0.0/8", "192.168.1.1"},
	})
	assert.NoError(t, err)
}

func TestHealthz(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	rec := e.do(t, request{method: http.MethodGet, path: "/healthz"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ok", rec.json(t)["status"])
}

func TestReadyz(t *testing.T) {
	t.Parallel()
	ok := httpapi.CheckerFunc(func(context.Context) error { return nil })
	bad := httpapi.CheckerFunc(func(context.Context) error { return errors.New("connection refused to 10.0.0.5") })

	tests := []struct {
		name       string
		checks     map[string]httpapi.Checker
		wantStatus int
		wantChecks map[string]string
		wantMsg    string
	}{
		{
			name:       "no checkers",
			checks:     nil,
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{},
		},
		{
			name:       "all healthy",
			checks:     map[string]httpapi.Checker{"postgres": ok, "redis": ok},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"postgres": "ok", "redis": "ok"},
		},
		{
			name:       "one failing",
			checks:     map[string]httpapi.Checker{"postgres": ok, "redis": bad},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"postgres": "ok", "redis": "unavailable"},
			wantMsg:    "redis",
		},
		{
			name:       "all failing",
			checks:     map[string]httpapi.Checker{"postgres": bad, "redis": bad},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"postgres": "unavailable", "redis": "unavailable"},
			wantMsg:    "postgres, redis",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.readiness = tt.checks
			rec := e.do(t, request{method: http.MethodGet, path: "/readyz"})
			assert.Equal(t, tt.wantStatus, rec.Code)

			body := rec.json(t)
			checks, _ := body["checks"].(map[string]any)
			require.Len(t, checks, len(tt.wantChecks))
			for k, v := range tt.wantChecks {
				assert.Equal(t, v, checks[k])
			}
			if tt.wantStatus == http.StatusOK {
				assert.Equal(t, "ok", body["status"])
				return
			}
			code, msg := rec.errorBody(t)
			assert.Equal(t, httpapi.CodeNotReady, code)
			assert.Contains(t, msg, tt.wantMsg)
			assert.NotContains(t, rec.Body.String(), "10.0.0.5", "internal error details must not leak")
		})
	}
}

func TestUnknownRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, path := range []string{"/", "/api", "/api/nope", "/api/boards/x/y/z", "/healthz/extra"} {
		rec := e.do(t, request{method: http.MethodGet, path: path, token: "good"})
		assert.Equal(t, http.StatusNotFound, rec.Code, path)
		code, _ := rec.errorBody(t)
		assert.Equal(t, httpapi.CodeNotFound, code, path)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/auth/login"},
		{http.MethodPut, "/api/auth/register"},
		{http.MethodDelete, "/api/auth/refresh"},
		{http.MethodPost, "/healthz"},
		{http.MethodPost, "/readyz"},
		{http.MethodPut, "/api/me"},
		{http.MethodDelete, "/api/me"},
		{http.MethodPut, "/api/boards"},
		{http.MethodGet, "/api/boards/22222222-2222-7222-8222-222222222222"},
		{http.MethodPost, "/api/boards/22222222-2222-7222-8222-222222222222"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			rec := e.do(t, request{method: tt.method, path: tt.path, token: "good"})
			assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, httpapi.CodeMethodNotAllowed, code)
		})
	}
}

func TestRequestIDHeader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)

	generated := e.do(t, request{method: http.MethodGet, path: "/healthz"}).Header().Get("X-Request-ID")
	_, err := uuid.Parse(generated)
	assert.NoError(t, err, "generated id should be a UUID, got %q", generated)

	echoed := e.do(t, request{method: http.MethodGet, path: "/healthz", headers: map[string]string{"X-Request-ID": "client-id_1.2"}})
	assert.Equal(t, "client-id_1.2", echoed.Header().Get("X-Request-ID"))

	for _, bad := range []string{"has space", "new\x7fline", strings.Repeat("a", 65), "semi;colon", "<script>"} {
		rec := e.do(t, request{method: http.MethodGet, path: "/healthz", headers: map[string]string{"X-Request-ID": bad}})
		got := rec.Header().Get("X-Request-ID")
		assert.NotEqual(t, bad, got)
		_, err := uuid.Parse(got)
		assert.NoError(t, err)
	}

	notFound := e.do(t, request{method: http.MethodGet, path: "/nope"})
	assert.NotEmpty(t, notFound.Header().Get("X-Request-ID"))
}

func TestPanicIsRecoveredAsInternalError(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.users.get = func(context.Context, uuid.UUID) (domain.User, error) {
		panic("kaboom secret-detail")
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	code, _ := rec.errorBody(t)
	assert.Equal(t, httpapi.CodeInternal, code)
	assert.NotContains(t, rec.Body.String(), "secret-detail")

	e.users.get = func(context.Context, uuid.UUID) (domain.User, error) { return sampleUser(), nil }
	rec = e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})
	assert.Equal(t, http.StatusOK, rec.Code, "router must keep serving after a panic")
}

func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.users.get = func(context.Context, uuid.UUID) (domain.User, error) {
		return domain.User{}, errors.New("pq: password authentication failed for user cardo")
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	code, msg := rec.errorBody(t)
	assert.Equal(t, httpapi.CodeInternal, code)
	assert.Equal(t, "internal server error", msg)
	assert.NotContains(t, rec.Body.String(), "pq:")
}

func TestLoggerDoesNotLogAuthorizationOrBodies(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := newEnv(t)
	e.users.get = func(context.Context, uuid.UUID) (domain.User, error) { return sampleUser(), nil }
	e.auth.login = func(context.Context, auth.LoginInput) (auth.Session, error) {
		return auth.Session{}, domain.ErrInvalidCredential
	}
	r, err := httpapi.NewRouter(httpapi.Deps{Logger: logger, Auth: e.auth, Tokens: e.tokens, Users: e.users, Boards: e.boards})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@b.co","password":"hunter2hunter2"}`))
	req.RemoteAddr = "192.0.2.10:1"
	r.ServeHTTP(httptest.NewRecorder(), req)

	req = httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/me", nil)
	req.RemoteAddr = "192.0.2.10:1"
	req.Header.Set("Authorization", "Bearer good")
	r.ServeHTTP(httptest.NewRecorder(), req)

	logs := buf.String()
	assert.Contains(t, logs, "http request")
	assert.NotContains(t, logs, "hunter2hunter2")
	assert.NotContains(t, logs, "Bearer good")
}

func TestReadyzReportsShutdownBeforeChecks(t *testing.T) {
	t.Parallel()
	var draining atomic.Bool
	pinged := false
	e := newEnv(t)
	e.readiness = map[string]httpapi.Checker{"postgres": httpapi.CheckerFunc(func(context.Context) error {
		pinged = true
		return nil
	})}
	e.shuttingDown = draining.Load

	rec := e.do(t, request{method: http.MethodGet, path: "/readyz"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, pinged)

	pinged = false
	draining.Store(true)
	rec = e.do(t, request{method: http.MethodGet, path: "/readyz"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	code, _ := rec.errorBody(t)
	assert.Equal(t, httpapi.CodeShuttingDown, code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
	assert.False(t, pinged, "dependency checks are skipped while draining")

	rec = e.do(t, request{method: http.MethodGet, path: "/healthz"})
	assert.Equal(t, http.StatusOK, rec.Code, "liveness stays green while draining")
}
