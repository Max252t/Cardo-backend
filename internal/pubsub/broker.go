package pubsub

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type Message struct {
	Channel string
	Payload []byte
}

type Broker interface {
	Publish(ctx context.Context, channel string, payload []byte) error
	Subscribe(ctx context.Context, channels ...string) error
	Unsubscribe(ctx context.Context, channels ...string) error
	Messages() <-chan Message
	Close() error
}

type EventPublisher struct {
	broker Broker
}

func NewEventPublisher(broker Broker) *EventPublisher {
	return &EventPublisher{broker: broker}
}

func (p *EventPublisher) Publish(ctx context.Context, events []domain.Event) error {
	for _, ev := range events {
		data, err := json.Marshal(protocol.EventEnvelope(ev))
		if err != nil {
			return fmt.Errorf("marshal event %d: %w", ev.Seq, err)
		}
		if err := p.broker.Publish(ctx, protocol.BoardChannel(ev.BoardID), data); err != nil {
			return fmt.Errorf("publish event %d: %w", ev.Seq, err)
		}
	}
	return nil
}

func Pump(broker Broker, handle func(channel string, payload []byte)) {
	for m := range broker.Messages() {
		handle(m.Channel, m.Payload)
	}
}
