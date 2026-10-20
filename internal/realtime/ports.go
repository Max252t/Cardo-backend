package realtime

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type Ticker interface {
	C() <-chan time.Time
	Stop()
}

type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type Clock interface {
	Now() time.Time
	NewTicker(d time.Duration) Ticker
	NewTimer(d time.Duration) Timer
}

type Bus interface {
	Subscribe(ctx context.Context, channels ...string) error
	Unsubscribe(ctx context.Context, channels ...string) error
}

type Presence interface {
	Join(ctx context.Context, boardID uuid.UUID, user protocol.PresenceUser) error
	Leave(ctx context.Context, boardID, userID uuid.UUID) error
	Touch(ctx context.Context, boardID uuid.UUID, users []protocol.PresenceUser) error
	Online(ctx context.Context, boardID uuid.UUID) ([]protocol.PresenceUser, error)
	Sweep(ctx context.Context, boardID uuid.UUID) error
}

type OpApplier interface {
	Apply(ctx context.Context, userID, boardID uuid.UUID, op protocol.Op) (protocol.OpOutcome, error)
}

type SyncSource interface {
	Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error)
	Events(ctx context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error)
}

type MemberLookup interface {
	BoardMember(ctx context.Context, boardID, userID uuid.UUID) (domain.Member, error)
}

type TokenParser interface {
	Parse(token string) (domain.AccessClaims, error)
}
