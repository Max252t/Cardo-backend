package pubsub

import (
	"context"
	"errors"
	"sync"
)

var ErrClosed = errors.New("pubsub: broker closed")

const memoryBufferSize = 4096

type MemoryNetwork struct {
	mu      sync.RWMutex
	brokers map[*MemoryBroker]struct{}
	drop    func(Message) bool
}

func NewMemoryNetwork() *MemoryNetwork {
	return &MemoryNetwork{brokers: make(map[*MemoryBroker]struct{})}
}

func (n *MemoryNetwork) SetDropFilter(drop func(Message) bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.drop = drop
}

func (n *MemoryNetwork) NewBroker() *MemoryBroker {
	b := &MemoryBroker{
		network: n,
		subs:    make(map[string]bool),
		out:     make(chan Message, memoryBufferSize),
	}
	n.mu.Lock()
	n.brokers[b] = struct{}{}
	n.mu.Unlock()
	return b
}

func (n *MemoryNetwork) deliver(msg Message) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if n.drop != nil && n.drop(msg) {
		return
	}
	for b := range n.brokers {
		b.deliver(msg)
	}
}

type MemoryBroker struct {
	network *MemoryNetwork
	mu      sync.Mutex
	subs    map[string]bool
	out     chan Message
	closed  bool
}

func (b *MemoryBroker) deliver(msg Message) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || !b.subs[msg.Channel] {
		return
	}
	b.out <- Message{Channel: msg.Channel, Payload: append([]byte(nil), msg.Payload...)}
}

func (b *MemoryBroker) Publish(ctx context.Context, channel string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.isClosed() {
		return ErrClosed
	}
	b.network.deliver(Message{Channel: channel, Payload: payload})
	return nil
}

func (b *MemoryBroker) Subscribe(_ context.Context, channels ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrClosed
	}
	for _, ch := range channels {
		b.subs[ch] = true
	}
	return nil
}

func (b *MemoryBroker) Unsubscribe(_ context.Context, channels ...string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range channels {
		delete(b.subs, ch)
	}
	return nil
}

func (b *MemoryBroker) Subscribed(channel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.subs[channel]
}

func (b *MemoryBroker) Messages() <-chan Message {
	return b.out
}

func (b *MemoryBroker) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

func (b *MemoryBroker) Close() error {
	b.network.mu.Lock()
	delete(b.network.brokers, b)
	b.network.mu.Unlock()
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.closed {
		b.closed = true
		close(b.out)
	}
	return nil
}
