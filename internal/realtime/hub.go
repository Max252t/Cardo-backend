package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

var ErrShuttingDown = errors.New("realtime: hub is shutting down")

type EventHook func(h *Hub, boardID uuid.UUID, ev protocol.Envelope)

type Deps struct {
	Bus      Bus
	Presence Presence
	Ops      OpApplier
	Sync     SyncSource
	Members  MemberLookup
	Tokens   TokenParser
	Clock    Clock
	Logger   *slog.Logger
}

type HubOption func(*Hub)

func WithEventHook(eventType string, hook EventHook) HubOption {
	return func(h *Hub) {
		h.hooks[eventType] = append(h.hooks[eventType], hook)
	}
}

type roomUser struct {
	profile protocol.PresenceUser
	conns   int
}

type room struct {
	clients map[*Client]struct{}
	users   map[uuid.UUID]*roomUser
}

type Hub struct {
	bus      Bus
	presence Presence
	ops      OpApplier
	sync     SyncSource
	members  MemberLookup
	tokens   TokenParser
	clock    Clock
	logger   *slog.Logger
	cfg      Config
	hooks    map[string][]EventHook

	baseCtx    context.Context
	cancelBase context.CancelFunc
	wg         sync.WaitGroup

	subMu   sync.Mutex
	mu      sync.RWMutex
	rooms   map[uuid.UUID]*room
	closing bool
}

func NewHub(deps Deps, cfg Config, opts ...HubOption) (*Hub, error) {
	if deps.Bus == nil || deps.Presence == nil || deps.Ops == nil || deps.Sync == nil || deps.Members == nil || deps.Tokens == nil {
		return nil, errors.New("realtime: bus, presence, ops, sync, members and tokens dependencies are required")
	}
	if deps.Clock == nil {
		deps.Clock = SystemClock{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &Hub{
		bus:        deps.Bus,
		presence:   deps.Presence,
		ops:        deps.Ops,
		sync:       deps.Sync,
		members:    deps.Members,
		tokens:     deps.Tokens,
		clock:      deps.Clock,
		logger:     deps.Logger,
		cfg:        cfg.withDefaults(),
		hooks:      make(map[string][]EventHook),
		baseCtx:    ctx,
		cancelBase: cancel,
		rooms:      make(map[uuid.UUID]*room),
	}
	h.hooks[protocol.EventBoardDeleted] = []EventHook{closeBoardOnDelete}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

func closeBoardOnDelete(h *Hub, boardID uuid.UUID, _ protocol.Envelope) {
	h.CloseBoard(boardID, protocol.CloseBoardDeleted, "board deleted")
}

func (h *Hub) Dispatch(channel string, payload []byte) {
	kind, boardID := protocol.ParseChannel(channel)
	switch kind {
	case protocol.ChannelBoard:
		h.dispatchEvent(boardID, payload)
	case protocol.ChannelPresence:
		h.dispatchPresence(boardID, payload)
	case protocol.ChannelUnknown:
		h.logger.Warn("message on unknown channel", slog.String("channel", channel))
	}
}

func (h *Hub) dispatchEvent(boardID uuid.UUID, payload []byte) {
	var ev protocol.Envelope
	if err := json.Unmarshal(payload, &ev); err != nil {
		h.logger.Warn("dropping malformed board event", slog.String("board_id", boardID.String()), slog.Any("error", err))
		return
	}
	h.broadcast(boardID, ev.Seq, payload, uuid.Nil)
	for _, hook := range h.hooks[ev.Type] {
		hook(h, boardID, ev)
	}
}

func (h *Hub) broadcast(boardID uuid.UUID, seq int64, payload []byte, skipUser uuid.UUID) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r := h.rooms[boardID]
	if r == nil {
		return
	}
	for c := range r.clients {
		if skipUser != uuid.Nil && c.member.UserID == skipUser {
			continue
		}
		c.deliver(seq, payload)
	}
}

func (h *Hub) clientsWhere(boardID uuid.UUID, match func(*Client) bool) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	r := h.rooms[boardID]
	if r == nil {
		return nil
	}
	var out []*Client
	for c := range r.clients {
		if match(c) {
			out = append(out, c)
		}
	}
	return out
}

func (h *Hub) CloseBoard(boardID uuid.UUID, code int, reason string) {
	for _, c := range h.clientsWhere(boardID, func(*Client) bool { return true }) {
		c.closeWith(code, reason)
	}
}

func (h *Hub) CloseUser(boardID, userID uuid.UUID, code int, reason string) {
	for _, c := range h.clientsWhere(boardID, func(c *Client) bool { return c.member.UserID == userID }) {
		c.closeWith(code, reason)
	}
}

func (h *Hub) SetRole(boardID, userID uuid.UUID, role domain.Role) {
	for _, c := range h.clientsWhere(boardID, func(c *Client) bool { return c.member.UserID == userID }) {
		c.setRole(role)
	}
}

func (h *Hub) ConnectionCount(boardID uuid.UUID) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if r := h.rooms[boardID]; r != nil {
		return len(r.clients)
	}
	return 0
}

