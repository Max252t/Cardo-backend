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

type Announcement struct {
	Channel string
	Payload []byte
	Via     Publisher
}

type Entry struct {
	Member     string
	UserPrefix string
	User       protocol.PresenceUser
	At         time.Time
	Cutoff     time.Time
	Join       Announcement
}

type Exit struct {
	Member     string
	UserPrefix string
	UserID     uuid.UUID
	Cutoff     time.Time
	Leave      Announcement
}

type SweepRequest struct {
	Cutoff        time.Time
	LeaveTemplate Announcement
}

type Store interface {
	Enter(ctx context.Context, boardID uuid.UUID, e Entry) (bool, error)
	Exit(ctx context.Context, boardID uuid.UUID, x Exit) (bool, error)
	Sweep(ctx context.Context, boardID uuid.UUID, req SweepRequest) ([]uuid.UUID, error)
	Put(ctx context.Context, boardID uuid.UUID, members []string, at time.Time) error
	SaveProfiles(ctx context.Context, boardID uuid.UUID, users []protocol.PresenceUser) error
	Fresh(ctx context.Context, boardID uuid.UUID, after time.Time) ([]string, error)
	Profiles(ctx context.Context, boardID uuid.UUID, ids []uuid.UUID) ([]protocol.PresenceUser, error)
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

func (s *Service) announcement(boardID uuid.UUID, msgType string, payload any) (Announcement, error) {
	data, err := protocol.Encode(msgType, payload)
	if err != nil {
		return Announcement{}, err
	}
	return Announcement{Channel: protocol.PresenceChannel(boardID), Payload: data, Via: s.pub}, nil
}

func (s *Service) Join(ctx context.Context, boardID uuid.UUID, user protocol.PresenceUser) error {
	join, err := s.announcement(boardID, protocol.TypePresenceJoin, protocol.PresenceJoin{User: user})
	if err != nil {
		return err
	}
	_, err = s.store.Enter(ctx, boardID, Entry{
		Member:     s.member(user.ID),
		UserPrefix: UserPrefix(user.ID),
		User:       user,
		At:         s.clock.Now(),
		Cutoff:     s.cutoff(),
		Join:       join,
	})
	if err != nil {
		return fmt.Errorf("enter presence: %w", err)
	}
	return nil
}

func (s *Service) Leave(ctx context.Context, boardID, userID uuid.UUID) error {
	leave, err := s.announcement(boardID, protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: userID})
	if err != nil {
		return err
	}
	_, err = s.store.Exit(ctx, boardID, Exit{
		Member:     s.member(userID),
		UserPrefix: UserPrefix(userID),
		UserID:     userID,
		Cutoff:     s.cutoff(),
		Leave:      leave,
	})
	if err != nil {
		return fmt.Errorf("exit presence: %w", err)
	}
	return nil
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
	template, err := s.announcement(boardID, protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: uuid.Nil})
	if err != nil {
		return err
	}
	if _, err := s.store.Sweep(ctx, boardID, SweepRequest{Cutoff: s.cutoff(), LeaveTemplate: template}); err != nil {
		return fmt.Errorf("sweep presence: %w", err)
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

func byNameThenID(a, b protocol.PresenceUser) int {
	if c := strings.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	return strings.Compare(a.ID.String(), b.ID.String())
}
