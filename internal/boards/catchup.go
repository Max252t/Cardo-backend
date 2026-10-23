package boards

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type History interface {
	Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error)
	Events(ctx context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error)
}

type CatchUpLimits struct {
	MaxEvents int
}

func (s *Service) CatchUp(ctx context.Context, userID, boardID uuid.UUID, lastSeq *int64, limits CatchUpLimits) (domain.CatchUp, error) {
	return CatchUp(ctx, s, userID, boardID, lastSeq, limits)
}

func CatchUp(ctx context.Context, h History, userID, boardID uuid.UUID, lastSeq *int64, limits CatchUpLimits) (domain.CatchUp, error) {
	if lastSeq != nil {
		events, ok, err := collectDelta(ctx, h, userID, boardID, *lastSeq, limits.MaxEvents)
		if err != nil {
			return domain.CatchUp{}, err
		}
		if ok {
			return domain.CatchUp{Events: events, Seq: deltaSeq(*lastSeq, events)}, nil
		}
	}
	snap, err := h.Snapshot(ctx, userID, boardID)
	if err != nil {
		return domain.CatchUp{}, fmt.Errorf("load snapshot: %w", err)
	}
	return domain.CatchUp{Snapshot: &snap, Seq: snap.Seq}, nil
}

func collectDelta(ctx context.Context, h History, userID, boardID uuid.UUID, lastSeq int64, maxEvents int) ([]domain.Event, bool, error) {
	var events []domain.Event
	after := lastSeq
	for {
		page, err := h.Events(ctx, userID, boardID, after)
		var vErr *domain.ValidationError
		switch {
		case errors.Is(err, domain.ErrGone), errors.As(err, &vErr):
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("load events after %d: %w", after, err)
		}
		events = append(events, page.Events...)
		if len(events) > maxEvents {
			return nil, false, nil
		}
		if !page.HasMore {
			return events, true, nil
		}
		if len(page.Events) == 0 {
			return nil, false, nil
		}
		after = page.Events[len(page.Events)-1].Seq
	}
}

func deltaSeq(lastSeq int64, events []domain.Event) int64 {
	seq := lastSeq
	for _, ev := range events {
		seq = max(seq, ev.Seq)
	}
	return seq
}
