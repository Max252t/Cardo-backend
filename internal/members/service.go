package members

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/clock"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/secrets"
)

const (
	DefaultInviteTTL     = 7 * 24 * time.Hour
	MaxInviteTTL         = 30 * 24 * time.Hour
	MaxInviteTTLSeconds  = int64(MaxInviteTTL / time.Second)
	DefaultInviteMaxUses = 1
	MaxInviteMaxUses     = 100
	tokenBytes           = 32
)

type Store interface {
	WithAdminLock(ctx context.Context, boardID uuid.UUID, fn boardtx.AdminFunc) error
	BoardMembers(ctx context.Context, boardID, userID uuid.UUID) ([]domain.Member, error)
	ActiveInvites(ctx context.Context, boardID, userID uuid.UUID, now time.Time) ([]domain.Invite, error)
	ActiveBoardIDByInviteHash(ctx context.Context, tokenHash string) (uuid.UUID, error)
}

type Service struct {
	store     Store
	publisher boardevents.Publisher
	logger    *slog.Logger
	clock     clock.Clock
	newID     func() (uuid.UUID, error)
	newToken  func() (string, error)
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

func WithClock(c clock.Clock) Option {
	return func(s *Service) {
		if c != nil {
			s.clock = c
		}
	}
}

func WithIDGenerator(fn func() (uuid.UUID, error)) Option {
	return func(s *Service) {
		if fn != nil {
			s.newID = fn
		}
	}
}

func WithTokenGenerator(fn func() (string, error)) Option {
	return func(s *Service) {
		if fn != nil {
			s.newToken = fn
		}
	}
}

func NewService(store Store, opts ...Option) *Service {
	s := &Service{
		store:     store,
		publisher: boardevents.Nop{},
		logger:    slog.New(slog.DiscardHandler),
		clock:     clock.System{},
		newID:     uuid.NewV7,
		newToken:  secrets.Generator{Bytes: tokenBytes}.NewToken,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func HashToken(token string) string {
	return secrets.Hash(token)
}

func (s *Service) List(ctx context.Context, userID, boardID uuid.UUID) ([]domain.Member, error) {
	return s.store.BoardMembers(ctx, boardID, userID)
}

func validAssignableRole(role domain.Role) error {
	if role != domain.RoleEditor && role != domain.RoleViewer {
		return domain.NewValidationError("role", "must be editor or viewer")
	}
	return nil
}

func requireNotOwner(m domain.Member) error {
	if m.Role == domain.RoleOwner {
		return domain.ErrForbidden
	}
	return nil
}

func (s *Service) publish(ctx context.Context, ev domain.Event) {
	boardevents.PublishDetached(ctx, s.publisher, s.logger, "publish member event failed", []domain.Event{ev},
		slog.String("board_id", ev.BoardID.String()),
		slog.String("type", ev.Type),
		slog.Int64("seq", ev.Seq),
	)
}

type mutation func(ctx context.Context, tx boardtx.AdminTx) (*domain.Event, error)

func (s *Service) mutate(ctx context.Context, boardID uuid.UUID, fn mutation) error {
	var ev *domain.Event
	err := s.store.WithAdminLock(ctx, boardID, func(ctx context.Context, tx boardtx.AdminTx) error {
		var err error
		ev, err = fn(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	if ev != nil {
		s.publish(ctx, *ev)
	}
	return nil
}

func appendEvent(ctx context.Context, tx boardtx.Tx, actorID uuid.UUID, eventType string, payload any) (*domain.Event, error) {
	ev, err := tx.AppendEvent(ctx, domain.EventDraft{Type: eventType, Payload: payload, ActorID: &actorID})
	if err != nil {
		return nil, err
	}
	return &ev, nil
}

func addedPayload(m domain.Member, actorID uuid.UUID) boardcontent.MemberAdded {
	return boardcontent.MemberAdded{UserID: m.UserID, DisplayName: m.DisplayName, AvatarColor: m.AvatarColor, Role: m.Role, ActorID: actorID}
}

func (s *Service) Add(ctx context.Context, actorID, boardID uuid.UUID, email string, role domain.Role) (domain.Member, error) {
	cleanEmail, err := domain.NormalizeEmail(email)
	if err != nil {
		return domain.Member{}, err
	}
	if err := validAssignableRole(role); err != nil {
		return domain.Member{}, err
	}
	var member domain.Member
	err = s.mutate(ctx, boardID, func(ctx context.Context, tx boardtx.AdminTx) (*domain.Event, error) {
		if _, err := tx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return nil, err
		}
		user, err := tx.UserByEmail(ctx, cleanEmail)
		if err != nil {
			return nil, err
		}
		if member, err = tx.AddMember(ctx, user.ID, role); err != nil {
			return nil, err
		}
		return appendEvent(ctx, tx, actorID, boardcontent.EventMemberAdded, addedPayload(member, actorID))
	})
	if err != nil {
		return domain.Member{}, err
	}
	return member, nil
}

func (s *Service) ChangeRole(ctx context.Context, actorID, boardID, targetID uuid.UUID, role domain.Role) (domain.Member, error) {
	if err := validAssignableRole(role); err != nil {
		return domain.Member{}, err
	}
	var member domain.Member
	err := s.mutate(ctx, boardID, func(ctx context.Context, tx boardtx.AdminTx) (*domain.Event, error) {
		if _, err := tx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return nil, err
		}
		current, err := tx.Member(ctx, targetID)
		if err != nil {
			return nil, err
		}
		if err := requireNotOwner(current); err != nil {
			return nil, err
		}
		if current.Role == role {
			member = current
			return nil, nil
		}
		if member, err = tx.SetMemberRole(ctx, targetID, role); err != nil {
			return nil, err
		}
		return appendEvent(ctx, tx, actorID, boardcontent.EventMemberRoleChanged,
			boardcontent.MemberRoleChanged{UserID: targetID, Role: role, ActorID: actorID})
	})
	if err != nil {
		return domain.Member{}, err
	}
	return member, nil
}

func (s *Service) Remove(ctx context.Context, actorID, boardID, targetID uuid.UUID) error {
	return s.mutate(ctx, boardID, func(ctx context.Context, tx boardtx.AdminTx) (*domain.Event, error) {
		actorRole, err := tx.MemberRole(ctx, actorID)
		if err != nil {
			return nil, err
		}
		if actorID != targetID && !actorRole.AtLeast(domain.RoleOwner) {
			return nil, domain.ErrForbidden
		}
		target, err := tx.Member(ctx, targetID)
		if err != nil {
			return nil, err
		}
		if err := requireNotOwner(target); err != nil {
			return nil, err
		}
		if err := tx.RemoveMember(ctx, targetID); err != nil {
			return nil, err
		}
		return appendEvent(ctx, tx, actorID, boardcontent.EventMemberRemoved,
			boardcontent.MemberRemoved{UserID: targetID, ActorID: actorID})
	})
}
