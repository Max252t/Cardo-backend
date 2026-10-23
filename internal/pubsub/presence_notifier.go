package pubsub

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type PresenceNotifier struct {
	broker Broker
}

var _ presence.Notifier = (*PresenceNotifier)(nil)

func NewPresenceNotifier(broker Broker) *PresenceNotifier {
	return &PresenceNotifier{broker: broker}
}

func (n *PresenceNotifier) Joined(ctx context.Context, boardID uuid.UUID, user domain.PresenceUser) error {
	return n.publish(ctx, boardID, protocol.TypePresenceJoin, protocol.PresenceJoin{User: protocol.FromPresenceUser(user)})
}

func (n *PresenceNotifier) Left(ctx context.Context, boardID, userID uuid.UUID) error {
	return n.publish(ctx, boardID, protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: userID})
}

func (n *PresenceNotifier) publish(ctx context.Context, boardID uuid.UUID, msgType string, payload any) error {
	data, err := protocol.Encode(msgType, payload)
	if err != nil {
		return err
	}
	if err := n.broker.Publish(ctx, protocol.PresenceChannel(boardID), data); err != nil {
		return fmt.Errorf("publish %s: %w", msgType, err)
	}
	return nil
}
