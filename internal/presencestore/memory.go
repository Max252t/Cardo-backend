package presencestore

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type MemoryStore struct {
	mu       sync.Mutex
	scores   map[uuid.UUID]map[string]time.Time
	profiles map[uuid.UUID]map[uuid.UUID]protocol.PresenceUser
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		scores:   make(map[uuid.UUID]map[string]time.Time),
		profiles: make(map[uuid.UUID]map[uuid.UUID]protocol.PresenceUser),
	}
}

func (m *MemoryStore) Put(_ context.Context, boardID uuid.UUID, members []string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	board := m.scores[boardID]
	if board == nil {
		board = make(map[string]time.Time)
		m.scores[boardID] = board
	}
	for _, member := range members {
		board[member] = at
	}
	return nil
}

func (m *MemoryStore) Remove(_ context.Context, boardID uuid.UUID, members ...string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, member := range members {
		delete(m.scores[boardID], member)
	}
	return nil
}

func (m *MemoryStore) Fresh(_ context.Context, boardID uuid.UUID, after time.Time) ([]string, error) {
	return m.collect(boardID, func(at time.Time) bool { return at.After(after) }), nil
}

func (m *MemoryStore) Stale(_ context.Context, boardID uuid.UUID, upTo time.Time) ([]string, error) {
	return m.collect(boardID, func(at time.Time) bool { return !at.After(upTo) }), nil
}

func (m *MemoryStore) collect(boardID uuid.UUID, keep func(time.Time) bool) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for member, at := range m.scores[boardID] {
		if keep(at) {
			out = append(out, member)
		}
	}
	return out
}

func (m *MemoryStore) SaveProfiles(_ context.Context, boardID uuid.UUID, users []protocol.PresenceUser) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	board := m.profiles[boardID]
	if board == nil {
		board = make(map[uuid.UUID]protocol.PresenceUser)
		m.profiles[boardID] = board
	}
	for _, u := range users {
		board[u.ID] = u
	}
	return nil
}

func (m *MemoryStore) Profiles(_ context.Context, boardID uuid.UUID, ids []uuid.UUID) ([]protocol.PresenceUser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []protocol.PresenceUser
	for _, id := range ids {
		if u, ok := m.profiles[boardID][id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

func (m *MemoryStore) DeleteProfiles(_ context.Context, boardID uuid.UUID, ids ...uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		delete(m.profiles[boardID], id)
	}
	return nil
}

var (
	_ presence.Store = (*MemoryStore)(nil)
	_ presence.Store = (*RedisStore)(nil)
)

func (m *MemoryStore) hasFreshUser(boardID uuid.UUID, prefix string, cutoff time.Time) bool {
	for member, at := range m.scores[boardID] {
		if at.After(cutoff) && strings.HasPrefix(member, prefix) {
			return true
		}
	}
	return false
}

func announce(ctx context.Context, a presence.Announcement) error {
	if a.Via == nil {
		return nil
	}
	return a.Via.Publish(ctx, a.Channel, a.Payload)
}

func (m *MemoryStore) Enter(ctx context.Context, boardID uuid.UUID, e presence.Entry) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	present := m.hasFreshUser(boardID, e.UserPrefix, e.Cutoff)
	if m.scores[boardID] == nil {
		m.scores[boardID] = make(map[string]time.Time)
	}
	m.scores[boardID][e.Member] = e.At
	if m.profiles[boardID] == nil {
		m.profiles[boardID] = make(map[uuid.UUID]protocol.PresenceUser)
	}
	m.profiles[boardID][e.User.ID] = e.User
	if present {
		return false, nil
	}
	return true, announce(ctx, e.Join)
}

func (m *MemoryStore) Exit(ctx context.Context, boardID uuid.UUID, x presence.Exit) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.scores[boardID], x.Member)
	if m.hasFreshUser(boardID, x.UserPrefix, x.Cutoff) {
		return false, nil
	}
	delete(m.profiles[boardID], x.UserID)
	return true, announce(ctx, x.Leave)
}

func (m *MemoryStore) Sweep(ctx context.Context, boardID uuid.UUID, req presence.SweepRequest) ([]uuid.UUID, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var stale []string
	for member, at := range m.scores[boardID] {
		if !at.After(req.Cutoff) {
			stale = append(stale, member)
		}
	}
	slices.Sort(stale)
	for _, member := range stale {
		delete(m.scores[boardID], member)
	}
	var departed []uuid.UUID
	seen := make(map[uuid.UUID]struct{})
	for _, member := range stale {
		id, ok := presence.MemberUser(member)
		if !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if m.hasFreshUser(boardID, presence.UserPrefix(id), req.Cutoff) {
			continue
		}
		delete(m.profiles[boardID], id)
		departed = append(departed, id)
		leave := req.LeaveTemplate
		leave.Payload = bytes.ReplaceAll(leave.Payload, []byte(uuid.Nil.String()), []byte(id.String()))
		if err := announce(ctx, leave); err != nil {
			return departed, err
		}
	}
	return departed, nil
}
