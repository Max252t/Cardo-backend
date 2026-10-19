package users

import (
	"context"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type Store interface {
	GetUserByID(ctx context.Context, id uuid.UUID) (domain.User, error)
	UpdateUserProfile(ctx context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error)
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return s.store.GetUserByID(ctx, id)
}

func (s *Service) UpdateProfile(ctx context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error) {
	var clean domain.ProfileUpdate
	if upd.DisplayName != nil {
		name, err := domain.NormalizeDisplayName(*upd.DisplayName)
		if err != nil {
			return domain.User{}, err
		}
		clean.DisplayName = &name
	}
	if upd.AvatarColor != nil {
		color, err := domain.NormalizeAvatarColor(*upd.AvatarColor)
		if err != nil {
			return domain.User{}, err
		}
		clean.AvatarColor = &color
	}
	if clean.DisplayName == nil && clean.AvatarColor == nil {
		return s.store.GetUserByID(ctx, id)
	}
	return s.store.UpdateUserProfile(ctx, id, clean)
}
