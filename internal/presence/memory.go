package presence

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

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
