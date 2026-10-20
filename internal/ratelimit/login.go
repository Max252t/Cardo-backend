package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Max252t/Cardo-backend/internal/auth"
)

var (
	_ auth.LoginLimiter = (*MemoryLimiter)(nil)
	_ auth.LoginLimiter = (*RedisLimiter)(nil)
)

type MemoryLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	clock   auth.Clock
	entries map[string]memoryEntry
}

type memoryEntry struct {
	count   int
	expires time.Time
}

const memorySweepThreshold = 10000

func NewMemoryLimiter(maxAttempts int, window time.Duration, clock auth.Clock) *MemoryLimiter {
	if clock == nil {
		clock = auth.SystemClock{}
	}
	return &MemoryLimiter{
		max:     maxAttempts,
		window:  window,
		clock:   clock,
		entries: make(map[string]memoryEntry),
	}
}

func (l *MemoryLimiter) Reserve(_ context.Context, key string) (auth.Reservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if len(l.entries) >= memorySweepThreshold {
		l.sweep(now)
	}
	e, ok := l.entries[key]
	if !ok || !now.Before(e.expires) {
		e = memoryEntry{expires: now.Add(l.window)}
	}
	e.count++
	l.entries[key] = e
	return auth.Reservation{
		Allowed:    e.count <= l.max,
		Attempts:   e.count,
		RetryAfter: e.expires.Sub(now),
	}, nil
}

func (l *MemoryLimiter) sweep(now time.Time) {
	for k, e := range l.entries {
		if !now.Before(e.expires) {
			delete(l.entries, k)
		}
	}
}

func (l *MemoryLimiter) Reset(_ context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
	return nil
}

const RedisLoginKeyPrefix = "ratelimit:login:"

var reserveScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {n, ttl}
`)

type RedisLimiter struct {
	client redis.UniversalClient
	max    int
	window time.Duration
}

func NewRedisLimiter(client redis.UniversalClient, maxAttempts int, window time.Duration) *RedisLimiter {
	return &RedisLimiter{
		client: client,
		max:    maxAttempts,
		window: window,
	}
}

func (l *RedisLimiter) Reserve(ctx context.Context, key string) (auth.Reservation, error) {
	windowMs := l.window.Milliseconds()
	if windowMs < 1 {
		windowMs = 1
	}
	res, err := reserveScript.Run(ctx, l.client, []string{RedisLoginKeyPrefix + key}, windowMs).Int64Slice()
	if err != nil {
		return auth.Reservation{}, fmt.Errorf("reserve login attempt: %w", err)
	}
	if len(res) != 2 {
		return auth.Reservation{}, fmt.Errorf("reserve login attempt: unexpected reply length %d", len(res))
	}
	return auth.Reservation{
		Allowed:    res[0] <= int64(l.max),
		Attempts:   int(res[0]),
		RetryAfter: time.Duration(res[1]) * time.Millisecond,
	}, nil
}

func (l *RedisLimiter) Reset(ctx context.Context, key string) error {
	if err := l.client.Del(ctx, RedisLoginKeyPrefix+key).Err(); err != nil {
		return fmt.Errorf("reset login attempts: %w", err)
	}
	return nil
}
