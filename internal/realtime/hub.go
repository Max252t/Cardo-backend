package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
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

	gates   boardGates
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
	h.hooks[boardcontent.EventBoardDeleted] = []EventHook{closeBoardOnDelete}
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

func anyClient(*Client) bool {
	return true
}

func ofUser(userID uuid.UUID) func(*Client) bool {
	return func(c *Client) bool { return c.member.UserID == userID }
}

func (h *Hub) CloseBoard(boardID uuid.UUID, code int, reason string) {
	for _, c := range h.clientsWhere(boardID, anyClient) {
		c.closeWith(code, reason)
	}
}

func (h *Hub) CloseUser(boardID, userID uuid.UUID, code int, reason string) {
	for _, c := range h.clientsWhere(boardID, ofUser(userID)) {
		c.closeWith(code, reason)
	}
}

func (h *Hub) SetRole(boardID, userID uuid.UUID, role domain.Role) {
	for _, c := range h.clientsWhere(boardID, ofUser(userID)) {
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

func (h *Hub) Shutdown(ctx context.Context) error {
	for _, c := range h.stopAccepting() {
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
	h.broadcast(boardID, 0, payload, presenceSubject(env))
}

func (h *Hub) busContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(h.baseCtx, h.cfg.BusTimeout)
}

func (h *Hub) stopAccepting() []*Client {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closing = true
	var all []*Client
	for _, r := range h.rooms {
		for c := range r.clients {
			all = append(all, c)
		}
	}
	return all
}

func presenceSubject(env protocol.Envelope) uuid.UUID {
	switch env.Type {
	case protocol.TypePresenceJoin:
		var join protocol.PresenceJoin
		if err := json.Unmarshal(env.Payload, &join); err == nil {
			return join.User.ID
		}
	case protocol.TypePresenceLeave:
		var leave protocol.PresenceLeave
		if err := json.Unmarshal(env.Payload, &leave); err == nil {
			return leave.UserID
		}
	}
	return uuid.Nil
}
