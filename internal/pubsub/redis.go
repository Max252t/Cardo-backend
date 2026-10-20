package pubsub

import (
	"context"
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
			if m.Kind == "subscribe" {
				b.confirm(m.Channel)
			}
		}
	}
}

func (b *RedisBroker) confirm(channel string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.waiters[channel] {
		close(w)
	}
	delete(b.waiters, channel)
}

func (b *RedisBroker) waiter(channel string) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	w := make(chan struct{})
	b.waiters[channel] = append(b.waiters[channel], w)
	return w
}

func (b *RedisBroker) forget(channel string, w chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.waiters[channel]
	for i, x := range list {
		if x == w {
			b.waiters[channel] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(b.waiters[channel]) == 0 {
		delete(b.waiters, channel)
	}
}

func (b *RedisBroker) Publish(ctx context.Context, channel string, payload []byte) error {
	return b.client.Publish(ctx, channel, payload).Err()
}

func (b *RedisBroker) Subscribe(ctx context.Context, channels ...string) error {
	waiters := make(map[string]chan struct{}, len(channels))
	for _, ch := range channels {
		waiters[ch] = b.waiter(ch)
	}
	if err := b.ps.Subscribe(ctx, channels...); err != nil {
		for ch, w := range waiters {
			b.forget(ch, w)
		}
		return fmt.Errorf("redis subscribe: %w", err)
	}
	for ch, w := range waiters {
		select {
		case <-w:
		case <-ctx.Done():
			b.forget(ch, w)
			return fmt.Errorf("redis subscribe %s: %w", ch, ctx.Err())
		case <-b.done:
			return ErrClosed
		}
	}
	return nil
}

func (b *RedisBroker) Unsubscribe(ctx context.Context, channels ...string) error {
	if err := b.ps.Unsubscribe(ctx, channels...); err != nil {
		return fmt.Errorf("redis unsubscribe: %w", err)
	}
	return nil
}

func (b *RedisBroker) Messages() <-chan Message {
	return b.out
}

func (b *RedisBroker) Close() error {
	return b.ps.Close()
}
