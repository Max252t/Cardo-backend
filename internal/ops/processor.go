package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type Store interface {
	WithBoardLock(ctx context.Context, boardID uuid.UUID, fn storage.BoardTxFunc) error
}

type Outcome struct {
	Event    *domain.Event
	Extra    []domain.Event
	Nack     *protocol.Nack
	Replayed bool
}

func (o Outcome) Events() []domain.Event {
	if o.Event == nil {
		return nil
	}
	return append([]domain.Event{*o.Event}, o.Extra...)
}

type Processor struct {
	store     Store
	publisher pubsub.Publisher
	logger    *slog.Logger
}

type Option func(*Processor)

func WithPublisher(p pubsub.Publisher) Option {
	return func(pr *Processor) {
		if p != nil {
			pr.publisher = p
		}
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(pr *Processor) {
		if l != nil {
			pr.logger = l
		}
	}
}

func NewProcessor(store Store, opts ...Option) *Processor {
	p := &Processor{
		store:     store,
		publisher: pubsub.Nop{},
		logger:    slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

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

func (p *Processor) Apply(ctx context.Context, userID, boardID uuid.UUID, op protocol.Op) (Outcome, error) {
	if op.OpID == uuid.Nil {
		return Outcome{}, domain.NewValidationError("op_id", "is required")
	}
	var out Outcome
	err := p.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx *storage.BoardTx) error {
		var err error
		out, err = p.applyLocked(ctx, btx, userID, op)
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	if out.Event != nil && !out.Replayed {
		p.publish(ctx, boardID, op.OpID, out.Events())
	}
	return out, nil
}

func (p *Processor) publish(ctx context.Context, boardID, opID uuid.UUID, events []domain.Event) {
	if err := p.publisher.Publish(context.WithoutCancel(ctx), events); err != nil {
		p.logger.WarnContext(ctx, "publish board events failed",
			slog.String("board_id", boardID.String()),
			slog.String("op_id", opID.String()),
			slog.Int64("seq", events[0].Seq),
			slog.Any("error", err),
		)
	}
}

func (p *Processor) applyLocked(ctx context.Context, btx *storage.BoardTx, userID uuid.UUID, op protocol.Op) (Outcome, error) {
	prev, found, err := btx.ProcessedOp(ctx, op.OpID)
	if err != nil {
		return Outcome{}, fmt.Errorf("read processed op: %w", err)
	}
	if found {
		return replay(ctx, btx, prev, userID)
	}

	var res result
	err = btx.Savepoint(ctx, func(ctx context.Context, sp *storage.BoardTx) error {
		a := &applier{btx: sp, actor: userID, opID: op.OpID}
		if err := a.authorize(ctx); err != nil {
			return err
		}
		if err := a.run(ctx, op); err != nil {
			return err
		}
		res = a.res
		return nil
	})
	var rej *rejection
	if errors.As(err, &rej) {
		nack := protocol.Nack{Reason: rej.reason, AtSeq: btx.Board().Seq, Message: rej.message}
		if err := btx.SaveProcessedOp(ctx, op.OpID, userID, storedOutcome{
			Kind: outcomeNack, Reason: nack.Reason, AtSeq: nack.AtSeq, Message: nack.Message,
		}); err != nil {
			return Outcome{}, fmt.Errorf("save processed op: %w", err)
		}
		return Outcome{Nack: &nack}, nil
	}
	if err != nil {
		return Outcome{}, err
	}
	if err := btx.SaveProcessedOp(ctx, op.OpID, userID, storedOutcome{Kind: outcomeEvent, Seq: res.primary.Seq}); err != nil {
		return Outcome{}, fmt.Errorf("save processed op: %w", err)
	}
	primary := res.primary
	return Outcome{Event: &primary, Extra: res.extra}, nil
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
		return Outcome{
			Nack:     &protocol.Nack{Reason: stored.Reason, AtSeq: stored.AtSeq, Message: stored.Message},
			Replayed: true,
		}, nil
	}
	return Outcome{}, fmt.Errorf("unknown processed op outcome kind %q", stored.Kind)
}
