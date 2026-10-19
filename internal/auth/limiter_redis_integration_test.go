package auth_test

import (
	"context"
	"sync"
	"sync/atomic"
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

func TestRedisLimiterAllowsMaxAttemptsThenBlocks(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 5, time.Minute)
	key := redisKey()

	for i := 1; i <= 5; i++ {
		r := reserve(t, l, key)
		assert.True(t, r.Allowed, "attempt %d", i)
		assert.Equal(t, i, r.Attempts)
	}
	r := reserve(t, l, key)
	assert.False(t, r.Allowed)
	assert.Greater(t, r.RetryAfter, time.Duration(0))
	assert.LessOrEqual(t, r.RetryAfter, time.Minute)
}

func TestRedisLimiterKeysAreIndependent(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 2, time.Minute)
	a, b := redisKey(), redisKey()
	reserveN(t, l, a, 2)
	assert.False(t, reserve(t, l, a).Allowed)
	assert.True(t, reserve(t, l, b).Allowed)
}

func TestRedisLimiterReset(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 2, time.Minute)
	key := redisKey()
	reserveN(t, l, key, 3)

	require.NoError(t, l.Reset(context.Background(), key))
	assert.Equal(t, 1, reserve(t, l, key).Attempts)
	require.NoError(t, l.Reset(context.Background(), key))
	require.NoError(t, l.Reset(context.Background(), key), "reset of a missing key is a no-op")
}

func TestRedisLimiterTTLIsSetOnceFromFirstAttempt(t *testing.T) {
	client := testutil.NewRedis(t)
	window := 90 * time.Second
	l := auth.NewRedisLimiter(client, 10, window)
	key := redisKey()
	ctx := context.Background()

	reserve(t, l, key)
	first, err := client.PTTL(ctx, auth.RedisLoginKeyPrefix+key).Result()
	require.NoError(t, err)
	assert.Greater(t, first, time.Duration(0))
	assert.LessOrEqual(t, first, window)

	reserveN(t, l, key, 3)
	second, err := client.PTTL(ctx, auth.RedisLoginKeyPrefix+key).Result()
	require.NoError(t, err)
	assert.Greater(t, second, time.Duration(0))
	assert.LessOrEqual(t, second, first, "later attempts must not extend the window")

	n, err := client.Get(ctx, auth.RedisLoginKeyPrefix+key).Int()
	require.NoError(t, err)
	assert.Equal(t, 4, n)
}

func TestRedisLimiterRestoresMissingTTL(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 10, time.Minute)
	key := redisKey()
	ctx := context.Background()
	require.NoError(t, client.Set(ctx, auth.RedisLoginKeyPrefix+key, 3, 0).Err())

	r := reserve(t, l, key)
	assert.Equal(t, 4, r.Attempts)
	ttl, err := client.PTTL(ctx, auth.RedisLoginKeyPrefix+key).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "a key without TTL must not block forever")
}

func TestRedisLimiterWindowExpiry(t *testing.T) {
	client := testutil.NewRedis(t)
	l := auth.NewRedisLimiter(client, 2, time.Minute)
	key := redisKey()
	ctx := context.Background()

	reserveN(t, l, key, 2)
	require.NoError(t, client.PExpire(ctx, auth.RedisLoginKeyPrefix+key, time.Millisecond).Err())
	assert.Eventually(t, func() bool {
		n, err := client.Exists(ctx, auth.RedisLoginKeyPrefix+key).Result()
		return err == nil && n == 0
	}, 5*time.Second, 5*time.Millisecond)

	r := reserve(t, l, key)
	assert.True(t, r.Allowed)
	assert.Equal(t, 1, r.Attempts)
}

func TestRedisLimiterConcurrentReservationsAreBounded(t *testing.T) {
	client := testutil.NewRedis(t)
	const maxAttempts = 5
	l := auth.NewRedisLimiter(client, maxAttempts, time.Minute)
	key := redisKey()

	var (
		wg      sync.WaitGroup
		allowed atomic.Int32
	)
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.Reserve(context.Background(), key)
			assert.NoError(t, err)
			if r.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(maxAttempts), allowed.Load())

	n, err := client.Get(context.Background(), auth.RedisLoginKeyPrefix+key).Int()
	require.NoError(t, err)
	assert.Equal(t, 50, n)
}

func TestServiceConcurrentLoginsWithRedisLimiter(t *testing.T) {
	client := testutil.NewRedis(t)
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")
	hasher := newSlowHasher(f.hasher, 20*time.Millisecond)
	svc := auth.NewService(auth.Deps{
		Store:      f.store,
		Hasher:     hasher,
		Access:     f.jwt,
		Limiter:    auth.NewRedisLimiter(client, 5, time.Minute),
		Clock:      f.clock,
		RefreshTTL: refreshTTL,
	})
	ip := redisKey()

	results := concurrentLogins(svc, 50, auth.LoginInput{Email: "alice@example.com", Password: "wrong-password", ClientIP: ip})
	assert.LessOrEqual(t, hasher.verifies.Load(), int32(5))
	assert.Equal(t, 5, results[domain.ErrInvalidCredential])
	assert.Equal(t, 45, results[domain.ErrRateLimited])
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