func (h *Hub) join(c *Client) error {
	h.subMu.Lock()
	first, err := h.register(c)
	h.subMu.Unlock()
	if err != nil {
		return err
	}
	if first {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(h.baseCtx), h.cfg.BusTimeout)
		defer cancel()
		if err := h.presence.Join(ctx, c.boardID, c.profile); err != nil {
			h.logger.Warn("presence join failed", c.logAttrs(slog.Any("error", err))...)
		}
	}
	return nil
}

func (h *Hub) register(c *Client) (bool, error) {
	h.mu.RLock()
	closing := h.closing
	r := h.rooms[c.boardID]
	h.mu.RUnlock()
	if closing {
		return false, ErrShuttingDown
	}
	if r == nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(h.baseCtx), h.cfg.BusTimeout)
		defer cancel()
		if err := h.bus.Subscribe(ctx, protocol.BoardChannel(c.boardID), protocol.PresenceChannel(c.boardID)); err != nil {
			return false, fmt.Errorf("subscribe board channels: %w", err)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false, ErrShuttingDown
	}
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

func (h *Hub) leave(c *Client) {
	h.subMu.Lock()
	lastConn, empty := h.unregister(c)
	if empty {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(h.baseCtx), h.cfg.BusTimeout)
		if err := h.bus.Unsubscribe(ctx, protocol.BoardChannel(c.boardID), protocol.PresenceChannel(c.boardID)); err != nil {
			h.logger.Warn("unsubscribe board channels failed", c.logAttrs(slog.Any("error", err))...)
		}
		cancel()
	}
	h.subMu.Unlock()
	if lastConn {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(h.baseCtx), h.cfg.BusTimeout)
		defer cancel()
		if err := h.presence.Leave(ctx, c.boardID, c.member.UserID); err != nil {
			h.logger.Warn("presence leave failed", c.logAttrs(slog.Any("error", err))...)
		}
	}
}

func (h *Hub) unregister(c *Client) (lastConn, empty bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	r := h.rooms[c.boardID]
	if r == nil {
		return false, false
	}
	if _, ok := r.clients[c]; !ok {
		return false, false
	}
	delete(r.clients, c)
	if u := r.users[c.member.UserID]; u != nil {
		u.conns--
		if u.conns <= 0 {
			delete(r.users, c.member.UserID)
			lastConn = true
		}
	}
	if len(r.clients) == 0 {
		delete(h.rooms, c.boardID)
		empty = true
	}
	return lastConn, empty
}

func (h *Hub) Run(ctx context.Context) {
	ticker := h.clock.NewTicker(h.cfg.PresenceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			h.heartbeat(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (h *Hub) heartbeat(ctx context.Context) {
	h.mu.RLock()
	snapshot := make(map[uuid.UUID][]protocol.PresenceUser, len(h.rooms))
	for boardID, r := range h.rooms {
		users := make([]protocol.PresenceUser, 0, len(r.users))
		for _, u := range r.users {
			users = append(users, u.profile)
		}
		snapshot[boardID] = users
	}
	h.mu.RUnlock()
	for boardID, users := range snapshot {
		opCtx, cancel := context.WithTimeout(ctx, h.cfg.BusTimeout)
		if err := h.presence.Touch(opCtx, boardID, users); err != nil {
			h.logger.Warn("presence heartbeat failed", slog.String("board_id", boardID.String()), slog.Any("error", err))
		}
		if err := h.presence.Sweep(opCtx, boardID); err != nil {
			h.logger.Warn("presence sweep failed", slog.String("board_id", boardID.String()), slog.Any("error", err))
		}
		cancel()
	}
}

func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closing = true
	var all []*Client
	for _, r := range h.rooms {
		for c := range r.clients {
			all = append(all, c)
		}
	}
	h.mu.Unlock()
	for _, c := range all {
		c.closeWith(protocol.CloseGoingAway, "server shutting down")
	}
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		h.cancelBase()
		return nil
	case <-ctx.Done():
		h.cancelBase()
		<-done
		return ctx.Err()
	}
}

func (h *Hub) dispatchPresence(boardID uuid.UUID, payload []byte) {
	var env protocol.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		h.logger.Warn("dropping malformed presence message", slog.String("board_id", boardID.String()), slog.Any("error", err))
		return
	}
	var subject uuid.UUID
	switch env.Type {
	case protocol.TypePresenceJoin:
		var join protocol.PresenceJoin
		if err := json.Unmarshal(env.Payload, &join); err == nil {
			subject = join.User.ID
		}
	case protocol.TypePresenceLeave:
		var leave protocol.PresenceLeave
		if err := json.Unmarshal(env.Payload, &leave); err == nil {
			subject = leave.UserID
		}
	}
	h.broadcast(boardID, 0, payload, subject)
}
