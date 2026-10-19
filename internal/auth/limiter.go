package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

type LoginLimiter interface {
	Allow(ctx context.Context, key string) (bool, error)
	RecordFailure(ctx context.Context, key string) error
	Reset(ctx context.Context, key string) error
}

func LoginKey(ip, email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "|" + ip))
	return hex.EncodeToString(sum[:])
}

type MemoryLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	clock   Clock
	entries map[string]memoryEntry
}

type memoryEntry struct {
	count   int
	expires time.Time
}

const memorySweepThreshold = 10000

func NewMemoryLimiter(maxFailures int, window time.Duration, clock Clock) *MemoryLimiter {
	if clock == nil {
		clock = SystemClock{}
	}
	return &MemoryLimiter{
		max:     maxFailures,
		window:  window,
		clock:   clock,
		entries: make(map[string]memoryEntry),
	}
}

func (l *MemoryLimiter) Allow(_ context.Context, key string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return true, nil
	}
	if !l.clock.Now().Before(e.expires) {
		delete(l.entries, key)
		return true, nil
	}
	return e.count < l.max, nil
}

func (l *MemoryLimiter) RecordFailure(_ context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	if len(l.entries) >= memorySweepThreshold {
		for k, e := range l.entries {
			if !now.Before(e.expires) {
				delete(l.entries, k)
			}
		}
	}
	e, ok := l.entries[key]
	if !ok || !now.Before(e.expires) {
		e = memoryEntry{expires: now.Add(l.window)}
	}
	e.count++
	l.entries[key] = e
	return nil
}

func (l *MemoryLimiter) Reset(_ context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
	return nil
}

type RedisLimiter struct {
	client redis.UniversalClient
	max    int
	window time.Duration
	prefix string
}

func NewRedisLimiter(client redis.UniversalClient, maxFailures int, window time.Duration) *RedisLimiter {
	return &RedisLimiter{
		client: client,
		max:    maxFailures,
		window: window,
		prefix: "ratelimit:login:",
	}
}

func (l *RedisLimiter) Allow(ctx context.Context, key string) (bool, error) {
	n, err := l.client.Get(ctx, l.prefix+key).Int()
	if errors.Is(err, redis.Nil) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("read login failures: %w", err)
	}
	return n < l.max, nil
}

func (l *RedisLimiter) RecordFailure(ctx context.Context, key string) error {
	k := l.prefix + key
	pipe := l.client.TxPipeline()
	pipe.Incr(ctx, k)
	pipe.ExpireNX(ctx, k, l.window)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("record login failure: %w", err)
	}
	return nil
}

func (l *RedisLimiter) Reset(ctx context.Context, key string) error {
	if err := l.client.Del(ctx, l.prefix+key).Err(); err != nil {
		return fmt.Errorf("reset login failures: %w", err)
	}
	return nil
}
