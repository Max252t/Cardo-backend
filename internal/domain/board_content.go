package domain

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

var ErrDuplicateID = errors.New("id already exists")

type EventDraft struct {
	Type    string
	Payload any
	OpID    *uuid.UUID
	ActorID *uuid.UUID
}

type RankedItem struct {
	ID   uuid.UUID
	Rank string
}

type CardEdit struct {
	Title       *string
	Description *string
}

type ProcessedOp struct {
	OpID    uuid.UUID
	BoardID uuid.UUID
	UserID  uuid.UUID
	Outcome json.RawMessage
}
