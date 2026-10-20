package protocol

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

const (
	TypeHello         = "hello"
	TypeAuthRefresh   = "auth.refresh"
	TypeAuthRefreshed = "auth.refreshed"
	TypeSyncDelta     = "sync.delta"
	TypeSyncSnapshot  = "sync.snapshot"
	TypeSyncReady     = "sync.ready"
	TypePresenceJoin  = "presence.join"
	TypePresenceLeave = "presence.leave"
	TypePresenceList  = "presence.list"

	CodeInvalidToken = "invalid_token"
)

type Hello struct {
	LastSeq *int64 `json:"last_seq"`
	Client  string `json:"client,omitempty"`
}

type AuthRefresh struct {
	AccessToken string `json:"access_token"`
}

type AuthRefreshed struct {
	ExpiresAt int64 `json:"expires_at"`
}

type SyncDelta struct {
	Events []Envelope `json:"events"`
}

type PresenceUser struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	AvatarColor string    `json:"avatar_color"`
}

type PresenceJoin struct {
	User PresenceUser `json:"user"`
}

type PresenceLeave struct {
	UserID uuid.UUID `json:"user_id"`
}

type PresenceList struct {
	Users []PresenceUser `json:"users"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func ParseEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, fmt.Errorf("%w: %w", ErrMalformed, err)
	}
	if env.V != Version {
		return Envelope{}, fmt.Errorf("%w: %d", ErrUnsupportedVersion, env.V)
	}
	if env.Type == "" {
		return Envelope{}, fmt.Errorf("%w: type is required", ErrMalformed)
	}
	return env, nil
}

func (e Envelope) AsOp() (Op, error) {
	if !IsOpType(e.Type) {
		return Op{}, fmt.Errorf("%w: %q", ErrUnknownType, e.Type)
	}
	if e.OpID == nil || *e.OpID == uuid.Nil {
		return Op{}, fmt.Errorf("%w: op_id is required", ErrMalformed)
	}
	return Op{Type: e.Type, OpID: *e.OpID, Payload: e.Payload}, nil
}

func NewEnvelope(msgType string, payload any) (Envelope, error) {
	env := Envelope{V: Version, Type: msgType}
	if payload == nil {
		return env, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal %s payload: %w", msgType, err)
	}
	env.Payload = data
	return env, nil
}

func Encode(msgType string, payload any) ([]byte, error) {
	env, err := NewEnvelope(msgType, payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(env)
}
