package pubsub_test

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/pubsub"
)

func TestMemorySubscriptionBookkeeping(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := pubsub.NewMemoryNetwork().NewBroker()
	assert.False(t, b.Subscribed("x"))
	require.NoError(t, b.Subscribe(ctx, "x", "y"))
	assert.True(t, b.Subscribed("x"))
	assert.True(t, b.Subscribed("y"))
	require.NoError(t, b.Unsubscribe(ctx, "x", "never-subscribed"))
	assert.False(t, b.Subscribed("x"))
	assert.True(t, b.Subscribed("y"))
	require.NoError(t, b.Subscribe(ctx, "y"))
	require.NoError(t, b.Unsubscribe(ctx, "y"))
	assert.False(t, b.Subscribed("y"), "subscriptions are not reference counted")
}

func TestMemoryPublishRespectsContextAndClosedBrokers(t *testing.T) {
	t.Parallel()
	net := pubsub.NewMemoryNetwork()
	b := net.NewBroker()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.ErrorIs(t, b.Publish(cancelled, "x", nil), context.Canceled)

	require.NoError(t, b.Close())
	require.NoError(t, b.Close(), "closing twice is harmless")
	assert.ErrorIs(t, b.Publish(context.Background(), "x", nil), pubsub.ErrClosed)
	assert.ErrorIs(t, b.Subscribe(context.Background(), "x"), pubsub.ErrClosed)
	assert.NoError(t, b.Unsubscribe(context.Background(), "x"))
}

func TestMemoryPayloadsAreCopiedPerSubscriber(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := pubsub.NewMemoryNetwork()
	pub, a, b := net.NewBroker(), net.NewBroker(), net.NewBroker()
	require.NoError(t, a.Subscribe(ctx, "x"))
	require.NoError(t, b.Subscribe(ctx, "x"))
	payload := []byte("original")
	require.NoError(t, pub.Publish(ctx, "x", payload))
	payload[0] = 'X'

	first := receive(t, a)
	first.Payload[1] = 'Z'
	second := receive(t, b)
	assert.Equal(t, "original", string(second.Payload))
}

func TestMemoryPublisherDoesNotReceiveUnlessSubscribed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := pubsub.NewMemoryNetwork().NewBroker()
	require.NoError(t, b.Publish(ctx, "x", []byte("hello")))
	assertSilent(t, b)
}

func TestMemoryDropFilterCanBeCleared(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := pubsub.NewMemoryNetwork()
	b := net.NewBroker()
	require.NoError(t, b.Subscribe(ctx, "x"))
	net.SetDropFilter(func(pubsub.Message) bool { return true })
	require.NoError(t, b.Publish(ctx, "x", []byte("dropped")))
	assertSilent(t, b)
	net.SetDropFilter(nil)
	require.NoError(t, b.Publish(ctx, "x", []byte("kept")))
	assert.Equal(t, "kept", string(receive(t, b).Payload))
}

func TestMemoryConcurrentPublishSubscribeAndCloseIsSafe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := pubsub.NewMemoryNetwork()
	listener := net.NewBroker()
	require.NoError(t, listener.Subscribe(ctx, "x"))

	var wg sync.WaitGroup
	for w := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b := net.NewBroker()
			for i := range 50 {
				_ = b.Subscribe(ctx, "x")
				_ = b.Publish(ctx, "x", []byte(strconv.Itoa(w*100+i)))
				_ = b.Unsubscribe(ctx, "x")
			}
			_ = b.Close()
		}()
	}
	wg.Wait()
	require.NoError(t, listener.Close())
}

func TestMemoryPerPublisherOrderIsPreserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := pubsub.NewMemoryNetwork()
	pub, sub := net.NewBroker(), net.NewBroker()
	require.NoError(t, sub.Subscribe(ctx, "x"))
	for i := range 200 {
		require.NoError(t, pub.Publish(ctx, "x", []byte(strconv.Itoa(i))))
	}
	for i := range 200 {
		assert.Equal(t, strconv.Itoa(i), string(receive(t, sub).Payload))
	}
}
