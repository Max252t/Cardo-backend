package httpapi_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/ratelimit"
)

var (
	aliceID = uuid.MustParse("aaaaaaaa-aaaa-7aaa-8aaa-aaaaaaaaaaaa")
	bobID   = uuid.MustParse("bbbbbbbb-bbbb-7bbb-8bbb-bbbbbbbbbbbb")
)

type realLimiterEnv struct {
	*env
	clock   *flowClock
	limiter *ratelimit.MemoryRESTLimiter
	metrics *fakeRateMetrics
	logs    *syncBuffer
	listed  atomic.Int32
}

func newRealLimiterEnv(t *testing.T, perSecond float64, burst int) *realLimiterEnv {
	t.Helper()
	clock := &flowClock{now: time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)}
	limiter, err := ratelimit.NewMemoryRESTLimiter(perSecond, burst, clock)
	require.NoError(t, err)
	return buildRealLimiterEnv(t, limiter, clock, limiter)
}

func buildRealLimiterEnv(t *testing.T, limiter httpapi.RateLimiter, clock *flowClock, memory *ratelimit.MemoryRESTLimiter) *realLimiterEnv {
	t.Helper()
	e := newEnv(t)
	r := &realLimiterEnv{env: e, clock: clock, limiter: memory, metrics: &fakeRateMetrics{}, logs: &syncBuffer{}}
	e.tokens.verify = func(token string) (uuid.UUID, error) {
		switch token {
		case "alice":
			return aliceID, nil
		case "bob":
			return bobID, nil
		}
		return uuid.Nil, domain.ErrTokenInvalid
	}
	e.users.get = func(_ context.Context, id uuid.UUID) (domain.User, error) {
		u := sampleUser()
		u.ID = id
		return u, nil
	}
	e.boards.list = func(context.Context, uuid.UUID) ([]domain.BoardWithRole, error) {
		r.listed.Add(1)
		return nil, nil
	}
	e.auth.login = func(context.Context, auth.LoginInput) (auth.Session, error) {
		return auth.Session{}, domain.ErrInvalidCredential
	}
	e.auth.register = func(context.Context, auth.RegisterInput) (auth.Session, error) {
		return auth.Session{}, domain.ErrEmailTaken
	}
	e.auth.refresh = func(context.Context, string) (auth.TokenPair, error) {
		return auth.TokenPair{}, domain.ErrTokenInvalid
	}
	e.auth.logout = func(context.Context, uuid.UUID, string) error { return nil }
	e.readiness = map[string]httpapi.Checker{"ok": httpapi.CheckerFunc(func(context.Context) error { return nil })}
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:        e.auth,
		Tokens:      e.tokens,
		Users:       e.users,
		Boards:      e.boards,
		Readiness:   e.readiness,
		Clock:       clock,
		Logger:      slog.New(slog.NewJSONHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		RateLimiter: limiter,
		RateMetrics: r.metrics,
	})
	require.NoError(t, err)
	e.router = router
	return r
}

func (r *realLimiterEnv) get(t *testing.T, path, token string) response {
	t.Helper()
	return r.do(t, request{method: http.MethodGet, path: path, token: token})
}

func TestRealLimiterIsolatesUsers(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.001, 2)

	assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)
	assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/boards", "alice").Code)

	assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "bob").Code)
	assert.Equal(t, http.StatusOK, e.get(t, "/api/boards", "bob").Code)
	assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "bob").Code)
	assert.Equal(t, 2, e.limiter.Size())
}

func TestRealLimiterSharesOneBucketAcrossRoutesAndMethods(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.001, 3)

	assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	assert.Equal(t, http.StatusOK, e.get(t, "/api/boards", "alice").Code)
	assert.Less(t, e.do(t, request{method: http.MethodPost, path: "/api/auth/logout", token: "alice", body: `{"refresh_token":"x"}`}).Code, 300, "logout shares the bucket and is not rejected by it")
	assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)
	assert.Equal(t, http.StatusTooManyRequests, e.do(t, request{method: http.MethodPatch, path: "/api/me", token: "alice", body: `{"display_name":"A"}`}).Code)
}

