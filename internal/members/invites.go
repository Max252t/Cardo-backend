package members

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

type InviteInput struct {
	Role      domain.Role
	ExpiresIn time.Duration
	MaxUses   int
}

func (in InviteInput) normalize() (InviteInput, error) {
	if err := validAssignableRole(in.Role); err != nil {
		return in, err
	}
	if in.ExpiresIn == 0 {
		in.ExpiresIn = DefaultInviteTTL
	}
	if in.ExpiresIn < 0 || in.ExpiresIn > MaxInviteTTL {
		return in, domain.NewValidationError("expires_in", "must be positive and at most 30 days")
	}
	if in.MaxUses == 0 {
		in.MaxUses = DefaultInviteMaxUses
	}
	if in.MaxUses < 0 || in.MaxUses > MaxInviteMaxUses {
		return in, domain.NewValidationError("max_uses", "must be between 1 and 100")
	}
	return in, nil
}

func (s *Service) CreateInvite(ctx context.Context, actorID, boardID uuid.UUID, in InviteInput) (domain.NewInvite, error) {
	clean, err := in.normalize()
	if err != nil {
		return domain.NewInvite{}, err
	}
	id, err := s.newID()
	if err != nil {
		return domain.NewInvite{}, err
	}
	token, err := s.newToken()
	if err != nil {
		return domain.NewInvite{}, err
	}
	inv := domain.Invite{
		ID:        id,
		BoardID:   boardID,
		TokenHash: HashToken(token),
		Role:      clean.Role,
		CreatedBy: actorID,
		ExpiresAt: s.clock.Now().Add(clean.ExpiresIn),
		MaxUses:   clean.MaxUses,
	}
	err = s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		if _, err := btx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return err
		}
		return btx.InsertInvite(ctx, inv)
	})
	if err != nil {
		return domain.NewInvite{}, err
	}
	return domain.NewInvite{Invite: inv, Token: token}, nil
}

func (s *Service) ListInvites(ctx context.Context, actorID, boardID uuid.UUID) ([]domain.Invite, error) {
	var out []domain.Invite
	err := s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		if _, err := btx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return err
		}
		var err error
		out, err = btx.ActiveInvites(ctx, s.clock.Now())
		return err
	})
	return out, err
}

func (s *Service) RevokeInvite(ctx context.Context, actorID, boardID, inviteID uuid.UUID) error {
	return s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		if _, err := btx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return err
		}
		return btx.RevokeInvite(ctx, inviteID, s.clock.Now())
	})
}

func (s *Service) Accept(ctx context.Context, userID uuid.UUID, token string) (domain.BoardWithRole, error) {
	hash := HashToken(token)
	boardID, err := s.store.BoardIDByInvite(ctx, hash)
	if err != nil {
		return domain.BoardWithRole{}, err
	}
	var (
		result domain.BoardWithRole
		ev     domain.Event
		joined bool
	)
	err = s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		inv, err := btx.InviteByHash(ctx, hash)
		if err != nil {
			return err
		}
		existing, err := btx.MemberRole(ctx, userID)
		if err == nil {
			result = domain.BoardWithRole{Board: btx.Board(), Role: existing}
			return nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if !inv.Usable(s.clock.Now()) {
			return domain.ErrInviteUnavailable
		}
		if err := btx.ConsumeInvite(ctx, inv.ID); err != nil {
			return err
		}
		member, err := btx.AddMember(ctx, userID, inv.Role)
		if err != nil {
			return err
		}
		if ev, err = appendEvent(ctx, btx, userID, boardcontent.EventMemberAdded, addedPayload(member, userID)); err != nil {
			return err
		}
		joined = true
		result = domain.BoardWithRole{Board: btx.Board(), Role: member.Role}
		return nil
	})
	if err != nil {
		return domain.BoardWithRole{}, err
	}
	if joined {
		s.publish(ctx, ev)
	}
	return result, nil
}
