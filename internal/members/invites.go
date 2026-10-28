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
	ExpiresIn int64
	MaxUses   int
}

func (in InviteInput) normalize() (InviteInput, time.Duration, error) {
	if err := validAssignableRole(in.Role); err != nil {
		return in, 0, err
	}
	ttl := DefaultInviteTTL
	if in.ExpiresIn != 0 {
		if in.ExpiresIn < 1 || in.ExpiresIn > MaxInviteTTLSeconds {
			return in, 0, domain.NewValidationError("expires_in", "must be between 1 and 2592000 seconds")
		}
		ttl = time.Duration(in.ExpiresIn) * time.Second
	}
	if in.MaxUses == 0 {
		in.MaxUses = DefaultInviteMaxUses
	}
	if in.MaxUses < 0 || in.MaxUses > MaxInviteMaxUses {
		return in, 0, domain.NewValidationError("max_uses", "must be between 1 and 100")
	}
	return in, ttl, nil
}

func (s *Service) CreateInvite(ctx context.Context, actorID, boardID uuid.UUID, in InviteInput) (domain.NewInvite, error) {
	clean, ttl, err := in.normalize()
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
		ExpiresAt: s.clock.Now().Add(ttl),
		MaxUses:   clean.MaxUses,
	}
	err = s.mutate(ctx, boardID, func(ctx context.Context, tx boardtx.AdminTx) (*domain.Event, error) {
		if _, err := tx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return nil, err
		}
		return nil, tx.InsertInvite(ctx, inv)
	})
	if err != nil {
		return domain.NewInvite{}, err
	}
	return domain.NewInvite{Invite: inv, Token: token}, nil
}

func (s *Service) ListInvites(ctx context.Context, actorID, boardID uuid.UUID) ([]domain.Invite, error) {
	return s.store.ActiveInvites(ctx, boardID, actorID, s.clock.Now())
}

func (s *Service) RevokeInvite(ctx context.Context, actorID, boardID, inviteID uuid.UUID) error {
	return s.mutate(ctx, boardID, func(ctx context.Context, tx boardtx.AdminTx) (*domain.Event, error) {
		if _, err := tx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return nil, err
		}
		return nil, tx.RevokeInvite(ctx, inviteID, s.clock.Now())
	})
}

func (s *Service) Accept(ctx context.Context, userID uuid.UUID, token string) (domain.BoardWithRole, error) {
	hash := HashToken(token)
	boardID, err := s.store.ActiveBoardIDByInviteHash(ctx, hash)
	if err != nil {
		return domain.BoardWithRole{}, err
	}
	var result domain.BoardWithRole
	err = s.mutate(ctx, boardID, func(ctx context.Context, tx boardtx.AdminTx) (*domain.Event, error) {
		inv, err := tx.InviteByHash(ctx, hash)
		if err != nil {
			return nil, err
		}
		existing, err := tx.MemberRole(ctx, userID)
		if err == nil {
			result = domain.BoardWithRole{Board: tx.Board(), Role: existing}
			return nil, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
		if !inv.Usable(s.clock.Now()) {
			return nil, domain.ErrInviteUnavailable
		}
		if err := tx.ConsumeInvite(ctx, inv.ID); err != nil {
			return nil, err
		}
		member, err := tx.AddMember(ctx, userID, inv.Role)
		if err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, userID, boardcontent.EventMemberAdded, addedPayload(member, userID))
		if err != nil {
			return nil, err
		}
		result = domain.BoardWithRole{Board: tx.Board(), Role: member.Role}
		return ev, nil
	})
	if err != nil {
		return domain.BoardWithRole{}, err
	}
	return result, nil
}
