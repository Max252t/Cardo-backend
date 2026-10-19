package auth_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func redisKey() string {
	return "it-" + uuid.NewString()
}

func TestRedisLimiterBlocksAfterMaxFailures(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 5, time.Minute)
	key := redisKey()

	assert.True(t, allow(t, l, key))
	for i := 1; i <= 4; i++ {
		fail(t, l, key, 1)
		assert.True(t, allow(t, l, key), "after %d failures", i)
	}
	fail(t, l, key, 1)
	assert.False(t, allow(t, l, key))
	fail(t, l, key, 1)
	assert.False(t, allow(t, l, key))
}

func TestRedisLimiterKeysAreIndependent(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 2, time.Minute)
	a, b := redisKey(), redisKey()
	fail(t, l, a, 2)
	assert.False(t, allow(t, l, a))
	assert.True(t, allow(t, l, b))
}

func TestRedisLimiterReset(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 2, time.Minute)
	key := redisKey()
	fail(t, l, key, 2)
	require.False(t, allow(t, l, key))

	require.NoError(t, l.Reset(context.Background(), key))
	assert.True(t, allow(t, l, key))
	require.NoError(t, l.Reset(context.Background(), key), "reset of a missing key is a no-op")
}

func TestRedisLimiterTTLIsSetOnceFromFirstFailure(t *testing.T) {
	client := testutil.NewRedis(t)
	window := 90 * time.Second
	l := auth.NewRedisLimiter(client, 10, window)
	key := redisKey()
	ctx := context.Background()

	fail(t, l, key, 1)
	first, err := client.TTL(ctx, "ratelimit:login:"+key).Result()
	require.NoError(t, err)
	assert.Greater(t, first, time.Duration(0))
	assert.LessOrEqual(t, first, window)

	fail(t, l, key, 3)
	second, err := client.TTL(ctx, "ratelimit:login:"+key).Result()
	require.NoError(t, err)
	assert.Greater(t, second, time.Duration(0))
	assert.LessOrEqual(t, second, first, "later failures must not extend the window")

	n, err := client.Get(ctx, "ratelimit:login:"+key).Int()
	require.NoError(t, err)
	assert.Equal(t, 4, n)
}

func TestRedisLimiterWindowExpiry(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 2, time.Minute)
	key := redisKey()
	ctx := context.Background()

	fail(t, l, key, 2)
	require.False(t, allow(t, l, key))

	require.NoError(t, client.PExpire(ctx, "ratelimit:login:"+key, time.Millisecond).Err())
	assert.Eventually(t, func() bool {
		ok, err := l.Allow(ctx, key)
		return err == nil && ok
	}, 5*time.Second, 5*time.Millisecond)

	fail(t, l, key, 1)
	ttl, err := client.TTL(ctx, "ratelimit:login:"+key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "a fresh window must start after expiry")
}

func TestRedisLimiterConcurrentFailuresAreCounted(t *testing.T) {
	client := testutil.NewRedis(t)
	const workers = 25
	l := auth.NewRedisLimiter(client, workers, time.Minute)
	key := redisKey()

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- l.RecordFailure(context.Background(), key)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	n, err := client.Get(context.Background(), "ratelimit:login:"+key).Int()
	require.NoError(t, err)
	assert.Equal(t, workers, n)
	assert.False(t, allow(t, l, key))
}

func TestServiceLoginWithRedisLimiterSixthAttempt(t *testing.T) {
	client := testutil.NewRedis(t)
	f := newFixture(t)
	svc := auth.NewService(auth.Deps{
		Store:      f.store,
		Hasher:     auth.NewArgon2Hasher(fastArgon2),
		Access:     f.jwt,
		Limiter:    auth.NewRedisLimiter(client, 5, time.Minute),
		Clock:      f.clock,
		RefreshTTL: refreshTTL,
	})
	email := redisKey() + "@example.com"
	_, err := svc.Register(context.Background(), auth.RegisterInput{Email: email, Password: "password1", DisplayName: "A"})
	require.NoError(t, err)

	in := auth.LoginInput{Email: email, Password: "wrong-password", ClientIP: "10.1.1.1"}
	for i := 1; i <= 5; i++ {
		_, err := svc.Login(context.Background(), in)
		require.ErrorIs(t, err, domain.ErrInvalidCredential, "attempt %d", i)
	}
	_, err = svc.Login(context.Background(), in)
	require.ErrorIs(t, err, domain.ErrRateLimited)

	in.Password = "password1"
	_, err = svc.Login(context.Background(), in)
	require.ErrorIs(t, err, domain.ErrRateLimited)

	in.ClientIP = "10.1.1.2"
	_, err = svc.Login(context.Background(), in)
	assert.NoError(t, err)
}
