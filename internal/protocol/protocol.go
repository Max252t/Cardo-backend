package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const Version = 1

const (
	OpCardCreate      = "op.card.create"
	OpCardEdit        = "op.card.edit"
	OpCardMove        = "op.card.move"
	OpCardArchive     = "op.card.archive"
	OpColumnCreate    = "op.column.create"
	OpColumnRename    = "op.column.rename"
	OpColumnMove      = "op.column.move"
	OpColumnArchive   = "op.column.archive"
	TypeNack          = "nack"
	TypeError         = "error"
	CodeProtocolError = "protocol_error"
)

const (
	EventCardCreated       = "card.created"
	EventCardEdited        = "card.edited"
	EventCardMoved         = "card.moved"
	EventCardArchived      = "card.archived"
	EventColumnCreated     = "column.created"
	EventColumnRenamed     = "column.renamed"
	EventColumnMoved       = "column.moved"
	EventColumnArchived    = "column.archived"
	EventColumnRebalanced  = "column.rebalanced"
	EventColumnsRebalanced = "columns.rebalanced"
	EventBoardRenamed      = domain.EventBoardRenamed
	EventBoardDeleted      = domain.EventBoardDeleted
)

const (
	ReasonVersionConflict = "version_conflict"
	ReasonNotFound        = "not_found"
	ReasonForbidden       = "forbidden"
	ReasonValidation      = "validation_error"
	ReasonRateLimited     = "rate_limited"
	ReasonInternal        = "internal"
)

var (
	ErrUnsupportedVersion = errors.New("protocol: unsupported version")
	ErrUnknownType        = errors.New("protocol: unknown message type")
	ErrMalformed          = errors.New("protocol: malformed message")
)

var opTypes = map[string]bool{
	OpCardCreate:    true,
	OpCardEdit:      true,
	OpCardMove:      true,
	OpCardArchive:   true,
	OpColumnCreate:  true,
	OpColumnRename:  true,
	OpColumnMove:    true,
	OpColumnArchive: true,
}

func IsOpType(t string) bool {
	return opTypes[t]
}

