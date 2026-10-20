package boardevents

import (
	"context"
	"log/slog"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type Publisher interface {
	Publish(ctx context.Context, events []domain.Event) error
}

type Nop struct{}

func (Nop) Publish(context.Context, []domain.Event) error {
	return nil
}

type PublisherFunc func(ctx context.Context, events []domain.Event) error

func (f PublisherFunc) Publish(ctx context.Context, events []domain.Event) error {
	return f(ctx, events)
}

func PublishDetached(ctx context.Context, p Publisher, logger *slog.Logger, failureMsg string, events []domain.Event, attrs ...any) {
	if err := p.Publish(context.WithoutCancel(ctx), events); err != nil {
		logger.WarnContext(ctx, failureMsg, append(attrs, slog.Any("error", err))...)
	}
}
