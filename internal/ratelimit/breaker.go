package ratelimit

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/Max252t/Cardo-backend/internal/clock"
)

var ErrBreakerOpen = errors.New("rate limiter circuit breaker is open")

type Allower interface {
	Allow(ctx context.Context, key string) (bool, time.Duration, error)
}

type Breaker struct {
	inner     Allower
	threshold int
	cooldown  time.Duration
	clock     clock.Clock

	mu        sync.Mutex
	failures  int
	openUntil time.Time
}

func NewBreaker(inner Allower, threshold int, cooldown time.Duration, c clock.Clock) *Breaker {
	if c == nil {
		c = clock.System{}
	}
	return &Breaker{inner: inner, threshold: max(threshold, 1), cooldown: cooldown, clock: c}
}

func (b *Breaker) Allow(ctx context.Context, key string) (bool, time.Duration, error) {
	if b.isOpen() {
		return false, 0, ErrBreakerOpen
	}
	allowed, retry, err := b.inner.Allow(ctx, key)
	switch {
	case err == nil:
		b.recordSuccess()
	case errors.Is(err, context.Canceled):
	default:
		b.recordFailure()
	}
	return allowed, retry, err
}

func (b *Breaker) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.clock.Now().Before(b.openUntil)
}

func (b *Breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
}

func (b *Breaker) recordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = b.clock.Now().Add(b.cooldown)
	}
}