type Envelope struct {
	V        int             `json:"v"`
	Type     string          `json:"type"`
	OpID     *uuid.UUID      `json:"op_id,omitempty"`
	Seq      int64           `json:"seq,omitempty"`
	ServerTS int64           `json:"server_ts,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

type Op struct {
	Type    string
	OpID    uuid.UUID
	Payload json.RawMessage
}

func ParseOp(data []byte) (Op, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Op{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if env.V != Version {
		return Op{}, fmt.Errorf("%w: %d", ErrUnsupportedVersion, env.V)
	}
	if !IsOpType(env.Type) {
		return Op{}, fmt.Errorf("%w: %q", ErrUnknownType, env.Type)
	}
	if env.OpID == nil || *env.OpID == uuid.Nil {
		return Op{}, fmt.Errorf("%w: op_id is required", ErrMalformed)
	}
	return Op{Type: env.Type, OpID: *env.OpID, Payload: env.Payload}, nil
}

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

func DecodePayload(op Op) (any, error) {
	var dst any
	switch op.Type {
	case OpCardCreate:
		dst = &CardCreate{}
	case OpCardEdit:
		dst = &CardEdit{}
	case OpCardMove:
		dst = &CardMove{}
	case OpCardArchive:
		dst = &CardArchive{}
	case OpColumnCreate:
		dst = &ColumnCreate{}
	case OpColumnRename:
		dst = &ColumnRename{}
	case OpColumnMove:
		dst = &ColumnMove{}
	case OpColumnArchive:
		dst = &ColumnArchive{}
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownType, op.Type)
	}
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

type Card struct {
	ID                 uuid.UUID `json:"id"`
	ColumnID           uuid.UUID `json:"column_id"`
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	Rank               string    `json:"rank"`
	Version            int64     `json:"version"`
	DescriptionVersion int64     `json:"description_version"`
	CreatedBy          uuid.UUID `json:"created_by"`
	UpdatedBy          uuid.UUID `json:"updated_by"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func FromCard(c domain.Card) Card {
	return Card{
		ID:                 c.ID,
		ColumnID:           c.ColumnID,
		Title:              c.Title,
		Description:        c.Description,
		Rank:               c.Rank,
		Version:            c.Version,
		DescriptionVersion: c.DescriptionVersion,
		CreatedBy:          c.CreatedBy,
		UpdatedBy:          c.UpdatedBy,
		CreatedAt:          c.CreatedAt.UTC(),
		UpdatedAt:          c.UpdatedAt.UTC(),
	}
}

type Column struct {
	ID      uuid.UUID `json:"id"`
	Title   string    `json:"title"`
	Rank    string    `json:"rank"`
	Version int64     `json:"version"`
}

func FromColumn(c domain.Column) Column {
	return Column{ID: c.ID, Title: c.Title, Rank: c.Rank, Version: c.Version}
}

type CardCreated struct {
	Card
	ActorID uuid.UUID `json:"actor_id"`
}

type CardEdited struct {
	CardID             uuid.UUID `json:"card_id"`
	Title              *string   `json:"title,omitempty"`
	Description        *string   `json:"description,omitempty"`
	Version            int64     `json:"version"`
	DescriptionVersion int64     `json:"description_version"`
	ActorID            uuid.UUID `json:"actor_id"`
}

type CardMoved struct {
	CardID       uuid.UUID `json:"card_id"`
	FromColumnID uuid.UUID `json:"from_column_id"`
	ToColumnID   uuid.UUID `json:"to_column_id"`
	Rank         string    `json:"rank"`
	Version      int64     `json:"version"`
	ActorID      uuid.UUID `json:"actor_id"`
}

type CardArchived struct {
	CardID   uuid.UUID `json:"card_id"`
	ColumnID uuid.UUID `json:"column_id"`
	Version  int64     `json:"version"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type ColumnCreated struct {
	Column
	ActorID uuid.UUID `json:"actor_id"`
}

type ColumnRenamed struct {
	ColumnID uuid.UUID `json:"column_id"`
	Title    string    `json:"title"`
	Version  int64     `json:"version"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type ColumnMoved struct {
	ColumnID uuid.UUID `json:"column_id"`
	Rank     string    `json:"rank"`
	Version  int64     `json:"version"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type ColumnArchived struct {
	ColumnID uuid.UUID   `json:"column_id"`
	CardIDs  []uuid.UUID `json:"card_ids"`
	Version  int64       `json:"version"`
	ActorID  uuid.UUID   `json:"actor_id"`
}

type CardRank struct {
	CardID uuid.UUID `json:"card_id"`
	Rank   string    `json:"rank"`
}

type ColumnRebalanced struct {
	ColumnID uuid.UUID  `json:"column_id"`
	Cards    []CardRank `json:"cards"`
	ActorID  uuid.UUID  `json:"actor_id"`
}

type ColumnRank struct {
	ColumnID uuid.UUID `json:"column_id"`
	Rank     string    `json:"rank"`
}

type ColumnsRebalanced struct {
	Columns []ColumnRank `json:"columns"`
	ActorID uuid.UUID    `json:"actor_id"`
}

type BoardRenamed struct {
	Title   string    `json:"title"`
	ActorID uuid.UUID `json:"actor_id"`
}

type BoardDeleted struct {
	ActorID uuid.UUID `json:"actor_id"`
}

type Nack struct {
	Reason  string `json:"reason"`
	AtSeq   int64  `json:"at_seq"`
	Message string `json:"message"`
}

func EventEnvelope(ev domain.Event) Envelope {
	return Envelope{
		V:        Version,
		Type:     ev.Type,
		OpID:     ev.OpID,
		Seq:      ev.Seq,
		ServerTS: ev.CreatedAt.UnixMilli(),
		Payload:  ev.Payload,
	}
}

func NackEnvelope(opID uuid.UUID, n Nack) (Envelope, error) {
	payload, err := json.Marshal(n)
	if err != nil {
		return Envelope{}, err
	}
	id := opID
	return Envelope{V: Version, Type: TypeNack, OpID: &id, Payload: payload}, nil
}

type Member struct {
	UserID      uuid.UUID   `json:"user_id"`
	DisplayName string      `json:"display_name"`
	AvatarColor string      `json:"avatar_color"`
	Role        domain.Role `json:"role"`
	JoinedAt    time.Time   `json:"joined_at"`
}

type BoardInfo struct {
	ID        uuid.UUID   `json:"id"`
	Title     string      `json:"title"`
	OwnerID   uuid.UUID   `json:"owner_id"`
	Role      domain.Role `json:"role"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type Snapshot struct {
	Board   BoardInfo `json:"board"`
	Columns []Column  `json:"columns"`
	Cards   []Card    `json:"cards"`
	Members []Member  `json:"members"`
	Seq     int64     `json:"seq"`
}

func FromSnapshot(s domain.BoardSnapshot) Snapshot {
	out := Snapshot{
		Board: BoardInfo{
			ID:        s.Board.ID,
			Title:     s.Board.Title,
			OwnerID:   s.Board.OwnerID,
			Role:      s.Board.Role,
			CreatedAt: s.Board.CreatedAt.UTC(),
			UpdatedAt: s.Board.UpdatedAt.UTC(),
		},
		Columns: make([]Column, 0, len(s.Columns)),
		Cards:   make([]Card, 0, len(s.Cards)),
		Members: make([]Member, 0, len(s.Members)),
		Seq:     s.Seq,
	}
	for _, c := range s.Columns {
		out.Columns = append(out.Columns, FromColumn(c))
	}
	for _, c := range s.Cards {
		out.Cards = append(out.Cards, FromCard(c))
	}
	for _, m := range s.Members {
		out.Members = append(out.Members, Member{
			UserID:      m.UserID,
			DisplayName: m.DisplayName,
			AvatarColor: m.AvatarColor,
			Role:        m.Role,
			JoinedAt:    m.JoinedAt.UTC(),
		})
	}
	return out
}

type EventsPage struct {
	Events  []Envelope `json:"events"`
	Seq     int64      `json:"seq"`
	HasMore bool       `json:"has_more"`
}

func FromEventPage(p domain.EventPage) EventsPage {
	out := EventsPage{Events: make([]Envelope, 0, len(p.Events)), Seq: p.Seq, HasMore: p.HasMore}
	for _, ev := range p.Events {
		out.Events = append(out.Events, EventEnvelope(ev))
	}
	return out
}