func TestRealLimiterRejectedRequestsNeverReachHandlers(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.001, 1)

	require.Equal(t, http.StatusOK, e.get(t, "/api/boards", "alice").Code)
	for range 5 {
		assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/boards", "alice").Code)
	}
	assert.EqualValues(t, 1, e.listed.Load())
	e.metrics.mu.Lock()
	defer e.metrics.mu.Unlock()
	assert.Equal(t, 5, e.metrics.limited)
	assert.Zero(t, e.metrics.failures)
}

func TestRealLimiterRetryAfterTracksTheBucket(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.1, 1)

	require.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	rec := e.get(t, "/api/me", "alice")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "10", rec.Header().Get("Retry-After"))

	e.clock.Advance(4 * time.Second)
	rec = e.get(t, "/api/me", "alice")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "6", rec.Header().Get("Retry-After"))

	e.clock.Advance(1500 * time.Millisecond)
	rec = e.get(t, "/api/me", "alice")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "5", rec.Header().Get("Retry-After"), "4.5 seconds round up")

	e.clock.Advance(4500 * time.Millisecond)
	assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code, "the advertised wait is enough")
}

func TestRealLimiterRetryAfterOfFastRateIsOneSecond(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 1000, 1)

	require.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	rec := e.get(t, "/api/me", "alice")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"))
}

func TestRealLimiterErrorBodyUsesTheUnifiedShape(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.001, 1)
	require.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)

	rec := e.get(t, "/api/me", "alice")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	code, message := rec.errorBody(t)
	assert.Equal(t, httpapi.CodeRateLimited, code)
	assert.NotEmpty(t, message)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	body := rec.json(t)
	assert.Len(t, body, 1)
	assert.Len(t, body["error"], 2)
	assert.NotContains(t, rec.Body.String(), aliceID.String())
}

func TestRealLimiterNeverLimitsUnauthenticatedRoutes(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.001, 1)
	require.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	require.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)

	for range 10 {
		assert.Equal(t, http.StatusOK, e.get(t, "/healthz", "").Code)
		assert.Equal(t, http.StatusOK, e.get(t, "/readyz", "").Code)
		assert.Equal(t, http.StatusUnauthorized, e.do(t, request{method: http.MethodPost, path: "/api/auth/login", body: `{"email":"a@b.co","password":"x"}`}).Code)
		assert.Equal(t, http.StatusConflict, e.do(t, request{method: http.MethodPost, path: "/api/auth/register", body: `{"email":"a@b.co","password":"correct-horse","display_name":"A"}`}).Code)
		assert.Equal(t, http.StatusUnauthorized, e.do(t, request{method: http.MethodPost, path: "/api/auth/refresh", body: `{"refresh_token":"x"}`}).Code)
	}
	assert.Equal(t, 1, e.limiter.Size())
}

func TestRealLimiterAuthenticationHappensBeforeTheLimiter(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.001, 1)
	require.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	require.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)

	for range 5 {
		rec := e.get(t, "/api/me", "forged")
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
		code, _ := rec.errorBody(t)
		assert.Equal(t, httpapi.CodeInvalidToken, code)
		assert.Empty(t, rec.Header().Get("Retry-After"))
		rec = e.get(t, "/api/me", "")
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	assert.Equal(t, 1, e.limiter.Size(), "rejected credentials never create limiter state")
	e.metrics.mu.Lock()
	defer e.metrics.mu.Unlock()
	assert.Equal(t, 1, e.metrics.limited)
}

func TestRealLimiterRecoversAfterTheWindow(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 2, 2)

	for range 2 {
		require.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	}
	require.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)

	e.clock.Advance(500 * time.Millisecond)
	assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)

	e.clock.Advance(time.Hour)
	for range 2 {
		assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	}
	assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)
}

