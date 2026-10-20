package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
)

type Store interface {
	WithBoardLock(ctx context.Context, boardID uuid.UUID, fn boardtx.Func) error
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
	events := make([]domain.Event, 0, len(o.Extra)+1)
	events = append(events, o.Extra...)
	return append(events, *o.Event)
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

func (p *Processor) Apply(ctx context.Context, userID, boardID uuid.UUID, op protocol.Op) (Outcome, error) {
	if op.OpID == uuid.Nil {
		return Outcome{}, domain.NewValidationError("op_id", "is required")
	}
	var out Outcome
	err := p.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		var err error
		out, err = applyLocked(ctx, btx, userID, op)
		return err
	})
	if err != nil {
		return Outcome{}, err
	}
	if out.Event != nil && !out.Replayed {
		events := out.Events()
		pubsub.PublishDetached(ctx, p.publisher, p.logger, "publish board events failed", events,
			slog.String("board_id", boardID.String()),
			slog.String("op_id", op.OpID.String()),
			slog.Int64("seq", events[0].Seq),
		)
	}
	return out, nil
}

func applyLocked(ctx context.Context, btx boardtx.Tx, userID uuid.UUID, op protocol.Op) (Outcome, error) {
	prev, found, err := btx.ProcessedOp(ctx, op.OpID)
	if err != nil {
		return Outcome{}, fmt.Errorf("read processed op: %w", err)
	}
	if found {
		return replay(ctx, btx, prev, userID)
	}

	res, err := execute(ctx, btx, userID, op)
	var rej *rejection
	if errors.As(err, &rej) {
		return rejected(ctx, btx, op.OpID, userID, rej)
	}
	if err != nil {
		return Outcome{}, err
	}
	primary := res.primary
	return Outcome{Event: &primary, Extra: res.extra}, nil
}

func execute(ctx context.Context, btx boardtx.Tx, userID uuid.UUID, op protocol.Op) (result, error) {
	var res result
	err := btx.Savepoint(ctx, func(ctx context.Context, sp boardtx.Tx) error {
		a := &applier{btx: sp, actor: userID, opID: op.OpID}
		if err := a.authorize(ctx); err != nil {
			return err
		}
		if err := a.run(ctx, op); err != nil {
			return err
		}
		saved, err := saveOutcome(ctx, sp, op.OpID, userID, eventOutcome(a.res.primary))
		if err != nil {
			return err
		}
		if !saved {
			return opIDTaken()
		}
		res = a.res
		return nil
	})
	return res, err
}

func rejected(ctx context.Context, btx boardtx.Tx, opID, userID uuid.UUID, rej *rejection) (Outcome, error) {
	if rej.transient {
		return transientNack(btx, rej), nil
	}
	nack := protocol.Nack{Reason: rej.reason, AtSeq: btx.Board().Seq, Message: rej.message}
	saved, err := saveOutcome(ctx, btx, opID, userID, nackOutcome(nack))
	if err != nil {
		return Outcome{}, err
	}
	if !saved {
		return transientNack(btx, opIDTaken()), nil
	}
	return Outcome{Nack: &nack}, nil
}
