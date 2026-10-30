package ratelimit_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/ratelimit"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type restLimiter interface {
	Allow(ctx context.Context, key string) (bool, time.Duration, error)
}

func allow(t *testing.T, l restLimiter, key string) (bool, time.Duration) {
	t.Helper()
	ok, retry, err := l.Allow(context.Background(), key)
	require.NoError(t, err)
	return ok, retry
}

func TestMemoryRESTLimiterAllowsBurstThenBlocks(t *testing.T) {
	t.Parallel()
	l, err := ratelimit.NewMemoryRESTLimiter(10, 3, newFakeClock())
	require.NoError(t, err)

	for i := range 3 {
		ok, _ := allow(t, l, "u")
		assert.True(t, ok, "request %d", i+1)
	}
	ok, retry := allow(t, l, "u")
	assert.False(t, ok)
	assert.Equal(t, 100*time.Millisecond, retry)
}

func TestMemoryRESTLimiterRefillsOverTime(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, err := ratelimit.NewMemoryRESTLimiter(10, 2, clock)
	require.NoError(t, err)

	allow(t, l, "u")
	allow(t, l, "u")
	ok, _ := allow(t, l, "u")
	require.False(t, ok)

	clock.Advance(100 * time.Millisecond)
	ok, _ = allow(t, l, "u")
	assert.True(t, ok)
	ok, _ = allow(t, l, "u")
	assert.False(t, ok)

	clock.Advance(time.Hour)
	for range 2 {
		ok, _ = allow(t, l, "u")
		assert.True(t, ok, "the bucket never holds more than the burst")
	}
	ok, _ = allow(t, l, "u")
	assert.False(t, ok)
}

func TestMemoryRESTLimiterDeniedRequestsDoNotConsumeTokens(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, err := ratelimit.NewMemoryRESTLimiter(1, 1, clock)
	require.NoError(t, err)

	allow(t, l, "u")
	for range 50 {
		ok, _ := allow(t, l, "u")
		require.False(t, ok)
	}
	clock.Advance(time.Second)
	ok, _ := allow(t, l, "u")
	assert.True(t, ok)
}

func TestMemoryRESTLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()
	l, err := ratelimit.NewMemoryRESTLimiter(1, 1, newFakeClock())
	require.NoError(t, err)

	ok, _ := allow(t, l, "a")
	assert.True(t, ok)
	ok, _ = allow(t, l, "a")
	assert.False(t, ok)
	ok, _ = allow(t, l, "b")
	assert.True(t, ok)
}

func TestMemoryRESTLimiterEvictsIdleEntries(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, err := ratelimit.NewMemoryRESTLimiter(10, 10, clock)
	require.NoError(t, err)

	for _, k := range []string{"a", "b", "c"} {
		allow(t, l, k)
	}
	require.Equal(t, 3, l.Size())

	clock.Advance(10 * time.Minute)
	allow(t, l, "d")
	assert.Equal(t, 1, l.Size())
}

func TestMemoryRESTLimiterRejectsInvalidSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rate  float64
		burst int
	}{{0, 1}, {-1, 1}, {1, 0}} {
		_, err := ratelimit.NewMemoryRESTLimiter(tc.rate, tc.burst, nil)
		assert.Error(t, err)
		_, err = ratelimit.NewRedisRESTLimiter(nil, tc.rate, tc.burst)
		assert.Error(t, err)
	}
}

func TestMemoryRESTLimiterIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	const burst = 25
	l, err := ratelimit.NewMemoryRESTLimiter(0.001, burst, nil)
	require.NoError(t, err)

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := l.Allow(context.Background(), "u")
			if err == nil && ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(burst), allowed.Load())
}

func TestRedisRESTLimiterAllowsBurstThenBlocks(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	l, err := ratelimit.NewRedisRESTLimiter(client, 10, 3)
	require.NoError(t, err)

	for i := range 3 {
		ok, _ := allow(t, l, user)
		assert.True(t, ok, "request %d", i+1)
	}
	ok, retry := allow(t, l, user)
	assert.False(t, ok)
	assert.Greater(t, retry, time.Duration(0))
	assert.LessOrEqual(t, retry, 100*time.Millisecond)

	ok, _ = allow(t, l, user+"-other")
	assert.True(t, ok)
}

func TestRedisRESTLimiterRefills(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	l, err := ratelimit.NewRedisRESTLimiter(client, 20, 1)
	require.NoError(t, err)

	ok, _ := allow(t, l, user)
	require.True(t, ok)
	ok, retry := allow(t, l, user)
	require.False(t, ok)

	require.Eventually(t, func() bool {
		ok, _, err := l.Allow(context.Background(), user)
		return err == nil && ok
	}, 5*time.Second, retry)
}

func TestRedisRESTLimiterIsAtomicAcrossClients(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	const burst = 20
	l, err := ratelimit.NewRedisRESTLimiter(client, 0.001, burst, ratelimit.WithRedisTimeout(5*time.Second))
	require.NoError(t, err)

	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, _, err := l.Allow(context.Background(), user)
			if err == nil && ok {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(burst), allowed.Load())
}

func TestRedisRESTLimiterKeyExpiresWhenBucketIsFull(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	l, err := ratelimit.NewRedisRESTLimiter(client, 5, 1)
	require.NoError(t, err)

	allow(t, l, user)
	ttl, err := client.PTTL(context.Background(), ratelimit.RedisRESTKeyPrefix+user).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0))
	assert.LessOrEqual(t, ttl, 250*time.Millisecond)
}

func TestRedisRESTLimiterReportsClientErrors(t *testing.T) {
	t.Parallel()
	user := "u"
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, client.Close())
	l, err := ratelimit.NewRedisRESTLimiter(client, 5, 5)
	require.NoError(t, err)

	ok, _, err := l.Allow(context.Background(), user)
	require.Error(t, err)
	assert.False(t, ok)
}

func TestRedisRESTLimiterTimesOutQuickly(t *testing.T) {
	t.Parallel()
	user := "u"
	client := redis.NewClient(&redis.Options{Addr: "10.255.255.1:6379", DialTimeout: 5 * time.Second})
	t.Cleanup(func() { _ = client.Close() })
	l, err := ratelimit.NewRedisRESTLimiter(client, 5, 5, ratelimit.WithRedisTimeout(50*time.Millisecond))
	require.NoError(t, err)

	start := time.Now()
	_, _, err = l.Allow(context.Background(), user)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second)
}
