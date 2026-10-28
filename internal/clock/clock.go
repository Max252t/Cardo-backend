package clock

import (
	"context"
	"time"
)

type Clock interface {
	Now() time.Time
}

type Waiter interface {
	Wait(ctx context.Context, d time.Duration) error
}

type System struct{}

func (System) Now() time.Time {
	return time.Now()
}

func (System) Wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
