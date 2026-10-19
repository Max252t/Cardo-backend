package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

var tinyParams = Argon2Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}

func TestArgon2HasherDefaultSlotsFollowGOMAXPROCS(t *testing.T) {
	t.Parallel()
	h := NewArgon2Hasher(tinyParams)
	assert.Positive(t, cap(h.slots))
	assert.Equal(t, DefaultHashAcquireTimeout, h.acquireTimeout)

	h = NewArgon2Hasher(tinyParams, WithMaxConcurrent(3), WithAcquireTimeout(time.Second))
	assert.Equal(t, 3, cap(h.slots))
	assert.Equal(t, time.Second, h.acquireTimeout)

	h = NewArgon2Hasher(tinyParams, WithMaxConcurrent(0), WithAcquireTimeout(0))
	assert.Positive(t, cap(h.slots))
	assert.Equal(t, DefaultHashAcquireTimeout, h.acquireTimeout)
}

func TestArgon2HasherOverloadedWhenSlotsBusy(t *testing.T) {
	t.Parallel()
	h := NewArgon2Hasher(tinyParams, WithMaxConcurrent(1), WithAcquireTimeout(20*time.Millisecond))
	encoded, err := h.Hash(context.Background(), "password")
	require.NoError(t, err)

	h.slots <- struct{}{}
	_, err = h.Hash(context.Background(), "password")
	assert.ErrorIs(t, err, domain.ErrOverloaded)
	_, err = h.Verify(context.Background(), "password", encoded)
	assert.ErrorIs(t, err, domain.ErrOverloaded)

	<-h.slots
	ok, err := h.Verify(context.Background(), "password", encoded)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestArgon2HasherHonoursContextWhileWaiting(t *testing.T) {
	t.Parallel()
	h := NewArgon2Hasher(tinyParams, WithMaxConcurrent(1), WithAcquireTimeout(time.Minute))
	h.slots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := h.Hash(ctx, "password")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), time.Second)
}

func TestArgon2HasherWaitsForFreeSlot(t *testing.T) {
	t.Parallel()
	h := NewArgon2Hasher(tinyParams, WithMaxConcurrent(1), WithAcquireTimeout(5*time.Second))
	h.slots <- struct{}{}
	go func() {
		time.Sleep(20 * time.Millisecond)
		<-h.slots
	}()
	_, err := h.Hash(context.Background(), "password")
	assert.NoError(t, err)
}
