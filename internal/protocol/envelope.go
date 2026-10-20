package protocol

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const Version = 1

const (
	TypeNack          = "nack"
	TypeError         = "error"
	CodeProtocolError = "protocol_error"
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

func NewOp(opType string, opID uuid.UUID, payload any) (Op, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Op{}, err
	}
	return Op{Type: opType, OpID: opID, Payload: data}, nil
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
