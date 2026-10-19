package pubsub

import (
	"context"

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
