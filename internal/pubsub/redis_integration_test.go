package pubsub_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

func TestRedisBrokerSubscribeWaitsForConfirmationAndRoutes(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a := pubsub.NewRedisBroker(ctx, client)
	b := pubsub.NewRedisBroker(ctx, client)
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	board := protocol.BoardChannel(uuid.New())
	presence := protocol.PresenceChannel(uuid.New())
	require.NoError(t, b.Subscribe(ctx, board, presence))
	require.NoError(t, a.Publish(ctx, board, []byte("one")))
	m := receive(t, b)
	assert.Equal(t, board, m.Channel)
	assert.Equal(t, "one", string(m.Payload))
	require.NoError(t, a.Publish(ctx, presence, []byte("two")))
	assert.Equal(t, "two", string(receive(t, b).Payload))

	require.NoError(t, b.Unsubscribe(ctx, board))
	require.NoError(t, a.Publish(ctx, board, []byte("ignored")))
	require.NoError(t, a.Publish(ctx, presence, []byte("three")))
	assert.Equal(t, "three", string(receive(t, b).Payload))

	require.NoError(t, b.Close())
	assert.Eventually(t, func() bool {
		_, open := <-b.Messages()
		return !open
	}, 5*time.Second, 10*time.Millisecond)
}
