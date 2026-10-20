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
	outcomeVersion = 1
	outcomeEvent   = "event"
	outcomeNack    = "nack"
)

type storedOutcome struct {
	V       int    `json:"v"`
	Kind    string `json:"kind"`
	Seq     int64  `json:"seq,omitempty"`
	Reason  string `json:"reason,omitempty"`
	AtSeq   int64  `json:"at_seq,omitempty"`
	Message string `json:"message,omitempty"`
}

func eventOutcome(ev domain.Event) storedOutcome {
	return storedOutcome{V: outcomeVersion, Kind: outcomeEvent, Seq: ev.Seq}
}

func nackOutcome(n protocol.Nack) storedOutcome {
	return storedOutcome{V: outcomeVersion, Kind: outcomeNack, Reason: n.Reason, AtSeq: n.AtSeq, Message: n.Message}
}

func (s storedOutcome) nack() protocol.Nack {
	return protocol.Nack{Reason: s.Reason, AtSeq: s.AtSeq, Message: s.Message}
}

func saveOutcome(ctx context.Context, btx *storage.BoardTx, opID, userID uuid.UUID, outcome storedOutcome) (bool, error) {
	saved, err := btx.SaveProcessedOp(ctx, opID, userID, outcome)
	if err != nil {
		return false, fmt.Errorf("save processed op: %w", err)
	}
	return saved, nil
}

func replay(ctx context.Context, btx *storage.BoardTx, prev storage.ProcessedOp, userID uuid.UUID) (Outcome, error) {
	if prev.BoardID != btx.Board().ID || prev.UserID != userID {
		return transientNack(btx, opIDTaken()), nil
	}
	var stored storedOutcome
	if err := json.Unmarshal(prev.Outcome, &stored); err != nil {
		return Outcome{}, fmt.Errorf("decode processed op outcome: %w", err)
	}
	if stored.V != outcomeVersion {
		return Outcome{}, fmt.Errorf("unsupported processed op outcome version %d", stored.V)
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

func transientNack(btx *storage.BoardTx, rej *rejection) Outcome {
	return Outcome{Nack: &protocol.Nack{Reason: rej.reason, AtSeq: btx.Board().Seq, Message: rej.message}}
}
