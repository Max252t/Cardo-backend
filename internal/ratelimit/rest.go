package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"

	"github.com/Max252t/Cardo-backend/internal/clock"
)

const (
	RedisRESTKeyPrefix = "ratelimit:rest:"

	defaultRedisTimeout = 250 * time.Millisecond
	minMemorySweepEvery = time.Minute
	memoryIdleMargin    = time.Minute
)

func validateRate(perSecond float64, burst int) error {
	if perSecond <= 0 || math.IsNaN(perSecond) || math.IsInf(perSecond, 0) {
		return errors.New("rate must be a positive number")
	}
	if burst < 1 {
		return errors.New("burst must be at least 1")
	}
	return nil
}

type MemoryRESTLimiter struct {
	mu         sync.Mutex
	limit      rate.Limit
	burst      int
	clock      clock.Clock
	idleAfter  time.Duration
	sweepEvery time.Duration
	lastSweep  time.Time
	entries    map[string]*memoryBucket
}

type memoryBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewMemoryRESTLimiter(perSecond float64, burst int, c clock.Clock) (*MemoryRESTLimiter, error) {
	if err := validateRate(perSecond, burst); err != nil {
		return nil, fmt.Errorf("memory rest limiter: %w", err)
	}
	if c == nil {
		c = clock.System{}
	}
	refill := time.Duration(float64(burst) / perSecond * float64(time.Second))
	idle := refill + memoryIdleMargin
	return &MemoryRESTLimiter{
		limit:      rate.Limit(perSecond),
		burst:      burst,
		clock:      c,
		idleAfter:  idle,
		sweepEvery: max(idle, minMemorySweepEvery),
		lastSweep:  c.Now(),
		entries:    make(map[string]*memoryBucket),
	}, nil
}

func (l *MemoryRESTLimiter) Allow(_ context.Context, key string) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	l.sweep(now)
	b, ok := l.entries[key]
	if !ok {
		b = &memoryBucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.entries[key] = b
	}
	b.lastSeen = now
	r := b.limiter.ReserveN(now, 1)
	if !r.OK() {
		return false, time.Second, nil
	}
	delay := r.DelayFrom(now)
	if delay <= 0 {
		return true, 0, nil
	}
	r.CancelAt(now)
	return false, delay, nil
}

func (l *MemoryRESTLimiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.sweepEvery {
		return
	}
	l.lastSweep = now
	for k, b := range l.entries {
		if now.Sub(b.lastSeen) >= l.idleAfter {
			delete(l.entries, k)
		}
	}
}

func (l *MemoryRESTLimiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

var gcraScript = redis.NewScript(`
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000000 + tonumber(t[2])
local interval = tonumber(ARGV[1])
local burst = tonumber(ARGV[2])
local tat = tonumber(redis.call('GET', KEYS[1]))
if not tat or tat < now then
  tat = now
end
local newTat = tat + interval
local window = burst * interval
local wait = newTat - now - window
if wait > 0 then
  return {0, wait}
end
local ttl = math.ceil((newTat - now) / 1000) + 1
redis.call('SET', KEYS[1], string.format('%.0f', newTat), 'PX', ttl)
return {1, 0}
`)

type RedisRESTLimiter struct {
	client   redis.UniversalClient
	interval int64
	burst    int
	timeout  time.Duration
}

type RedisRESTOption func(*RedisRESTLimiter)

func WithRedisTimeout(d time.Duration) RedisRESTOption {
	return func(l *RedisRESTLimiter) {
		if d > 0 {
			l.timeout = d
		}
	}
}

func NewRedisRESTLimiter(client redis.UniversalClient, perSecond float64, burst int, opts ...RedisRESTOption) (*RedisRESTLimiter, error) {
	if err := validateRate(perSecond, burst); err != nil {
		return nil, fmt.Errorf("redis rest limiter: %w", err)
	}
	interval := int64(math.Round(float64(time.Second/time.Microsecond) / perSecond))
	if interval < 1 {
		interval = 1
	}
	l := &RedisRESTLimiter{client: client, interval: interval, burst: burst, timeout: defaultRedisTimeout}
	for _, opt := range opts {
		opt(l)
	}
	return l, nil
}

func (l *RedisRESTLimiter) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, l.timeout)
	defer cancel()
	res, err := gcraScript.Run(ctx, l.client, []string{RedisRESTKeyPrefix + key}, l.interval, l.burst).Int64Slice()
	if err != nil {
		return false, 0, fmt.Errorf("rest rate limit: %w", err)
	}
	if len(res) != 2 {
		return false, 0, fmt.Errorf("rest rate limit: unexpected reply length %d", len(res))
	}
	if res[0] == 1 {
		return true, 0, nil
	}
	return false, time.Duration(res[1]) * time.Microsecond, nil
}
