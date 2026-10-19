package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	OpCardCreate    = "op.card.create"
	OpCardEdit      = "op.card.edit"
	OpCardMove      = "op.card.move"
	OpCardArchive   = "op.card.archive"
	OpColumnCreate  = "op.column.create"
	OpColumnRename  = "op.column.rename"
	OpColumnMove    = "op.column.move"
	OpColumnArchive = "op.column.archive"
)

type CardCreate struct {
	CardID      uuid.UUID  `json:"card_id"`
	ColumnID    uuid.UUID  `json:"column_id"`
	Title       string     `json:"title"`
	Description *string    `json:"description,omitempty"`
	AfterID     *uuid.UUID `json:"after_id,omitempty"`
	BeforeID    *uuid.UUID `json:"before_id,omitempty"`
}

type CardEdit struct {
	CardID                 uuid.UUID `json:"card_id"`
	Title                  *string   `json:"title,omitempty"`
	Description            *string   `json:"description,omitempty"`
	BaseDescriptionVersion *int64    `json:"base_description_version,omitempty"`
}

type CardMove struct {
	CardID      uuid.UUID  `json:"card_id"`
	ToColumnID  uuid.UUID  `json:"to_column_id"`
	AfterID     *uuid.UUID `json:"after_id,omitempty"`
	BeforeID    *uuid.UUID `json:"before_id,omitempty"`
	BaseVersion *int64     `json:"base_version,omitempty"`
}

type CardArchive struct {
	CardID uuid.UUID `json:"card_id"`
}

type ColumnCreate struct {
	ColumnID uuid.UUID  `json:"column_id"`
	Title    string     `json:"title"`
	AfterID  *uuid.UUID `json:"after_id,omitempty"`
	BeforeID *uuid.UUID `json:"before_id,omitempty"`
}

type ColumnRename struct {
	ColumnID uuid.UUID `json:"column_id"`
	Title    string    `json:"title"`
}

type ColumnMove struct {
	ColumnID    uuid.UUID  `json:"column_id"`
	AfterID     *uuid.UUID `json:"after_id,omitempty"`
	BeforeID    *uuid.UUID `json:"before_id,omitempty"`
	BaseVersion *int64     `json:"base_version,omitempty"`
}

type ColumnArchive struct {
	ColumnID uuid.UUID `json:"column_id"`
}

var opPayloads = map[string]func() any{
	OpCardCreate:    func() any { return &CardCreate{} },
	OpCardEdit:      func() any { return &CardEdit{} },
	OpCardMove:      func() any { return &CardMove{} },
	OpCardArchive:   func() any { return &CardArchive{} },
	OpColumnCreate:  func() any { return &ColumnCreate{} },
	OpColumnRename:  func() any { return &ColumnRename{} },
	OpColumnMove:    func() any { return &ColumnMove{} },
	OpColumnArchive: func() any { return &ColumnArchive{} },
}

func IsOpType(t string) bool {
	_, ok := opPayloads[t]
	return ok
}

func DecodePayload(op Op) (any, error) {
	newPayload, ok := opPayloads[op.Type]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, op.Type)
	}
	dst := newPayload()
	if err := decodeStrict(op.Payload, dst); err != nil {
		return nil, err
	}
	return dst, nil
}

func decodeStrict(data json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return domain.NewValidationError("payload", "is required")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return domain.NewValidationError("payload", describeDecodeError(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return domain.NewValidationError("payload", "must be a single JSON object")
	}
	return nil
}

func describeDecodeError(err error) string {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return "field " + typeErr.Field + " has an invalid type"
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "json: unknown field ") {
		return "unknown field " + strings.TrimPrefix(msg, "json: unknown field ")
	}
	return "is not a valid JSON object"
}
