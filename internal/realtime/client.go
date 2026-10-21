package realtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"golang.org/x/time/rate"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

var errClientClosing = errors.New("realtime: client is closing")

type clientState int

const (
	stateAwaitHello clientState = iota
	stateSyncing
	stateLive
)

type outbound struct {
	data        []byte
	isClose     bool
	closeCode   int
	closeReason string
}

type pendingMessage struct {
	seq  int64
	data []byte
}

type Client struct {
	hub     *Hub
	conn    *websocket.Conn
	member  domain.Member
	boardID uuid.UUID
	profile protocol.PresenceUser
	role    atomic.Value
	send    chan outbound
	limiter *rate.Limiter

	ctx    context.Context
	cancel context.CancelFunc

	expiresAt   atomic.Int64
	expiryReset chan struct{}
	lastPong    atomic.Int64
	latestSeq   atomic.Int64
	closing     atomic.Bool

	helloReceived chan struct{}
	readDone      chan struct{}
	writeDone     chan struct{}
	handshakes    sync.WaitGroup

	mu      sync.Mutex
	state   clientState
	floor   int64
	pending []pendingMessage

	violations      int
	rateWindowStart time.Time
	rateStrikes     int
}

func newClient(h *Hub, conn *websocket.Conn, boardID uuid.UUID, member domain.Member, expiresAt time.Time) *Client {
	ctx, cancel := context.WithCancel(h.baseCtx)
	c := &Client{
		hub:           h,
		conn:          conn,
		member:        member,
		boardID:       boardID,
		profile:       protocol.PresenceUser{ID: member.UserID, Name: member.DisplayName, AvatarColor: member.AvatarColor},
		send:          make(chan outbound, h.cfg.SendBuffer),
		limiter:       rate.NewLimiter(rate.Limit(h.cfg.OpsPerSecond), h.cfg.OpsBurst),
		ctx:           ctx,
		cancel:        cancel,
		expiryReset:   make(chan struct{}, 1),
		helloReceived: make(chan struct{}),
		readDone:      make(chan struct{}),
		writeDone:     make(chan struct{}),
	}
	c.role.Store(member.Role)
	c.expiresAt.Store(expiresAt.UnixNano())
	c.lastPong.Store(h.clock.Now().UnixNano())
	return c
}

func (c *Client) Role() domain.Role {
	role, _ := c.role.Load().(domain.Role)
	return role
}

func (c *Client) setRole(role domain.Role) {
	c.role.Store(role)
}

func (c *Client) logAttrs(extra ...any) []any {
	return append([]any{
		slog.String("board_id", c.boardID.String()),
		slog.String("user_id", c.member.UserID.String()),
	}, extra...)
}

func (c *Client) run() {
	defer c.hub.wg.Done()
	go c.closeOnCancel()
	go c.writePump()
	go c.watchExpiry()
	go c.watchHello()
	c.readPump()
	c.closing.Store(true)
	c.cancel()
	_ = c.conn.Close()
	<-c.writeDone
	c.handshakes.Wait()
	c.hub.leave(c)
}

func (c *Client) noteSeq(seq int64) {
	for {
		cur := c.latestSeq.Load()
		if seq <= cur || c.latestSeq.CompareAndSwap(cur, seq) {
			return
		}
	}
}

func (c *Client) deliver(seq int64, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == stateLive {
		if seq > 0 && seq <= c.floor {
			return
		}
		c.noteSeq(seq)
		c.enqueue(data)
		return
	}
	if len(c.pending) >= c.hub.cfg.PendingLimit {
		c.closeWith(protocol.CloseSlowClient, "handshake buffer overflow")
		return
	}
	c.pending = append(c.pending, pendingMessage{seq: seq, data: data})
}

func (c *Client) goLive(ctx context.Context, sentSeq int64, ready []byte) error {
	seen := make(map[int64]struct{})
	readySent := false
	for {
		c.mu.Lock()
		batch := c.pending
		c.pending = nil
		if len(batch) == 0 && readySent {
			c.state = stateLive
			c.floor = sentSeq
			c.mu.Unlock()
			return nil
		}
		c.mu.Unlock()
		for _, p := range batch {
			if p.seq > 0 {
				if p.seq <= sentSeq {
					continue
				}
				if _, dup := seen[p.seq]; dup {
					continue
				}
				seen[p.seq] = struct{}{}
				c.noteSeq(p.seq)
			}
			if err := c.sendBlocking(ctx, p.data); err != nil {
				return err
			}
		}
		if len(batch) == 0 {
			if err := c.sendBlocking(ctx, ready); err != nil {
				return err
			}
			readySent = true
		}
	}
}

