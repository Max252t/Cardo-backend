package auth_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
)

func allow(t *testing.T, l auth.LoginLimiter, key string) bool {
	t.Helper()
	ok, err := l.Allow(context.Background(), key)
	require.NoError(t, err)
	return ok
}

func fail(t *testing.T, l auth.LoginLimiter, key string, n int) {
	t.Helper()
	for range n {
		require.NoError(t, l.RecordFailure(context.Background(), key))
	}
}

func TestMemoryLimiterBlocksAfterMaxFailures(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(5, time.Minute, clock)

	assert.True(t, allow(t, l, "k"))
	for i := 1; i <= 4; i++ {
		fail(t, l, "k", 1)
		assert.True(t, allow(t, l, "k"), "after %d failures", i)
	}
	fail(t, l, "k", 1)
	assert.False(t, allow(t, l, "k"))
	fail(t, l, "k", 1)
	assert.False(t, allow(t, l, "k"))
}

func TestMemoryLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(2, time.Minute, newFakeClock())
	fail(t, l, "a", 2)
	assert.False(t, allow(t, l, "a"))
	assert.True(t, allow(t, l, "b"))
}

func TestMemoryLimiterWindowExpiry(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(3, time.Minute, clock)

	fail(t, l, "k", 3)
	assert.False(t, allow(t, l, "k"))

	clock.Advance(59 * time.Second)
	assert.False(t, allow(t, l, "k"))

	clock.Advance(2 * time.Second)
	assert.True(t, allow(t, l, "k"))

	fail(t, l, "k", 2)
	assert.True(t, allow(t, l, "k"))
	fail(t, l, "k", 1)
	assert.False(t, allow(t, l, "k"))
}

func TestMemoryLimiterWindowIsFixedFromFirstFailure(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(3, time.Minute, clock)

	fail(t, l, "k", 1)
	clock.Advance(40 * time.Second)
	fail(t, l, "k", 2)
	assert.False(t, allow(t, l, "k"))

	clock.Advance(21 * time.Second)
	assert.True(t, allow(t, l, "k"))
}

func TestMemoryLimiterResetClearsFailures(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(2, time.Minute, newFakeClock())
	fail(t, l, "k", 2)
	assert.False(t, allow(t, l, "k"))
	require.NoError(t, l.Reset(context.Background(), "k"))
	assert.True(t, allow(t, l, "k"))
	require.NoError(t, l.Reset(context.Background(), "missing"))
}

func TestMemoryLimiterNilClock(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(1, time.Hour, nil)
	fail(t, l, "k", 1)
	assert.False(t, allow(t, l, "k"))
}

func TestMemoryLimiterSweepsExpiredEntries(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(1, time.Minute, clock)
	for i := range 10000 {
		fail(t, l, fmt.Sprintf("k%d", i), 1)
	}
	clock.Advance(2 * time.Minute)
	fail(t, l, "fresh", 1)
	assert.False(t, allow(t, l, "fresh"))
	assert.True(t, allow(t, l, "k0"))
}

func TestMemoryLimiterConcurrentUse(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(1000, time.Minute, newFakeClock())
	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_ = l.RecordFailure(context.Background(), "k")
				_, _ = l.Allow(context.Background(), "k")
			}
		}()
	}
	wg.Wait()
	assert.False(t, allow(t, l, "k"))
}

func TestRedisLimiterReportsClientErrors(t *testing.T) {
	t.Parallel()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, client.Close())
	l := auth.NewRedisLimiter(client, 5, time.Minute)

	ok, err := l.Allow(context.Background(), "k")
	require.Error(t, err)
	assert.False(t, ok, "the limiter must fail closed when redis is unavailable")
	assert.Error(t, l.RecordFailure(context.Background(), "k"))
	assert.Error(t, l.Reset(context.Background(), "k"))
}
