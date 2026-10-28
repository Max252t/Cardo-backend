package realtime

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type boardGate struct {
	mu   sync.Mutex
	refs int
}

type boardGates struct {
	mu    sync.Mutex
	gates map[uuid.UUID]*boardGate
}

func (g *boardGates) lock(boardID uuid.UUID) func() {
	g.mu.Lock()
	if g.gates == nil {
		g.gates = make(map[uuid.UUID]*boardGate)
	}
	gate := g.gates[boardID]
	if gate == nil {
		gate = &boardGate{}
		g.gates[boardID] = gate
	}
	gate.refs++
	g.mu.Unlock()

	gate.mu.Lock()
	return func() {
		gate.mu.Unlock()
		g.mu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(g.gates, boardID)
		}
		g.mu.Unlock()
	}
}

func (h *Hub) join(c *Client) error {
	defer h.gates.lock(c.boardID)()
	if h.isClosing() {
		return ErrShuttingDown
	}
	subscribed := false
	if !h.hasRoom(c.boardID) {
		if err := h.subscribe(c.boardID); err != nil {
			return err
		}
		subscribed = true
	}
	first, err := h.register(c)
	if err != nil {
		if subscribed {
			h.unsubscribe(c)
		}
		return err
	}
	if first {
		ctx, cancel := h.busContext()
		defer cancel()
		if err := h.presence.Join(ctx, c.boardID, c.profile); err != nil {
			h.logger.Warn("presence join failed", c.logAttrs(slog.Any("error", err))...)
		}
	}
	return nil
}

func (h *Hub) leave(c *Client) {
	defer h.gates.lock(c.boardID)()
	lastConn, empty := h.unregister(c)
	if lastConn {
		ctx, cancel := h.busContext()
		if err := h.presence.Leave(ctx, c.boardID, c.member.UserID); err != nil {
			h.logger.Warn("presence leave failed", c.logAttrs(slog.Any("error", err))...)
		}
		cancel()
	}
	if empty {
		h.unsubscribe(c)
	}
}

func (h *Hub) subscribe(boardID uuid.UUID) error {
	ctx, cancel := h.busContext()
	defer cancel()
	if err := h.bus.Subscribe(ctx, protocol.BoardChannel(boardID), protocol.PresenceChannel(boardID)); err != nil {
		return fmt.Errorf("subscribe board channels: %w", err)
	}
	return nil
}

func (h *Hub) unsubscribe(c *Client) {
	ctx, cancel := h.busContext()
	defer cancel()
	if err := h.bus.Unsubscribe(ctx, protocol.BoardChannel(c.boardID), protocol.PresenceChannel(c.boardID)); err != nil {
		h.logger.Warn("unsubscribe board channels failed", c.logAttrs(slog.Any("error", err))...)
	}
}

func (h *Hub) isClosing() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.closing
}

func (h *Hub) hasRoom(boardID uuid.UUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.rooms[boardID] != nil
}

func (h *Hub) register(c *Client) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false, ErrShuttingDown
	}
	r := h.rooms[c.boardID]
	if r == nil {
		r = &room{clients: make(map[*Client]struct{}), users: make(map[uuid.UUID]*roomUser)}
		h.rooms[c.boardID] = r
	}
	r.clients[c] = struct{}{}
	u := r.users[c.member.UserID]
	if u == nil {
		u = &roomUser{profile: c.profile}
		r.users[c.member.UserID] = u
	}
	u.conns++
	h.wg.Add(1)
	return u.conns == 1, nil
}

func (h *Hub) heartbeat(ctx context.Context) {
	for _, boardID := range h.boardIDs() {
		h.heartbeatBoard(ctx, boardID)
		h.revalidateMembers(ctx, boardID)
	}
}

func (h *Hub) heartbeatBoard(ctx context.Context, boardID uuid.UUID) {
	defer h.gates.lock(boardID)()
	users := h.onlineUsers(boardID)
	if len(users) == 0 {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, h.cfg.BusTimeout)
	defer cancel()
	if err := h.presence.Touch(opCtx, boardID, users); err != nil {
		h.logger.Warn("presence heartbeat failed", slog.String("board_id", boardID.String()), slog.Any("error", err))
	}
	if err := h.presence.Sweep(opCtx, boardID); err != nil {
		h.logger.Warn("presence sweep failed", slog.String("board_id", boardID.String()), slog.Any("error", err))
	}
}

func (h *Hub) boardIDs() []uuid.UUID {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]uuid.UUID, 0, len(h.rooms))
	for id := range h.rooms {
		ids = append(ids, id)
	}
	return ids
}

func (h *Hub) onlineUsers(boardID uuid.UUID) []domain.PresenceUser {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r := h.rooms[boardID]
	if r == nil {
		return nil
	}
	users := make([]domain.PresenceUser, 0, len(r.users))
	for _, u := range r.users {
		users = append(users, u.profile)
	}
	return users
}
