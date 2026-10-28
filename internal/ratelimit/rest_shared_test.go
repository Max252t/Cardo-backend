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

func TestRedisRESTLimiterBucketIsSharedBetweenReplicas(t *testing.T) {
	clientA := testutil.NewRedis(t)
	clientB := testutil.NewRedis(t)
	user := uuid.NewString()
	replicaA, err := ratelimit.NewRedisRESTLimiter(clientA, 0.0001, 4)
	require.NoError(t, err)
	replicaB, err := ratelimit.NewRedisRESTLimiter(clientB, 0.0001, 4)
	require.NoError(t, err)

	for range 2 {
		ok, _ := allow(t, replicaA, user)
		require.True(t, ok)
		ok, _ = allow(t, replicaB, user)
		require.True(t, ok)
	}
	ok, _ := allow(t, replicaA, user)
	assert.False(t, ok)
	ok, _ = allow(t, replicaB, user)
	assert.False(t, ok)
}

func TestRedisRESTLimiterConcurrentReplicasAllowExactlyBurst(t *testing.T) {
	user := uuid.NewString()
	const burst = 30
	limiters := make([]*ratelimit.RedisRESTLimiter, 3)
	for i := range limiters {
		l, err := ratelimit.NewRedisRESTLimiter(testutil.NewRedis(t), 0.0001, burst)
		require.NoError(t, err)
		limiters[i] = l
	}

	var allowed, denied, failed atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range 150 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, _, err := limiters[i%len(limiters)].Allow(context.Background(), user)
			switch {
			case err != nil:
				failed.Add(1)
			case ok:
				allowed.Add(1)
			default:
				denied.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.EqualValues(t, burst, allowed.Load())
	assert.EqualValues(t, 150-burst, denied.Load())
	assert.Zero(t, failed.Load())
}

func TestRedisRESTLimiterRetryAfterMatchesTheRate(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	l, err := ratelimit.NewRedisRESTLimiter(client, 2, 1)
	require.NoError(t, err)

	ok, _ := allow(t, l, user)
	require.True(t, ok)
	ok, retry := allow(t, l, user)
	require.False(t, ok)
	assert.Greater(t, retry, 400*time.Millisecond)
	assert.LessOrEqual(t, retry, 500*time.Millisecond)
}

func TestRedisRESTLimiterDeniedRequestsDoNotPushTheBucketForward(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	l, err := ratelimit.NewRedisRESTLimiter(client, 1, 1)
	require.NoError(t, err)

	allow(t, l, user)
	for range 50 {
		ok, _ := allow(t, l, user)
		require.False(t, ok)
	}
	require.Eventually(t, func() bool {
		ok, _, err := l.Allow(context.Background(), user)
		return err == nil && ok
	}, 2*time.Second, 10*time.Millisecond, "hammering a closed bucket must not extend the wait")
}

func TestRedisRESTLimiterKeysAreNamespaced(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	l, err := ratelimit.NewRedisRESTLimiter(client, 10, 5)
	require.NoError(t, err)
	allow(t, l, user)

	exists, err := client.Exists(context.Background(), ratelimit.RedisRESTKeyPrefix+user).Result()
	require.NoError(t, err)
	assert.EqualValues(t, 1, exists)
	exists, err = client.Exists(context.Background(), user).Result()
	require.NoError(t, err)
	assert.Zero(t, exists)
}

func TestRedisRESTLimiterBurstIsHonouredAfterIdleRefill(t *testing.T) {
	client := testutil.NewRedis(t)
	user := uuid.NewString()
	l, err := ratelimit.NewRedisRESTLimiter(client, 50, 3)
	require.NoError(t, err)

	for range 3 {
		ok, _ := allow(t, l, user)
		require.True(t, ok)
	}
	ok, _ := allow(t, l, user)
	require.False(t, ok)

	time.Sleep(200 * time.Millisecond)
	for i := range 3 {
		ok, _ := allow(t, l, user)
		assert.True(t, ok, "request %d after refill", i+1)
	}
	ok, _ = allow(t, l, user)
	assert.False(t, ok)
}

func TestRedisRESTLimiterFailsWhenServerIsUnreachableAndRecoversWithTheClient(t *testing.T) {
	good := testutil.NewRedis(t)
	user := uuid.NewString()
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = dead.Close() })

	broken, err := ratelimit.NewRedisRESTLimiter(dead, 5, 5, ratelimit.WithRedisTimeout(200*time.Millisecond))
	require.NoError(t, err)
	start := time.Now()
	ok, retry, err := broken.Allow(context.Background(), user)
	require.Error(t, err)
	assert.False(t, ok)
	assert.Zero(t, retry)
	assert.Less(t, time.Since(start), 2*time.Second)

	healthy, err := ratelimit.NewRedisRESTLimiter(good, 5, 5)
	require.NoError(t, err)
	ok, _ = allow(t, healthy, user)
	assert.True(t, ok)
}

