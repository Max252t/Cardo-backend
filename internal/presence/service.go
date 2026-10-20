package presence

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const (
	DefaultTTL      = 45 * time.Second
	memberSeparator = "|"
)

type Clock interface {
	Now() time.Time
}

type Publisher interface {
	Publish(ctx context.Context, channel string, payload []byte) error
}

type Store interface {
	Put(ctx context.Context, boardID uuid.UUID, members []string, at time.Time) error
	Remove(ctx context.Context, boardID uuid.UUID, members ...string) error
	Fresh(ctx context.Context, boardID uuid.UUID, after time.Time) ([]string, error)
	Stale(ctx context.Context, boardID uuid.UUID, upTo time.Time) ([]string, error)
	SaveProfiles(ctx context.Context, boardID uuid.UUID, users []protocol.PresenceUser) error
	Profiles(ctx context.Context, boardID uuid.UUID, ids []uuid.UUID) ([]protocol.PresenceUser, error)
	DeleteProfiles(ctx context.Context, boardID uuid.UUID, ids ...uuid.UUID) error
}

type Service struct {
	store    Store
	pub      Publisher
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

func New(store Store, pub Publisher, clock Clock, instanceID string, opts ...Option) *Service {
	s := &Service{store: store, pub: pub, clock: clock, instance: instanceID, ttl: DefaultTTL}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) member(userID uuid.UUID) string {
	return userID.String() + memberSeparator + s.instance
}

func memberUser(member string) (uuid.UUID, bool) {
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
		if id, ok := memberUser(m); ok {
			users[id] = struct{}{}
		}
	}
	return users, nil
}

func (s *Service) Join(ctx context.Context, boardID uuid.UUID, user protocol.PresenceUser) error {
	before, err := s.freshUsers(ctx, boardID)
	if err != nil {
		return err
	}
	if err := s.store.Put(ctx, boardID, []string{s.member(user.ID)}, s.clock.Now()); err != nil {
		return fmt.Errorf("put presence: %w", err)
	}
	if err := s.store.SaveProfiles(ctx, boardID, []protocol.PresenceUser{user}); err != nil {
		return fmt.Errorf("save presence profile: %w", err)
	}
	if _, present := before[user.ID]; present {
		return nil
	}
	return s.announce(ctx, boardID, protocol.TypePresenceJoin, protocol.PresenceJoin{User: user})
}

func (s *Service) Leave(ctx context.Context, boardID, userID uuid.UUID) error {
	if err := s.store.Remove(ctx, boardID, s.member(userID)); err != nil {
		return fmt.Errorf("remove presence: %w", err)
	}
	remaining, err := s.freshUsers(ctx, boardID)
	if err != nil {
		return err
	}
	if _, stillPresent := remaining[userID]; stillPresent {
		return nil
	}
	return s.depart(ctx, boardID, userID)
}

func (s *Service) Touch(ctx context.Context, boardID uuid.UUID, users []protocol.PresenceUser) error {
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

func (s *Service) Online(ctx context.Context, boardID uuid.UUID) ([]protocol.PresenceUser, error) {
	fresh, err := s.freshUsers(ctx, boardID)
	if err != nil {
		return nil, err
	}
	if len(fresh) == 0 {
		return []protocol.PresenceUser{}, nil
	}
	users, err := s.store.Profiles(ctx, boardID, slices.Collect(maps.Keys(fresh)))
	if err != nil {
		return nil, fmt.Errorf("load presence profiles: %w", err)
	}
	slices.SortFunc(users, byNameThenID)
	return users, nil
}

func (s *Service) Sweep(ctx context.Context, boardID uuid.UUID) error {
	stale, err := s.store.Stale(ctx, boardID, s.cutoff())
	if err != nil {
		return fmt.Errorf("list stale presence: %w", err)
	}
	if len(stale) == 0 {
		return nil
	}
	if err := s.store.Remove(ctx, boardID, stale...); err != nil {
		return fmt.Errorf("remove stale presence: %w", err)
	}
	remaining, err := s.freshUsers(ctx, boardID)
	if err != nil {
		return err
	}
	var firstErr error
	for id := range departedUsers(stale, remaining) {
		if err := s.depart(ctx, boardID, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Service) depart(ctx context.Context, boardID, userID uuid.UUID) error {
	if err := s.store.DeleteProfiles(ctx, boardID, userID); err != nil {
		return fmt.Errorf("delete presence profile: %w", err)
	}
	return s.announce(ctx, boardID, protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: userID})
}

func (s *Service) announce(ctx context.Context, boardID uuid.UUID, msgType string, payload any) error {
	data, err := protocol.Encode(msgType, payload)
	if err != nil {
		return err
	}
	if err := s.pub.Publish(ctx, protocol.PresenceChannel(boardID), data); err != nil {
		return fmt.Errorf("publish %s: %w", msgType, err)
	}
	return nil
}

func (s *Service) membersOf(users []protocol.PresenceUser) []string {
	members := make([]string, 0, len(users))
	for _, u := range users {
		members = append(members, s.member(u.ID))
	}
	return members
}

func departedUsers(stale []string, remaining map[uuid.UUID]struct{}) map[uuid.UUID]struct{} {
	departed := make(map[uuid.UUID]struct{})
	for _, m := range stale {
		if id, ok := memberUser(m); ok {
			if _, alive := remaining[id]; !alive {
				departed[id] = struct{}{}
			}
		}
	}
	return departed
}

func byNameThenID(a, b protocol.PresenceUser) int {
	if c := strings.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	return strings.Compare(a.ID.String(), b.ID.String())
}
