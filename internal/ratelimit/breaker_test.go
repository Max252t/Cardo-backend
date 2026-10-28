package ratelimit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/ratelimit"
)

type scriptedAllower struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *scriptedAllower) Allow(context.Context, string) (bool, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return false, 0, s.err
	}
	return true, 0, nil
}

func (s *scriptedAllower) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *scriptedAllower) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestBreakerOpensAfterConsecutiveFailuresAndSkipsTheBackend(t *testing.T) {
	t.Parallel()
	inner := &scriptedAllower{err: errors.New("redis down")}
	clock := newFakeClock()
	b := ratelimit.NewBreaker(inner, 3, 5*time.Second, clock)

	for range 3 {
		_, _, err := b.Allow(context.Background(), "u")
		require.Error(t, err)
		assert.NotErrorIs(t, err, ratelimit.ErrBreakerOpen)
	}
	for range 10 {
		_, _, err := b.Allow(context.Background(), "u")
		require.ErrorIs(t, err, ratelimit.ErrBreakerOpen)
	}
	assert.Equal(t, 3, inner.count())
}

func TestBreakerProbesAfterTheCooldownAndClosesOnSuccess(t *testing.T) {
	t.Parallel()
	inner := &scriptedAllower{err: errors.New("redis down")}
	clock := newFakeClock()
	b := ratelimit.NewBreaker(inner, 2, 5*time.Second, clock)
	for range 2 {
		_, _, _ = b.Allow(context.Background(), "u")
	}
	_, _, err := b.Allow(context.Background(), "u")
	require.ErrorIs(t, err, ratelimit.ErrBreakerOpen)

	clock.Advance(5 * time.Second)
	_, _, err = b.Allow(context.Background(), "u")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ratelimit.ErrBreakerOpen, "the probe reaches the backend")
	_, _, err = b.Allow(context.Background(), "u")
	require.ErrorIs(t, err, ratelimit.ErrBreakerOpen, "a failed probe reopens the breaker")

	clock.Advance(5 * time.Second)
	inner.setErr(nil)
	ok, _, err := b.Allow(context.Background(), "u")
	require.NoError(t, err)
	assert.True(t, ok)
	for range 5 {
		_, _, err = b.Allow(context.Background(), "u")
		require.NoError(t, err)
	}
}

func TestBreakerCountsOnlyConsecutiveFailures(t *testing.T) {
	t.Parallel()
	inner := &scriptedAllower{}
	b := ratelimit.NewBreaker(inner, 3, time.Minute, newFakeClock())
	for range 5 {
		inner.setErr(errors.New("blip"))
		_, _, _ = b.Allow(context.Background(), "u")
		_, _, _ = b.Allow(context.Background(), "u")
		inner.setErr(nil)
		_, _, err := b.Allow(context.Background(), "u")
		require.NoError(t, err)
	}
}

func TestBreakerIgnoresCanceledContexts(t *testing.T) {
	t.Parallel()
	inner := &scriptedAllower{err: context.Canceled}
	b := ratelimit.NewBreaker(inner, 1, time.Minute, newFakeClock())
	for range 5 {
		_, _, err := b.Allow(context.Background(), "u")
		require.ErrorIs(t, err, context.Canceled)
	}
	assert.Equal(t, 5, inner.count())
}