func TestRedisRESTLimiterHonoursCallerContext(t *testing.T) {
	client := testutil.NewRedis(t)
	l, err := ratelimit.NewRedisRESTLimiter(client, 5, 5)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ok, _, err := l.Allow(ctx, uuid.NewString())
	require.Error(t, err)
	assert.False(t, ok)
}

func TestRedisRESTLimiterIgnoresNonPositiveTimeoutOption(t *testing.T) {
	client := testutil.NewRedis(t)
	l, err := ratelimit.NewRedisRESTLimiter(client, 5, 5, ratelimit.WithRedisTimeout(0), ratelimit.WithRedisTimeout(-time.Second))
	require.NoError(t, err)
	ok, _ := allow(t, l, uuid.NewString())
	assert.True(t, ok)
}

func TestRedisRESTLimiterRejectsInvalidRatesButAcceptsExtremes(t *testing.T) {
	client := testutil.NewRedis(t)
	_, err := ratelimit.NewRedisRESTLimiter(client, 1e12, 1)
	assert.NoError(t, err)
	l, err := ratelimit.NewRedisRESTLimiter(client, 1e12, 1)
	require.NoError(t, err)
	ok, _ := allow(t, l, uuid.NewString())
	assert.True(t, ok)

	slow, err := ratelimit.NewRedisRESTLimiter(client, 0.00001, 1)
	require.NoError(t, err)
	user := uuid.NewString()
	ok, _ = allow(t, slow, user)
	require.True(t, ok)
	ok, retry := allow(t, slow, user)
	assert.False(t, ok)
	assert.Greater(t, retry, time.Hour)
}

func TestMemoryRESTLimiterKeepsActiveUsersWhenSweeping(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, err := ratelimit.NewMemoryRESTLimiter(1, 3, clock)
	require.NoError(t, err)

	for range 3 {
		ok, _ := allow(t, l, "busy")
		require.True(t, ok)
	}
	allow(t, l, "idle")

	for range 40 {
		clock.Advance(10 * time.Second)
		allow(t, l, "busy")
	}
	assert.Equal(t, 1, l.Size(), "only the continuously active key survives the sweeps")
}

func TestMemoryRESTLimiterDoesNotForgetDrainedBucketsTooEarly(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, err := ratelimit.NewMemoryRESTLimiter(0.001, 2, clock)
	require.NoError(t, err)

	allow(t, l, "u")
	allow(t, l, "u")
	ok, _ := allow(t, l, "u")
	require.False(t, ok)

	clock.Advance(5 * time.Minute)
	allow(t, l, "other")
	ok, retry := allow(t, l, "u")
	assert.False(t, ok, "the drained bucket is still remembered while it is refilling")
	assert.Greater(t, retry, time.Duration(0))
}

func TestMemoryRESTLimiterFractionalRateRetryAfter(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	l, err := ratelimit.NewMemoryRESTLimiter(0.5, 1, clock)
	require.NoError(t, err)

	allow(t, l, "u")
	ok, retry := allow(t, l, "u")
	require.False(t, ok)
	assert.Equal(t, 2*time.Second, retry)

	clock.Advance(500 * time.Millisecond)
	_, retry = allow(t, l, "u")
	assert.Equal(t, 1500*time.Millisecond, retry)
}

func TestMemoryRESTLimiterManyKeysAreIndependent(t *testing.T) {
	t.Parallel()
	l, err := ratelimit.NewMemoryRESTLimiter(0.0001, 1, newFakeClock())
	require.NoError(t, err)
	for range 500 {
		ok, _ := allow(t, l, uuid.NewString())
		require.True(t, ok)
	}
	assert.Equal(t, 500, l.Size())
}
