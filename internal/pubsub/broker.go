package pubsub

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

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

const (
	boardChannelPrefix    = "board:"
	presenceChannelPrefix = "presence:"
)

func BoardChannel(boardID uuid.UUID) string {
	return boardChannelPrefix + boardID.String()
}

func PresenceChannel(boardID uuid.UUID) string {
	return presenceChannelPrefix + boardID.String()
}

type ChannelKind int

const (
	ChannelUnknown ChannelKind = iota
	ChannelBoard
	ChannelPresence
)

func ParseChannel(channel string) (ChannelKind, uuid.UUID) {
	for kind, prefix := range map[ChannelKind]string{ChannelBoard: boardChannelPrefix, ChannelPresence: presenceChannelPrefix} {
		if len(channel) > len(prefix) && channel[:len(prefix)] == prefix {
			id, err := uuid.Parse(channel[len(prefix):])
			if err != nil {
				return ChannelUnknown, uuid.Nil
			}
			return kind, id
		}
	}
	return ChannelUnknown, uuid.Nil
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
		if err := p.broker.Publish(ctx, BoardChannel(ev.BoardID), data); err != nil {
			return fmt.Errorf("publish event %d: %w", ev.Seq, err)
		}
	}
	return nil
}