func (c *Client) sendBlocking(ctx context.Context, data []byte) error {
	if c.closing.Load() {
		return errClientClosing
	}
	select {
	case c.send <- outbound{data: data}:
		return nil
	case <-ctx.Done():
		c.closeNow(protocol.CloseSlowClient, "handshake flush timed out")
		return ctx.Err()
	}
}

func (c *Client) enqueue(data []byte) {
	if c.closing.Load() {
		return
	}
	select {
	case c.send <- outbound{data: data}:
	default:
		c.closeNow(protocol.CloseSlowClient, "send buffer overflow")
	}
}

func (c *Client) closeWith(code int, reason string) {
	if !c.closing.CompareAndSwap(false, true) {
		return
	}
	select {
	case c.send <- outbound{isClose: true, closeCode: code, closeReason: reason}:
	default:
		go c.forceClose(code, reason)
	}
}

func (c *Client) closeNow(code int, reason string) {
	if !c.closing.CompareAndSwap(false, true) {
		return
	}
	go c.forceClose(code, reason)
}

func (c *Client) forceClose(code int, reason string) {
	c.sendCloseFrame(code, reason)
	c.cancel()
	_ = c.conn.Close()
}

func (c *Client) writePump() {
	defer close(c.writeDone)
	ping := c.hub.clock.NewTicker(c.hub.cfg.PingInterval)
	defer ping.Stop()
	for {
		select {
		case item := <-c.send:
			if item.isClose {
				c.writeClose(item)
				return
			}
			if err := c.write(websocket.TextMessage, item.data); err != nil {
				c.cancel()
				return
			}
		case <-ping.C():
			if c.hub.clock.Now().UnixNano()-c.lastPong.Load() >= int64(c.hub.cfg.PongWait) {
				c.closeNow(protocol.CloseGoingAway, "pong timeout")
				return
			}
			if err := c.write(websocket.PingMessage, nil); err != nil {
				c.cancel()
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) write(messageType int, data []byte) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.hub.cfg.WriteWait)); err != nil {
		return err
	}
	return c.conn.WriteMessage(messageType, data)
}

func (c *Client) writeClose(item outbound) {
	c.sendCloseFrame(item.closeCode, item.closeReason)
	grace := c.hub.clock.NewTimer(c.hub.cfg.CloseGrace)
	defer grace.Stop()
	select {
	case <-c.readDone:
	case <-grace.C():
	case <-c.ctx.Done():
	}
	c.cancel()
}

func (c *Client) touch() {
	c.lastPong.Store(c.hub.clock.Now().UnixNano())
}

func (c *Client) readPump() {
	defer close(c.readDone)
	c.conn.SetReadLimit(c.hub.cfg.MaxMessageBytes)
	c.conn.SetPongHandler(func(string) error {
		c.touch()
		return nil
	})
	for {
		messageType, data, err := c.conn.ReadMessage()
		if err != nil {
			c.hub.logger.Debug("websocket read ended", c.logAttrs(slog.Any("error", err))...)
			return
		}
		c.touch()
		if c.closing.Load() {
			continue
		}
		if messageType != websocket.TextMessage {
			c.violation("binary frames are not supported")
			continue
		}
		c.handle(data)
	}
}

func (c *Client) watchExpiry() {
	for {
		remaining := time.Unix(0, c.expiresAt.Load()).Sub(c.hub.clock.Now())
		if remaining <= 0 {
			c.closeWith(protocol.CloseInvalidToken, "token expired")
			return
		}
		timer := c.hub.clock.NewTimer(remaining)
		select {
		case <-timer.C():
		case <-c.expiryReset:
			timer.Stop()
		case <-c.ctx.Done():
			timer.Stop()
			return
		}
	}
}

func (c *Client) watchHello() {
	timer := c.hub.clock.NewTimer(c.hub.cfg.HelloTimeout)
	defer timer.Stop()
	select {
	case <-timer.C():
		c.closeWith(protocol.CloseProtocolError, "hello timeout")
	case <-c.helloReceived:
	case <-c.ctx.Done():
	}
}

func (c *Client) setExpiry(expiresAt time.Time) {
	c.expiresAt.Store(expiresAt.UnixNano())
	select {
	case c.expiryReset <- struct{}{}:
	default:
	}
}

func (c *Client) closeOnCancel() {
	<-c.ctx.Done()
	_ = c.conn.Close()
}

func (c *Client) sendCloseFrame(code int, reason string) {
	c.hub.logger.Debug("closing websocket", c.logAttrs(slog.Int("code", code))...)
	writeCloseFrame(c.conn, code, reason, c.hub.cfg.WriteWait)
}

func writeCloseFrame(conn *websocket.Conn, code int, reason string, wait time.Duration) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, protocol.CloseReason(reason)),
		time.Now().Add(wait))
}
