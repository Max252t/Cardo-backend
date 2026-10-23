package boardcontent

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

const (
	ReasonVersionConflict = "version_conflict"
	ReasonNotFound        = "not_found"
	ReasonForbidden       = "forbidden"
	ReasonValidation      = "validation_error"
	ReasonRateLimited     = "rate_limited"
	ReasonInternal        = "internal"
)

var ErrUnknownType = errors.New("protocol: unknown message type")

type Op struct {
	Type    string
	OpID    uuid.UUID
	Payload json.RawMessage
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
