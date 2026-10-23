package members

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	DefaultInviteTTL     = 7 * 24 * time.Hour
	MaxInviteTTL         = 30 * 24 * time.Hour
	DefaultInviteMaxUses = 1
	MaxInviteMaxUses     = 100
	tokenBytes           = 32
)

type Store interface {
	WithBoardLock(ctx context.Context, boardID uuid.UUID, fn boardtx.Func) error
	BoardMembers(ctx context.Context, boardID, userID uuid.UUID) ([]domain.Member, error)
	BoardIDByInvite(ctx context.Context, tokenHash string) (uuid.UUID, error)
}

type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time {
	return time.Now()
}

type Service struct {
	store     Store
	publisher boardevents.Publisher
	logger    *slog.Logger
	clock     Clock
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

func WithClock(c Clock) Option {
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
		clock:     systemClock{},
		newID:     uuid.NewV7,
		newToken:  randomToken,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func randomToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate invite token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
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

func (s *Service) publish(ctx context.Context, ev domain.Event) {
	boardevents.PublishDetached(ctx, s.publisher, s.logger, "publish member event failed", []domain.Event{ev},
		slog.String("board_id", ev.BoardID.String()),
		slog.String("type", ev.Type),
		slog.Int64("seq", ev.Seq),
	)
}

func appendEvent(ctx context.Context, btx boardtx.Tx, actorID uuid.UUID, eventType string, payload any) (domain.Event, error) {
	return btx.AppendEvent(ctx, domain.EventDraft{Type: eventType, Payload: payload, ActorID: &actorID})
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
	var (
		member domain.Member
		ev     domain.Event
	)
	err = s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		if _, err := btx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return err
		}
		user, err := btx.UserByEmail(ctx, cleanEmail)
		if err != nil {
			return err
		}
		if member, err = btx.AddMember(ctx, user.ID, role); err != nil {
			return err
		}
		ev, err = appendEvent(ctx, btx, actorID, boardcontent.EventMemberAdded, addedPayload(member, actorID))
		return err
	})
	if err != nil {
		return domain.Member{}, err
	}
	s.publish(ctx, ev)
	return member, nil
}

func (s *Service) ChangeRole(ctx context.Context, actorID, boardID, targetID uuid.UUID, role domain.Role) (domain.Member, error) {
	if err := validAssignableRole(role); err != nil {
		return domain.Member{}, err
	}
	var (
		member  domain.Member
		ev      domain.Event
		changed bool
	)
	err := s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		if _, err := btx.RequireRole(ctx, actorID, domain.RoleOwner); err != nil {
			return err
		}
		current, err := btx.Member(ctx, targetID)
		if err != nil {
			return err
		}
		if current.Role == domain.RoleOwner {
			return domain.NewValidationError("user_id", "the owner role cannot be changed")
		}
		if current.Role == role {
			member = current
			return nil
		}
		if member, err = btx.SetMemberRole(ctx, targetID, role); err != nil {
			return err
		}
		changed = true
		ev, err = appendEvent(ctx, btx, actorID, boardcontent.EventMemberRoleChanged,
			boardcontent.MemberRoleChanged{UserID: targetID, Role: role, ActorID: actorID})
		return err
	})
	if err != nil {
		return domain.Member{}, err
	}
	if changed {
		s.publish(ctx, ev)
	}
	return member, nil
}

func (s *Service) Remove(ctx context.Context, actorID, boardID, targetID uuid.UUID) error {
	var ev domain.Event
	err := s.store.WithBoardLock(ctx, boardID, func(ctx context.Context, btx boardtx.Tx) error {
		actorRole, err := btx.MemberRole(ctx, actorID)
		if err != nil {
			return err
		}
		if actorID != targetID && !actorRole.AtLeast(domain.RoleOwner) {
			return domain.ErrForbidden
		}
		target, err := btx.Member(ctx, targetID)
		if err != nil {
			return err
		}
		if target.Role == domain.RoleOwner {
			return domain.ErrForbidden
		}
		if err := btx.RemoveMember(ctx, targetID); err != nil {
			return err
		}
		ev, err = appendEvent(ctx, btx, actorID, boardcontent.EventMemberRemoved,
			boardcontent.MemberRemoved{UserID: targetID, ActorID: actorID})
		return err
	})
	if err != nil {
		return err
	}
	s.publish(ctx, ev)
	return nil
}
