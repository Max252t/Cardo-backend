package protocol

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

const Version = 1

const (
	TypeNack          = "nack"
	TypeError         = "error"
	CodeProtocolError = "protocol_error"
)

var (
	ErrUnsupportedVersion = errors.New("protocol: unsupported version")
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

func ParseOp(data []byte) (boardcontent.Op, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return boardcontent.Op{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if env.V != Version {
		return boardcontent.Op{}, fmt.Errorf("%w: %d", ErrUnsupportedVersion, env.V)
	}
	if !boardcontent.IsOpType(env.Type) {
		return boardcontent.Op{}, fmt.Errorf("%w: %q", boardcontent.ErrUnknownType, env.Type)
	}
	if env.OpID == nil || *env.OpID == uuid.Nil {
		return boardcontent.Op{}, fmt.Errorf("%w: op_id is required", ErrMalformed)
	}
	return boardcontent.Op{Type: env.Type, OpID: *env.OpID, Payload: env.Payload}, nil
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

func NackEnvelope(opID uuid.UUID, n boardcontent.Nack) (Envelope, error) {
	payload, err := json.Marshal(n)
	if err != nil {
		return Envelope{}, err
	}
	id := opID
	return Envelope{V: Version, Type: TypeNack, OpID: &id, Payload: payload}, nil
}
