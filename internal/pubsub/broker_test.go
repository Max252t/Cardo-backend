package pubsub_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
)

func receive(t *testing.T, b pubsub.Broker) pubsub.Message {
	t.Helper()
	select {
	case m := <-b.Messages():
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message received")
		return pubsub.Message{}
	}
}

func assertSilent(t *testing.T, b pubsub.Broker) {
	t.Helper()
	select {
	case m := <-b.Messages():
		t.Fatalf("unexpected message on %s", m.Channel)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestChannels(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	kind, got := pubsub.ParseChannel(pubsub.BoardChannel(id))
	assert.Equal(t, pubsub.ChannelBoard, kind)
	assert.Equal(t, id, got)
	kind, got = pubsub.ParseChannel(pubsub.PresenceChannel(id))
	assert.Equal(t, pubsub.ChannelPresence, kind)
	assert.Equal(t, id, got)
	for _, bad := range []string{"", "board:", "board:x", "other:" + id.String()} {
		kind, _ = pubsub.ParseChannel(bad)
		assert.Equal(t, pubsub.ChannelUnknown, kind, bad)
	}
}

func TestMemoryBrokersShareANetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := pubsub.NewMemoryNetwork()
	a, b := net.NewBroker(), net.NewBroker()
	defer func() { _ = a.Close() }()
	defer func() { _ = b.Close() }()

	require.NoError(t, b.Subscribe(ctx, "x"))
	require.NoError(t, a.Publish(ctx, "x", []byte("hello")))
	m := receive(t, b)
	assert.Equal(t, "x", m.Channel)
	assert.Equal(t, "hello", string(m.Payload))
	assertSilent(t, a)

	require.NoError(t, a.Subscribe(ctx, "x"))
	require.NoError(t, b.Publish(ctx, "x", []byte("both")))
	assert.Equal(t, "both", string(receive(t, a).Payload))
	assert.Equal(t, "both", string(receive(t, b).Payload))

	require.NoError(t, b.Unsubscribe(ctx, "x"))
	require.NoError(t, a.Publish(ctx, "x", []byte("only-a")))
	assert.Equal(t, "only-a", string(receive(t, a).Payload))
	assertSilent(t, b)

	net.SetDropFilter(func(m pubsub.Message) bool { return string(m.Payload) == "lost" })
	require.NoError(t, a.Publish(ctx, "x", []byte("lost")))
	assertSilent(t, a)

	require.NoError(t, a.Close())
	assert.ErrorIs(t, a.Publish(ctx, "x", nil), pubsub.ErrClosed)
	_, open := <-a.Messages()
	assert.False(t, open)
}

func TestEventPublisherSendsEnvelopesToBoardChannel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	net := pubsub.NewMemoryNetwork()
	sender, listener := net.NewBroker(), net.NewBroker()
	boardID := uuid.New()
	require.NoError(t, listener.Subscribe(ctx, pubsub.BoardChannel(boardID)))

	opID := uuid.New()
	events := []domain.Event{
		{BoardID: boardID, Seq: 7, Type: protocol.EventColumnRebalanced, Payload: json.RawMessage(`{"a":1}`), CreatedAt: time.UnixMilli(1000)},
		{BoardID: boardID, Seq: 8, Type: protocol.EventCardMoved, OpID: &opID, Payload: json.RawMessage(`{"b":2}`), CreatedAt: time.UnixMilli(2000)},
	}
	require.NoError(t, pubsub.NewEventPublisher(sender).Publish(ctx, events))
	for _, want := range events {
		var env protocol.Envelope
		require.NoError(t, json.Unmarshal(receive(t, listener).Payload, &env))
		assert.Equal(t, protocol.Version, env.V)
		assert.Equal(t, want.Seq, env.Seq)
		assert.Equal(t, want.Type, env.Type)
		assert.Equal(t, want.CreatedAt.UnixMilli(), env.ServerTS)
		assert.JSONEq(t, string(want.Payload), string(env.Payload))
	}

	require.NoError(t, sender.Close())
	assert.Error(t, pubsub.NewEventPublisher(sender).Publish(ctx, events))
}
