package ops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

type Store interface {
	WithBoardLock(ctx context.Context, boardID uuid.UUID, fn boardtx.Func) error
}

type Processor struct {
	store     Store
	publisher boardevents.Publisher
	logger    *slog.Logger
	metrics   Metrics
}

type Option func(*Processor)

func WithPublisher(p boardevents.Publisher) Option {
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
		publisher: boardevents.Nop{},
		logger:    slog.New(slog.DiscardHandler),
		metrics:   nopMetrics{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Processor) Apply(ctx context.Context, userID, boardID uuid.UUID, op boardcontent.Op) (boardcontent.OpOutcome, error) {
	if op.OpID == uuid.Nil {
		p.metrics.ObserveOp(metricType(op.Type), boardcontent.ReasonValidation, 0)
		return boardcontent.OpOutcome{}, domain.NewValidationError("op_id", "is required")
	}
	start := time.Now()
	out, err := p.apply(ctx, userID, boardID, op)
	p.metrics.ObserveOp(metricType(op.Type), metricResult(out, err), time.Since(start))
	if err != nil {
		return boardcontent.OpOutcome{}, err
	}
	p.publish(ctx, boardID, op, out)
	return out, nil
}

func (p *Processor) apply(ctx context.Context, userID, boardID uuid.UUID, op boardcontent.Op) (boardcontent.OpOutcome, error) {
	var out boardcontent.OpOutcome
	err := p.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		var err error
		out, err = applyLocked(ctx, btx, userID, op)
		return err
	})
	if err != nil {
		return boardcontent.OpOutcome{}, err
	}
	return out, nil
}

func (p *Processor) publish(ctx context.Context, boardID uuid.UUID, op boardcontent.Op, out boardcontent.OpOutcome) {
	if out.Event == nil || out.Replayed {
		return
	}
	events := out.Events()
	boardevents.PublishDetached(ctx, p.publisher, p.logger, "publish board events failed", events,
		slog.String("board_id", boardID.String()),
		slog.String("op_id", op.OpID.String()),
		slog.Int64("seq", events[0].Seq),
	)
}

func applyLocked(ctx context.Context, btx boardtx.Tx, userID uuid.UUID, op boardcontent.Op) (boardcontent.OpOutcome, error) {
	prev, found, err := btx.ProcessedOp(ctx, op.OpID)
	if err != nil {
		return boardcontent.OpOutcome{}, fmt.Errorf("read processed op: %w", err)
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
		return boardcontent.OpOutcome{}, err
	}
	primary := res.primary
	return boardcontent.OpOutcome{Event: &primary, Extra: res.extra}, nil
}

func execute(ctx context.Context, btx boardtx.Tx, userID uuid.UUID, op boardcontent.Op) (result, error) {
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

func rejected(ctx context.Context, btx boardtx.Tx, opID, userID uuid.UUID, rej *rejection) (boardcontent.OpOutcome, error) {
	if rej.transient {
		return transientNack(btx, rej), nil
	}
	nack := boardcontent.Nack{Reason: rej.reason, AtSeq: btx.Board().Seq, Message: rej.message}
	saved, err := saveOutcome(ctx, btx, opID, userID, nackOutcome(nack))
	if err != nil {
		return boardcontent.OpOutcome{}, err
	}
	if !saved {
		return transientNack(btx, opIDTaken()), nil
	}
	return boardcontent.OpOutcome{Nack: &nack}, nil
}
