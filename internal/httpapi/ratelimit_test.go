package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

type fakeLimiter struct {
	mu      sync.Mutex
	keys    []string
	allowed bool
	retry   time.Duration
	err     error
}

func (f *fakeLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, key)
	return f.allowed, f.retry, f.err
}

func (f *fakeLimiter) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.keys...)
}

type fakeRateMetrics struct {
	mu       sync.Mutex
	limited  int
	failures int
}

func (m *fakeRateMetrics) RateLimited() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.limited++
}

func (m *fakeRateMetrics) RateLimiterError() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures++
}

func newRateLimitedEnv(t *testing.T, limiter httpapi.RateLimiter, metrics httpapi.RateLimitMetrics) *env {
	t.Helper()
	e := newEnv(t)
	e.users.get = func(context.Context, uuid.UUID) (domain.User, error) { return sampleUser(), nil }
	e.auth.login = func(context.Context, auth.LoginInput) (auth.Session, error) {
		return auth.Session{}, domain.ErrInvalidCredential
	}
	r, err := httpapi.NewRouter(httpapi.Deps{
		Auth:        e.auth,
		Tokens:      e.tokens,
		Users:       e.users,
		Boards:      e.boards,
		RateLimiter: limiter,
		RateMetrics: metrics,
	})
	require.NoError(t, err)
	e.router = r
	return e
}

func TestRateLimitRejectsWith429AndRetryAfter(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		retry time.Duration
		want  string
	}{
		{"fraction rounds up", 10 * time.Millisecond, "1"},
		{"exact seconds", 2 * time.Second, "2"},
		{"ceil", 1500 * time.Millisecond, "2"},
		{"zero is at least one", 0, "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			limiter := &fakeLimiter{allowed: false, retry: tc.retry}
			metrics := &fakeRateMetrics{}
			e := newRateLimitedEnv(t, limiter, metrics)

			rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})

			assert.Equal(t, http.StatusTooManyRequests, rec.Code)
			assert.Equal(t, tc.want, rec.Header().Get("Retry-After"))
			code, _ := rec.errorBody(t)
			assert.Equal(t, httpapi.CodeRateLimited, code)
			assert.Equal(t, 1, metrics.limited)
		})
	}
}

func TestRateLimitIsKeyedByUserAndRunsAfterAuth(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{allowed: true}
	e := newRateLimitedEnv(t, limiter, nil)

	rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{testUserID.String()}, limiter.calls())

	rec = e.do(t, request{method: http.MethodGet, path: "/api/me", token: "bad"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = e.do(t, request{method: http.MethodGet, path: "/api/me"})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Len(t, limiter.calls(), 1, "unauthenticated requests never reach the user limiter")
}

func TestRateLimitSkipsPublicAndSystemRoutes(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{allowed: false, retry: time.Second}
	e := newRateLimitedEnv(t, limiter, nil)

	assert.Equal(t, http.StatusOK, e.do(t, request{method: http.MethodGet, path: "/healthz"}).Code)
	assert.Equal(t, http.StatusUnauthorized, e.do(t, request{method: http.MethodPost, path: "/api/auth/login", body: `{"email":"a@b.co","password":"x"}`}).Code)
	assert.Empty(t, limiter.calls())
}

func TestRateLimitFailsOpenWhenTheBackendFails(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{err: errors.New("redis down")}
	metrics := &fakeRateMetrics{}
	e := newRateLimitedEnv(t, limiter, metrics)

	for range 3 {
		rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Equal(t, 3, metrics.failures)
	assert.Zero(t, metrics.limited)
}

func TestRateLimitDisabledWithoutLimiter(t *testing.T) {
	t.Parallel()
	e := newRateLimitedEnv(t, nil, nil)
	for range 5 {
		assert.Equal(t, http.StatusOK, e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"}).Code)
	}
}

func TestRateLimitDoesNotCoverTheWebSocketRoute(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	sockets := &fakeSockets{
		authorize: func(_ context.Context, userID, _ uuid.UUID) (domain.Member, error) {
			return domain.Member{UserID: userID, Role: domain.RoleEditor}, nil
		},
		serve: func(w http.ResponseWriter, _ *http.Request, _ uuid.UUID, _ domain.Member, _ time.Time) error {
			w.WriteHeader(http.StatusAccepted)
			return nil
		},
	}
	limiter := &fakeLimiter{allowed: false, retry: time.Second}
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:   e.auth,
		Tokens: e.tokens,
		Users:  e.users,
		Boards: e.boards,
		Claims: fakeClaims{parse: func(string) (domain.AccessClaims, error) {
			return domain.AccessClaims{UserID: testUserID, ExpiresAt: time.Now().Add(time.Hour)}, nil
		}},
		Sockets:     sockets,
		RateLimiter: limiter,
	})
	require.NoError(t, err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, wsPath(), nil)
	req.Header.Set("Authorization", "Bearer good")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusAccepted, rec.Code)
	assert.Empty(t, limiter.calls())
}

func TestRateLimitDoesNotCountClientDisconnectsAsBackendFailures(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{err: context.Canceled}
	metrics := &fakeRateMetrics{}
	e := newRateLimitedEnv(t, limiter, metrics)

	rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, metrics.failures)
}
