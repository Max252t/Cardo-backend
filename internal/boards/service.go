package boards

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

const MaxEventsPerPage = 1000

type Store interface {
	ListBoardsForUser(ctx context.Context, userID uuid.UUID) ([]domain.BoardWithRole, error)
	CreateBoardWithOwner(ctx context.Context, b domain.Board) (domain.BoardWithRole, error)
	WithBoardLock(ctx context.Context, boardID uuid.UUID, fn boardtx.Func) error
	Snapshot(ctx context.Context, boardID, userID uuid.UUID) (domain.BoardSnapshot, error)
	EventsAfter(ctx context.Context, boardID, userID uuid.UUID, afterSeq int64, limit int) (domain.EventPage, error)
}

type Service struct {
	store     Store
	publisher boardevents.Publisher
	logger    *slog.Logger
}

type Option func(*Service)

func WithPublisher(p boardevents.Publisher) Option {
	return func(s *Service) {
		if p != nil {
			s.publisher = p
		}
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

func NewService(store Store, opts ...Option) *Service {
	s := &Service{store: store, publisher: boardevents.Nop{}, logger: slog.New(slog.DiscardHandler)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]domain.BoardWithRole, error) {
	return s.store.ListBoardsForUser(ctx, userID)
}

func (s *Service) Create(ctx context.Context, userID uuid.UUID, title string) (domain.BoardWithRole, error) {
	clean, err := domain.NormalizeBoardTitle(title)
	if err != nil {
		return domain.BoardWithRole{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.BoardWithRole{}, err
	}
	return s.store.CreateBoardWithOwner(ctx, domain.Board{ID: id, Title: clean, OwnerID: userID})
}

func (s *Service) Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error) {
	return s.store.Snapshot(ctx, boardID, userID)
}

func (s *Service) Events(ctx context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error) {
	if afterSeq < 0 {
		return domain.EventPage{}, domain.NewValidationError("after_seq", "must not be negative")
	}
	return s.store.EventsAfter(ctx, boardID, userID, afterSeq, MaxEventsPerPage)
}

func (s *Service) Rename(ctx context.Context, userID, boardID uuid.UUID, title string) (domain.BoardWithRole, domain.Event, error) {
	clean, err := domain.NormalizeBoardTitle(title)
	if err != nil {
		return domain.BoardWithRole{}, domain.Event{}, err
	}
	var (
		out domain.BoardWithRole
		ev  domain.Event
	)
	err = s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		role, err := btx.RequireRole(ctx, userID, domain.RoleOwner)
		if err != nil {
			return err
		}
		if err := btx.SetTitle(ctx, clean); err != nil {
			return err
		}
		ev, err = appendUserEvent(ctx, btx, userID, domain.EventBoardRenamed, boardcontent.BoardRenamed{Title: clean, ActorID: userID})
		if err != nil {
			return err
		}
		out = domain.BoardWithRole{Board: btx.Board(), Role: role}
		return nil
	})
	if err != nil {
		return domain.BoardWithRole{}, domain.Event{}, err
	}
	s.publish(ctx, ev)
	return out, ev, nil
}

func (s *Service) Delete(ctx context.Context, userID, boardID uuid.UUID) (domain.Event, error) {
	var ev domain.Event
	err := s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		if _, err := btx.RequireRole(ctx, userID, domain.RoleOwner); err != nil {
			return err
		}
		var err error
		if ev, err = appendUserEvent(ctx, btx, userID, domain.EventBoardDeleted, boardcontent.BoardDeleted{ActorID: userID}); err != nil {
			return err
		}
		return btx.MarkDeleted(ctx)
	})
	if err != nil {
		return domain.Event{}, err
	}
	s.publish(ctx, ev)
	return ev, nil
}

func (s *Service) publish(ctx context.Context, ev domain.Event) {
	boardevents.PublishDetached(ctx, s.publisher, s.logger, "publish board event failed", []domain.Event{ev},
		slog.String("board_id", ev.BoardID.String()),
		slog.Int64("seq", ev.Seq),
	)
}

func appendUserEvent(ctx context.Context, btx boardtx.Tx, actorID uuid.UUID, eventType string, payload any) (domain.Event, error) {
	return btx.AppendEvent(ctx, domain.EventDraft{
		Type:    eventType,
		Payload: payload,
		ActorID: &actorID,
	})
}