func TestRealLimiterConcurrentRequestsOfOneUserAreCountedExactly(t *testing.T) {
	t.Parallel()
	const burst = 15
	e := newRealLimiterEnv(t, 0.001, burst)
	var ok, limited, other atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 80 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			switch e.get(t, "/api/me", "alice").Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.EqualValues(t, burst, ok.Load())
	assert.EqualValues(t, 80-burst, limited.Load())
	assert.Zero(t, other.Load())
}

func TestRealLimiterOneUserDoesNotStarveAnotherUnderLoad(t *testing.T) {
	t.Parallel()
	e := newRealLimiterEnv(t, 0.001, 5)

	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.get(t, "/api/me", "alice")
		}()
	}
	wg.Wait()

	for range 5 {
		assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "bob").Code)
	}
}

type flakyLimiter struct {
	fail  atomic.Bool
	calls atomic.Int32
}

func (f *flakyLimiter) Allow(context.Context, string) (bool, time.Duration, error) {
	f.calls.Add(1)
	if f.fail.Load() {
		return false, 0, errors.New("backend unreachable")
	}
	return true, 0, nil
}

func warnCount(logs *syncBuffer) int {
	return strings.Count(logs.String(), "rest rate limiter unavailable")
}

func TestLimiterFailureWarningIsThrottled(t *testing.T) {
	t.Parallel()
	clock := &flowClock{now: time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)}
	limiter := &flakyLimiter{}
	limiter.fail.Store(true)
	e := buildRealLimiterEnv(t, limiter, clock, nil)

	for range 20 {
		assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	}
	assert.Equal(t, 1, warnCount(e.logs), "a burst of failures logs once")

	clock.Advance(29 * time.Second)
	e.get(t, "/api/me", "alice")
	assert.Equal(t, 1, warnCount(e.logs))

	clock.Advance(2 * time.Second)
	e.get(t, "/api/me", "alice")
	assert.Equal(t, 2, warnCount(e.logs), "another warning after the throttle window")

	e.metrics.mu.Lock()
	failures := e.metrics.failures
	e.metrics.mu.Unlock()
	assert.Equal(t, 22, failures, "every failure is counted even when the log is throttled")
	assert.NotContains(t, e.logs.String(), "alice")
}

func TestLimiterRecoveryResumesEnforcement(t *testing.T) {
	t.Parallel()
	clock := &flowClock{now: time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)}
	memory, err := ratelimit.NewMemoryRESTLimiter(0.001, 1, clock)
	require.NoError(t, err)
	flaky := &switchLimiter{inner: memory}
	e := buildRealLimiterEnv(t, flaky, clock, memory)

	flaky.broken.Store(true)
	for range 5 {
		assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	}
	flaky.broken.Store(false)
	assert.Equal(t, http.StatusOK, e.get(t, "/api/me", "alice").Code)
	assert.Equal(t, http.StatusTooManyRequests, e.get(t, "/api/me", "alice").Code)
}

type switchLimiter struct {
	inner  httpapi.RateLimiter
	broken atomic.Bool
}

func (s *switchLimiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	if s.broken.Load() {
		return false, 0, errors.New("backend unreachable")
	}
	return s.inner.Allow(ctx, key)
}

func TestLimiterFailureStillRunsTheHandlerAndKeepsErrorFormatsIntact(t *testing.T) {
	t.Parallel()
	clock := &flowClock{now: time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)}
	limiter := &flakyLimiter{}
	limiter.fail.Store(true)
	e := buildRealLimiterEnv(t, limiter, clock, nil)

	rec := e.get(t, "/api/boards", "alice")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.EqualValues(t, 1, e.listed.Load())

	rec = e.get(t, "/api/me", "forged")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.EqualValues(t, 1, limiter.calls.Load(), "only the authenticated request consulted the limiter")
}
