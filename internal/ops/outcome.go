package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

const (
	outcomeEvent = "event"
	outcomeNack  = "nack"
)

type storedOutcome struct {
	Kind    string `json:"kind"`
	Seq     int64  `json:"seq,omitempty"`
	Reason  string `json:"reason,omitempty"`
	AtSeq   int64  `json:"at_seq,omitempty"`
	Message string `json:"message,omitempty"`
}

func eventOutcome(ev domain.Event) storedOutcome {
	return storedOutcome{Kind: outcomeEvent, Seq: ev.Seq}
}

func nackOutcome(n protocol.Nack) storedOutcome {
	return storedOutcome{Kind: outcomeNack, Reason: n.Reason, AtSeq: n.AtSeq, Message: n.Message}
}

func (s storedOutcome) nack() protocol.Nack {
	return protocol.Nack{Reason: s.Reason, AtSeq: s.AtSeq, Message: s.Message}
}

func saveOutcome(ctx context.Context, btx *storage.BoardTx, opID, userID uuid.UUID, outcome storedOutcome) error {
	if err := btx.SaveProcessedOp(ctx, opID, userID, outcome); err != nil {
		return fmt.Errorf("save processed op: %w", err)
	}
	return nil
}

func replay(ctx context.Context, btx *storage.BoardTx, prev storage.ProcessedOp, userID uuid.UUID) (Outcome, error) {
	if prev.BoardID != btx.Board().ID || prev.UserID != userID {
		return Outcome{Nack: &protocol.Nack{
			Reason:  protocol.ReasonValidation,
			AtSeq:   btx.Board().Seq,
			Message: "op_id was already used by another operation",
		}}, nil
	}
	var stored storedOutcome
	if err := json.Unmarshal(prev.Outcome, &stored); err != nil {
		return Outcome{}, fmt.Errorf("decode processed op outcome: %w", err)
	}
	switch stored.Kind {
	case outcomeEvent:
		ev, err := btx.EventBySeq(ctx, stored.Seq)
		if err != nil {
			return Outcome{}, fmt.Errorf("read replayed event %d: %w", stored.Seq, err)
		}
		return Outcome{Event: &ev, Replayed: true}, nil
	case outcomeNack:
		nack := stored.nack()
		return Outcome{Nack: &nack, Replayed: true}, nil
	}
	return Outcome{}, fmt.Errorf("unknown processed op outcome kind %q", stored.Kind)
}
