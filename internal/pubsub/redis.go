package pubsub

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
)

const redisChannelSize = 4096

type RedisBroker struct {
	client *redis.Client
	ps     *redis.PubSub
	out    chan Message
	done   chan struct{}

	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

func NewRedisBroker(ctx context.Context, client *redis.Client) *RedisBroker {
	b := &RedisBroker{
		client:  client,
		ps:      client.Subscribe(ctx),
		out:     make(chan Message, redisChannelSize),
		done:    make(chan struct{}),
		waiters: make(map[string][]chan struct{}),
	}
	go b.forward(b.ps.ChannelWithSubscriptions(redis.WithChannelSize(redisChannelSize)))
	return b
}

func (b *RedisBroker) forward(in <-chan any) {
	defer close(b.done)
	defer close(b.out)
	for raw := range in {
		switch m := raw.(type) {
		case *redis.Message:
			b.out <- Message{Channel: m.Channel, Payload: []byte(m.Payload)}
		case *redis.Subscription:
			if m.Kind == "subscribe" || m.Kind == "unsubscribe" {
				b.confirm(m.Kind, m.Channel)
			}
		}
	}
}

func waiterKey(kind, channel string) string {
	return kind + ":" + channel
}

func (b *RedisBroker) confirm(kind, channel string) {
	key := waiterKey(kind, channel)
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.waiters[key] {
		close(w)
	}
	delete(b.waiters, key)
}

func (b *RedisBroker) waiter(kind, channel string) chan struct{} {
	key := waiterKey(kind, channel)
	b.mu.Lock()
	defer b.mu.Unlock()
	w := make(chan struct{})
	b.waiters[key] = append(b.waiters[key], w)
	return w
}

func (b *RedisBroker) forget(kind, channel string, w chan struct{}) {
	key := waiterKey(kind, channel)
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.waiters[key]
	for i, x := range list {
		if x == w {
			b.waiters[key] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(b.waiters[key]) == 0 {
		delete(b.waiters, key)
	}
}

func (b *RedisBroker) Publish(ctx context.Context, channel string, payload []byte) error {
	return b.client.Publish(ctx, channel, payload).Err()
}

func (b *RedisBroker) Subscribe(ctx context.Context, channels ...string) error {
	return b.change(ctx, "subscribe", b.ps.Subscribe, channels)
}

func (b *RedisBroker) Unsubscribe(ctx context.Context, channels ...string) error {
	return b.change(ctx, "unsubscribe", b.ps.Unsubscribe, channels)
}

func (b *RedisBroker) change(ctx context.Context, kind string, send func(context.Context, ...string) error, channels []string) error {
	pending := make(map[string]chan struct{}, len(channels))
	defer func() {
		for ch, w := range pending {
			b.forget(kind, ch, w)
		}
	}()
	for _, ch := range channels {
		pending[ch] = b.waiter(kind, ch)
	}
	if err := send(ctx, channels...); err != nil {
		if b.closed() || errors.Is(err, redis.ErrClosed) {
			return ErrClosed
		}
		return fmt.Errorf("redis %s: %w", kind, err)
	}
	for ch, w := range pending {
		select {
		case <-w:
		case <-ctx.Done():
			return fmt.Errorf("redis %s %s: %w", kind, ch, ctx.Err())
		case <-b.done:
			return ErrClosed
		}
	}
	return nil
}

func (b *RedisBroker) closed() bool {
	select {
	case <-b.done:
		return true
	default:
		return false
	}
}

func (b *RedisBroker) Messages() <-chan Message {
	return b.out
}

func (b *RedisBroker) Close() error {
	return b.ps.Close()
}
