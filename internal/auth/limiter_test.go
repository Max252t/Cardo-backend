package auth_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
)

func reserve(t *testing.T, l auth.LoginLimiter, key string) auth.Reservation {
	t.Helper()
	r, err := l.Reserve(context.Background(), key)
	require.NoError(t, err)
	return r
}

func reserveN(t *testing.T, l auth.LoginLimiter, key string, n int) {
	t.Helper()
	for range n {
		reserve(t, l, key)
	}
}

func TestMemoryLimiterAllowsMaxAttemptsThenBlocks(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(5, time.Minute, newFakeClock())

	for i := 1; i <= 5; i++ {
		r := reserve(t, l, "k")
		assert.True(t, r.Allowed, "attempt %d", i)
		assert.Equal(t, i, r.Attempts)
	}
	r := reserve(t, l, "k")
	assert.False(t, r.Allowed)
	assert.Equal(t, 6, r.Attempts)
	assert.False(t, reserve(t, l, "k").Allowed)
}

func TestMemoryLimiterRetryAfterIsRemainingWindow(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(1, time.Minute, clock)

	r := reserve(t, l, "k")
	assert.Equal(t, time.Minute, r.RetryAfter)
	clock.Advance(20 * time.Second)
	r = reserve(t, l, "k")
	assert.False(t, r.Allowed)
	assert.Equal(t, 40*time.Second, r.RetryAfter)
}

func TestMemoryLimiterKeysAreIndependent(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(2, time.Minute, newFakeClock())
	reserveN(t, l, "a", 2)
	assert.False(t, reserve(t, l, "a").Allowed)
	assert.True(t, reserve(t, l, "b").Allowed)
}

func TestMemoryLimiterWindowExpiry(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(3, time.Minute, clock)

	reserveN(t, l, "k", 3)
	clock.Advance(59 * time.Second)
	assert.False(t, reserve(t, l, "k").Allowed)

	clock.Advance(2 * time.Second)
	r := reserve(t, l, "k")
	assert.True(t, r.Allowed)
	assert.Equal(t, 1, r.Attempts)
}

func TestMemoryLimiterWindowIsFixedFromFirstAttempt(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(3, time.Minute, clock)

	reserve(t, l, "k")
	clock.Advance(40 * time.Second)
	reserveN(t, l, "k", 2)
	assert.False(t, reserve(t, l, "k").Allowed)

	clock.Advance(21 * time.Second)
	assert.True(t, reserve(t, l, "k").Allowed)
}

func TestMemoryLimiterResetClearsAttempts(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(2, time.Minute, newFakeClock())
	reserveN(t, l, "k", 2)
	require.NoError(t, l.Reset(context.Background(), "k"))
	assert.Equal(t, 1, reserve(t, l, "k").Attempts)
	require.NoError(t, l.Reset(context.Background(), "missing"))
}

func TestMemoryLimiterNilClock(t *testing.T) {
	t.Parallel()
	l := auth.NewMemoryLimiter(1, time.Hour, nil)
	reserve(t, l, "k")
	assert.False(t, reserve(t, l, "k").Allowed)
}

func TestMemoryLimiterSweepsExpiredEntries(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l := auth.NewMemoryLimiter(1, time.Minute, clock)
	for i := range 10000 {
		reserve(t, l, fmt.Sprintf("k%d", i))
	}
	clock.Advance(2 * time.Minute)
	reserve(t, l, "fresh")
	assert.False(t, reserve(t, l, "fresh").Allowed)
	assert.True(t, reserve(t, l, "k0").Allowed)
}

func TestMemoryLimiterConcurrentReservationsAreBounded(t *testing.T) {
	t.Parallel()
	const maxAttempts = 5
	l := auth.NewMemoryLimiter(maxAttempts, time.Minute, newFakeClock())
	var (
		wg      sync.WaitGroup
		allowed atomic.Int32
	)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.Reserve(context.Background(), "k")
			assert.NoError(t, err)
			if r.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(maxAttempts), allowed.Load())
}

func TestRedisLimiterReportsClientErrors(t *testing.T) {
	t.Parallel()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, client.Close())
	l := auth.NewRedisLimiter(client, 5, time.Minute)

	r, err := l.Reserve(context.Background(), "k")
	require.Error(t, err)
	assert.False(t, r.Allowed, "the limiter must fail closed when redis is unavailable")
	assert.Error(t, l.Reset(context.Background(), "k"))
}
