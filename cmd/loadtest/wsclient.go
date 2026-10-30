package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const (
	dialTimeout      = 10 * time.Second
	handshakeTimeout = 30 * time.Second
	writeTimeout     = 10 * time.Second
)

type handshakeResult struct {
	Kind        string        `json:"kind"`
	Events      int           `json:"events"`
	Dial        time.Duration `json:"-"`
	FirstSync   time.Duration `json:"-"`
	LastSync    time.Duration `json:"-"`
	Ready       time.Duration `json:"-"`
	DialMs      float64       `json:"dial_ms"`
	FirstSyncMs float64       `json:"first_sync_ms"`
	LastSyncMs  float64       `json:"last_sync_ms"`
	ReadyMs     float64       `json:"ready_ms"`
}

type session struct {
	start     time.Time
	kind      string
	events    int
	firstSync time.Duration
	lastSync  time.Duration
	ready     chan time.Duration
	readyOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

type wsClient struct {
	idx      int
	acc      account
	url      string
	col      *collector
	world    *world
	isOwner  bool
	lastSeq  atomic.Int64
	received atomic.Int64

	writeMu sync.Mutex
	mu      sync.Mutex
	conn    *websocket.Conn
	current *session
	stopped atomic.Bool
}

func newWSClient(idx int, acc account, url string, col *collector, w *world) *wsClient {
	return &wsClient{idx: idx, acc: acc, url: url, col: col, world: w, isOwner: idx == 0}
}

func (c *wsClient) connect(ctx context.Context, lastSeq *int64) (handshakeResult, error) {
	start := time.Now()
	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout}
	conn, resp, err := dialer.DialContext(ctx, c.url, http.Header{"Authorization": {"Bearer " + c.acc.Token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return handshakeResult{}, fmt.Errorf("dial client %d: %w", c.idx, err)
	}
	dialed := time.Since(start)
	s := &session{start: start, ready: make(chan time.Duration, 1), closed: make(chan struct{})}
	c.mu.Lock()
	c.conn = conn
	c.current = s
	c.mu.Unlock()
	go c.readLoop(conn, s)
	hello, err := protocol.NewEnvelope(protocol.TypeHello, protocol.Hello{LastSeq: lastSeq, Client: "loadtest"})
	if err != nil {
		return handshakeResult{}, err
	}
	if err := c.write(conn, hello); err != nil {
		return handshakeResult{}, fmt.Errorf("hello client %d: %w", c.idx, err)
	}
	select {
	case ready := <-s.ready:
		return handshakeResult{
			Kind:        s.kind,
			Events:      s.events,
			Dial:        dialed,
			FirstSync:   s.firstSync,
			LastSync:    s.lastSync,
			Ready:       ready,
			DialMs:      ms(dialed),
			FirstSyncMs: ms(s.firstSync),
			LastSyncMs:  ms(s.lastSync),
			ReadyMs:     ms(ready),
		}, nil
	case <-s.closed:
		return handshakeResult{}, fmt.Errorf("client %d closed during handshake: %w", c.idx, s.closeErr)
	case <-time.After(handshakeTimeout):
		return handshakeResult{}, fmt.Errorf("client %d handshake timed out", c.idx)
	case <-ctx.Done():
		return handshakeResult{}, ctx.Err()
	}
}

func (c *wsClient) write(conn *websocket.Conn, env protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (c *wsClient) sendOp(op plannedOp, record bool) error {
	raw, err := json.Marshal(op.payload)
	if err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	id := op.id
	env := protocol.Envelope{V: protocol.Version, Type: op.opType, OpID: &id, Payload: raw}
	if record {
		c.col.opSent(op.id, c.idx, time.Now())
	}
	return c.write(conn, env)
}

func (c *wsClient) disconnect() {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.current = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (c *wsClient) stop() {
	c.stopped.Store(true)
	c.disconnect()
}

func (c *wsClient) readLoop(conn *websocket.Conn, s *session) {
	defer close(s.closed)
	for {
		_, data, err := conn.ReadMessage()
		at := time.Now()
		if err != nil {
			s.closeErr = err
			if !c.stopped.Load() && c.currentIs(s) {
				c.col.failure(fmt.Sprintf("client %d connection ended: %v", c.idx, err))
			}
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.col.failure(fmt.Sprintf("client %d: malformed message: %v", c.idx, err))
			continue
		}
		c.handle(s, env, at)
	}
}

func (c *wsClient) currentIs(s *session) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.current == s
}

func (c *wsClient) handle(s *session, env protocol.Envelope, at time.Time) {
	switch env.Type {
	case protocol.TypeSyncDelta:
		var delta protocol.SyncDelta
		if err := json.Unmarshal(env.Payload, &delta); err != nil {
			c.col.failure(fmt.Sprintf("client %d: bad sync.delta: %v", c.idx, err))
			return
		}
		if s.kind == "" {
			s.kind = "delta"
			s.firstSync = at.Sub(s.start)
		}
		s.lastSync = at.Sub(s.start)
		s.events += len(delta.Events)
		for _, ev := range delta.Events {
			c.observe(ev)
		}
	case protocol.TypeSyncSnapshot:
		var snap protocol.Snapshot
		if err := json.Unmarshal(env.Payload, &snap); err != nil {
			c.col.failure(fmt.Sprintf("client %d: bad sync.snapshot: %v", c.idx, err))
			return
		}
		s.kind = "snapshot"
		s.firstSync = at.Sub(s.start)
		s.lastSync = s.firstSync
		s.events = len(snap.Cards) + len(snap.Columns)
		c.advance(snap.Seq)
	case protocol.TypeSyncReady:
		s.readyOnce.Do(func() { s.ready <- at.Sub(s.start) })
	case protocol.TypeNack:
		var nack boardcontent.Nack
		if err := json.Unmarshal(env.Payload, &nack); err != nil || env.OpID == nil {
			c.col.failure(fmt.Sprintf("client %d: bad nack", c.idx))
			return
		}
		c.col.nack(*env.OpID, nack.Reason)
	case protocol.TypeError:
		c.col.failure(fmt.Sprintf("client %d: server error %s", c.idx, string(env.Payload)))
	case protocol.TypePresenceList, protocol.TypePresenceJoin, protocol.TypePresenceLeave, protocol.TypeAuthRefreshed:
	default:
		if env.Seq > 0 {
			c.observe(env)
			if c.col.recording.Load() && env.OpID != nil {
				c.col.eventReceived(*env.OpID, at, c.idx)
			}
		}
	}
}

func (c *wsClient) advance(seq int64) {
	for {
		cur := c.lastSeq.Load()
		if seq <= cur || c.lastSeq.CompareAndSwap(cur, seq) {
			break
		}
	}
	c.col.noteSeq(seq)
}

func (c *wsClient) observe(ev protocol.Envelope) {
	c.received.Add(1)
	c.advance(ev.Seq)
	if c.isOwner {
		c.world.apply(ev)
	}
}
