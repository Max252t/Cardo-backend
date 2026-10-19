package boards

import (
	"context"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type Service struct {
	store *storage.Store
}

func NewService(store *storage.Store) *Service {
	return &Service{store: store}
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

func (s *Service) Rename(ctx context.Context, userID, boardID uuid.UUID, title string) (domain.BoardWithRole, domain.Event, error) {
	clean, err := domain.NormalizeBoardTitle(title)
	if err != nil {
		return domain.BoardWithRole{}, domain.Event{}, err
	}
	var (
		out domain.BoardWithRole
		ev  domain.Event
	)
	err = s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx *storage.BoardTx) error {
		role, err := btx.RequireRole(ctx, userID, domain.RoleOwner)
		if err != nil {
			return err
		}
		if err := btx.SetTitle(ctx, clean); err != nil {
			return err
		}
		ev, err = btx.AppendEvent(ctx, storage.EventInput{
			Type:    domain.EventBoardRenamed,
			Payload: map[string]string{"title": clean},
			ActorID: &userID,
		})
		if err != nil {
			return err
		}
		out = domain.BoardWithRole{Board: btx.Board(), Role: role}
		return nil
	})
	return out, ev, err
}

func (s *Service) Delete(ctx context.Context, userID, boardID uuid.UUID) (domain.Event, error) {
	var ev domain.Event
	err := s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx *storage.BoardTx) error {
		if _, err := btx.RequireRole(ctx, userID, domain.RoleOwner); err != nil {
			return err
		}
		var err error
		ev, err = btx.AppendEvent(ctx, storage.EventInput{
			Type:    domain.EventBoardDeleted,
			Payload: map[string]string{},
			ActorID: &userID,
		})
		if err != nil {
			return err
		}
		return btx.MarkDeleted(ctx)
	})
	return ev, err
}
