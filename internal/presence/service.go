package presence

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	DefaultTTL      = 45 * time.Second
	memberSeparator = "|"
)

type Clock interface {
	Now() time.Time
}

type Notifier interface {
	Joined(ctx context.Context, boardID uuid.UUID, user domain.PresenceUser) error
	Left(ctx context.Context, boardID, userID uuid.UUID) error
}

type Entry struct {
	Member     string
	UserPrefix string
	User       domain.PresenceUser
	At         time.Time
	Cutoff     time.Time
	Notify     Notifier
}

type Exit struct {
	Member     string
	UserPrefix string
	UserID     uuid.UUID
	Cutoff     time.Time
	Notify     Notifier
}

type SweepRequest struct {
	Cutoff time.Time
	Notify Notifier
}

type Store interface {
	Enter(ctx context.Context, boardID uuid.UUID, e Entry) (bool, error)
	Exit(ctx context.Context, boardID uuid.UUID, x Exit) (bool, error)
	Sweep(ctx context.Context, boardID uuid.UUID, req SweepRequest) ([]uuid.UUID, error)
	Put(ctx context.Context, boardID uuid.UUID, members []string, at time.Time) error
	SaveProfiles(ctx context.Context, boardID uuid.UUID, users []domain.PresenceUser) error
	Fresh(ctx context.Context, boardID uuid.UUID, after time.Time) ([]string, error)
	Profiles(ctx context.Context, boardID uuid.UUID, ids []uuid.UUID) ([]domain.PresenceUser, error)
}

type Service struct {
	store    Store
	notifier Notifier
	clock    Clock
	instance string
	ttl      time.Duration
}

type Option func(*Service)

func WithTTL(ttl time.Duration) Option {
	return func(s *Service) {
		if ttl > 0 {
			s.ttl = ttl
		}
	}
}

func New(store Store, notifier Notifier, clock Clock, instanceID string, opts ...Option) *Service {
	s := &Service{store: store, notifier: notifier, clock: clock, instance: instanceID, ttl: DefaultTTL}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func UserPrefix(userID uuid.UUID) string {
	return userID.String() + memberSeparator
}

func (s *Service) member(userID uuid.UUID) string {
	return UserPrefix(userID) + s.instance
}

func MemberUser(member string) (uuid.UUID, bool) {
	raw, _, _ := strings.Cut(member, memberSeparator)
	id, err := uuid.Parse(raw)
	return id, err == nil
}

func (s *Service) cutoff() time.Time {
	return s.clock.Now().Add(-s.ttl)
}

func (s *Service) freshUsers(ctx context.Context, boardID uuid.UUID) (map[uuid.UUID]struct{}, error) {
	members, err := s.store.Fresh(ctx, boardID, s.cutoff())
	if err != nil {
		return nil, fmt.Errorf("list fresh presence: %w", err)
	}
	users := make(map[uuid.UUID]struct{}, len(members))
	for _, m := range members {
		if id, ok := MemberUser(m); ok {
			users[id] = struct{}{}
		}
	}
	return users, nil
}

func (s *Service) Join(ctx context.Context, boardID uuid.UUID, user domain.PresenceUser) error {
	_, err := s.store.Enter(ctx, boardID, Entry{
		Member:     s.member(user.ID),
		UserPrefix: UserPrefix(user.ID),
		User:       user,
		At:         s.clock.Now(),
		Cutoff:     s.cutoff(),
		Notify:     s.notifier,
	})
	if err != nil {
		return fmt.Errorf("enter presence: %w", err)
	}
	return nil
}

func (s *Service) Leave(ctx context.Context, boardID, userID uuid.UUID) error {
	_, err := s.store.Exit(ctx, boardID, Exit{
		Member:     s.member(userID),
		UserPrefix: UserPrefix(userID),
		UserID:     userID,
		Cutoff:     s.cutoff(),
		Notify:     s.notifier,
	})
	if err != nil {
		return fmt.Errorf("exit presence: %w", err)
	}
	return nil
}

func (s *Service) Touch(ctx context.Context, boardID uuid.UUID, users []domain.PresenceUser) error {
	if len(users) == 0 {
		return nil
	}
	if err := s.store.Put(ctx, boardID, s.membersOf(users), s.clock.Now()); err != nil {
		return fmt.Errorf("touch presence: %w", err)
	}
	if err := s.store.SaveProfiles(ctx, boardID, users); err != nil {
		return fmt.Errorf("save presence profiles: %w", err)
	}
	return nil
}

func (s *Service) Online(ctx context.Context, boardID uuid.UUID) ([]domain.PresenceUser, error) {
	fresh, err := s.freshUsers(ctx, boardID)
	if err != nil {
		return nil, err
	}
	if len(fresh) == 0 {
		return []domain.PresenceUser{}, nil
	}
	users, err := s.store.Profiles(ctx, boardID, slices.Collect(maps.Keys(fresh)))
	if err != nil {
		return nil, fmt.Errorf("load presence profiles: %w", err)
	}
	slices.SortFunc(users, byNameThenID)
	return users, nil
}

func (s *Service) Sweep(ctx context.Context, boardID uuid.UUID) error {
	if _, err := s.store.Sweep(ctx, boardID, SweepRequest{Cutoff: s.cutoff(), Notify: s.notifier}); err != nil {
		return fmt.Errorf("sweep presence: %w", err)
	}
	return nil
}

func (s *Service) membersOf(users []domain.PresenceUser) []string {
	members := make([]string, 0, len(users))
	for _, u := range users {
		members = append(members, s.member(u.ID))
	}
	return members
}

func byNameThenID(a, b domain.PresenceUser) int {
	if c := strings.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	return strings.Compare(a.ID.String(), b.ID.String())
}
