package simclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const (
	defaultGapWait    = time.Second
	defaultIdlePoll   = 2 * time.Second
	defaultMaxPending = 40
	tickInterval      = 20 * time.Millisecond
	httpTimeout       = 10 * time.Second
	dialTimeout       = 5 * time.Second
	writeTimeout      = 5 * time.Second
	retryFetchDelay   = 250 * time.Millisecond
	inboxSize         = 1 << 14
)

type Config struct {
	Name       string
	BoardID    uuid.UUID
	Endpoints  []string
	Token      string
	Seed       int64
	Profile    Profile
	GapWait    time.Duration
	IdlePoll   time.Duration
	MaxPending int
	Logf       func(format string, args ...any)
}

type Stats struct {
	OpsCreated      int
	OpsSent         int
	LocalDrops      int
	EventsApplied   int
	Reconnects      int
	DialFailures    int
	SelfDrops       int
	GapFetches      int
	GapFetchEvents  int
	PollEvents      int
	SnapshotReloads int
	ResyncRequired  int
	Gone            int
	Buffered        int
	Nacks           map[string]int
	Failures        []string
}

type Status struct {
	Live        bool
	Initialized bool
	Pending     int
	Buffered    int
	State       *State
	Stats       Stats
}

type inbound struct {
	gen  int
	data []byte
	err  error
}

type fetchSource int

const (
	fetchGap fetchSource = iota
	fetchPoll
)

type Client struct {
	cfg Config
	rng *rand.Rand
	gen *generator

	cmds    chan func()
	inbox   chan inbound
	stopped chan struct{}
	cancel  context.CancelFunc
	http    *http.Client

	mu   sync.Mutex
	last Status

	connGen     int
	conn        *websocket.Conn
	live        bool
	initialized bool
	state       *State
	pending     []PendingOp
	view        *view
	dirty       bool
	buf         map[int64]protocol.Envelope
	stats       Stats

	gapDeadline time.Time
	reconnectAt time.Time
	nextPoll    time.Time
}

func New(cfg Config) *Client {
	if cfg.GapWait <= 0 {
		cfg.GapWait = defaultGapWait
	}
	if cfg.IdlePoll <= 0 {
		cfg.IdlePoll = defaultIdlePoll
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = defaultMaxPending
	}
	if cfg.Profile.MaxColumns <= 0 {
		cfg.Profile.MaxColumns = 6
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	rng := rand.New(rand.NewSource(cfg.Seed))
	return &Client{
		cfg:     cfg,
		rng:     rng,
		gen:     &generator{rng: rng, name: cfg.Name, profile: cfg.Profile},
		cmds:    make(chan func()),
		inbox:   make(chan inbound, inboxSize),
		stopped: make(chan struct{}),
		http:    &http.Client{Timeout: httpTimeout},
		state:   NewState(),
		buf:     make(map[int64]protocol.Envelope),
		stats:   Stats{Nacks: make(map[string]int)},
	}
}

func (c *Client) Start(ctx context.Context) {
	ctx, c.cancel = context.WithCancel(ctx)
	go c.loop(ctx)
}

func (c *Client) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	<-c.stopped
}

func (c *Client) exec(fn func()) bool {
	done := make(chan struct{})
	wrapped := func() {
		fn()
		close(done)
	}
	select {
	case c.cmds <- wrapped:
	case <-c.stopped:
		return false
	}
	select {
	case <-done:
		return true
	case <-c.stopped:
		return false
	}
}

func (c *Client) RandomOp() bool {
	created := false
	c.exec(func() { created = c.randomOp() })
	return created
}

func (c *Client) Drop(offline time.Duration) {
	c.exec(func() {
		if c.conn == nil {
			return
		}
		c.stats.SelfDrops++
		c.disconnect(offline)
	})
}

func (c *Client) Status() Status {
	var st Status
	if c.exec(func() { st = c.status() }) {
		return st
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func (c *Client) status() Status {
	stats := c.stats
	stats.Nacks = make(map[string]int, len(c.stats.Nacks))
	for k, v := range c.stats.Nacks {
		stats.Nacks[k] = v
	}
	stats.Failures = append([]string(nil), c.stats.Failures...)
	stats.Buffered = len(c.buf)
	return Status{
		Live:        c.live,
		Initialized: c.initialized,
		Pending:     len(c.pending),
		Buffered:    len(c.buf),
		State:       c.state.Clone(),
		Stats:       stats,
	}
}

func (c *Client) loop(ctx context.Context) {
	defer close(c.stopped)
	defer func() {
		st := c.status()
		c.mu.Lock()
		c.last = st
		c.mu.Unlock()
		if c.conn != nil {
			_ = c.conn.Close()
		}
	}()
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	c.nextPoll = time.Now().Add(c.cfg.IdlePoll)
	c.connect(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-c.cmds:
			fn()
		case m := <-c.inbox:
			c.onInbound(m)
		case now := <-ticker.C:
			c.onTick(ctx, now)
		}
	}
}

func (c *Client) onTick(ctx context.Context, now time.Time) {
	if !c.reconnectAt.IsZero() && !now.Before(c.reconnectAt) {
		c.connect(ctx)
	}
	if !c.gapDeadline.IsZero() && !now.Before(c.gapDeadline) {
		c.gapDeadline = time.Time{}
		c.stats.GapFetches++
		c.fetchEvents(ctx, fetchGap)
	}
	if !now.Before(c.nextPoll) {
		c.nextPoll = now.Add(c.cfg.IdlePoll)
		if c.initialized {
			c.fetchEvents(ctx, fetchPoll)
		}
	}
}

func (c *Client) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.cfg.Logf("%s: %s", c.cfg.Name, msg)
	c.stats.Failures = append(c.stats.Failures, msg)
}

func (c *Client) pickEndpoint() string {
	return c.cfg.Endpoints[c.rng.Intn(len(c.cfg.Endpoints))]
}

func (c *Client) reconnectDelay() time.Duration {
	return time.Duration(20+c.rng.Intn(130)) * time.Millisecond
}

func (c *Client) connect(ctx context.Context) {
	c.reconnectAt = time.Time{}
	if c.conn != nil {
		return
	}
	url := "ws" + strings.TrimPrefix(c.pickEndpoint(), "http") + "/ws/boards/" + c.cfg.BoardID.String()
	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout}
	conn, resp, err := dialer.DialContext(ctx, url, http.Header{"Authorization": {"Bearer " + c.cfg.Token}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		c.stats.DialFailures++
		c.cfg.Logf("%s: dial failed: %v", c.cfg.Name, err)
		c.reconnectAt = time.Now().Add(c.reconnectDelay())
		return
	}
	c.connGen++
	c.conn = conn
	c.live = false
	c.stats.Reconnects++
	c.buf = make(map[int64]protocol.Envelope)
	c.gapDeadline = time.Time{}
	go c.readLoop(ctx, conn, c.connGen)
	hello := protocol.Hello{Client: "simclient"}
	if c.initialized {
		seq := c.state.Seq
		hello.LastSeq = &seq
	}
	env, err := protocol.NewEnvelope(protocol.TypeHello, hello)
	if err != nil {
		c.fail("build hello: %v", err)
		return
	}
	c.write(env)
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn, gen int) {
	for {
		_, data, err := conn.ReadMessage()
		select {
		case c.inbox <- inbound{gen: gen, data: data, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) disconnect(delay time.Duration) {
	if c.conn != nil {
		_ = c.conn.UnderlyingConn().Close()
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connGen++
	c.live = false
	c.buf = make(map[int64]protocol.Envelope)
	c.gapDeadline = time.Time{}
	c.reconnectAt = time.Now().Add(delay)
}

func (c *Client) write(env protocol.Envelope) bool {
	if c.conn == nil {
		return false
	}
	data, err := json.Marshal(env)
	if err != nil {
		c.fail("marshal %s: %v", env.Type, err)
		return false
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.cfg.Logf("%s: write failed: %v", c.cfg.Name, err)
		c.disconnect(c.reconnectDelay())
		return false
	}
	return true
}

func (c *Client) onInbound(m inbound) {
	if m.gen != c.connGen {
		return
	}
	if m.err != nil {
		var closeErr *websocket.CloseError
		if errors.As(m.err, &closeErr) {
			c.fail("server closed the connection: code %d %q", closeErr.Code, closeErr.Text)
		} else {
			c.cfg.Logf("%s: read failed: %v", c.cfg.Name, m.err)
		}
		c.disconnect(c.reconnectDelay())
		return
	}
	env, err := protocol.ParseEnvelope(m.data)
	if err != nil {
		c.fail("parse server message: %v", err)
		return
	}
	c.handle(env)
}

func (c *Client) handle(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeSyncDelta:
		var delta protocol.SyncDelta
		if err := json.Unmarshal(env.Payload, &delta); err != nil {
			c.fail("decode sync.delta: %v", err)
			return
		}
		for _, ev := range delta.Events {
			c.acceptEvent(ev)
		}
	case protocol.TypeSyncSnapshot:
		var snap protocol.Snapshot
		if err := json.Unmarshal(env.Payload, &snap); err != nil {
			c.fail("decode sync.snapshot: %v", err)
			return
		}
		c.loadSnapshot(snap)
	case protocol.TypeSyncReady:
		c.onReady()
	case protocol.TypeNack:
		c.onNack(env)
	case protocol.TypeError:
		c.fail("server error: %s", string(env.Payload))
	case protocol.TypePresenceList, protocol.TypePresenceJoin, protocol.TypePresenceLeave, protocol.TypeAuthRefreshed:
	default:
		if env.Seq > 0 {
			c.acceptEvent(env)
			return
		}
		c.fail("unexpected message type %q", env.Type)
	}
}

func (c *Client) acceptEvent(env protocol.Envelope) {
	if env.Seq <= 0 {
		return
	}
	if env.Seq <= c.state.Seq {
		c.resolve(env.OpID)
		return
	}
	if env.Seq > c.state.Seq+1 {
		c.buf[env.Seq] = env
		if c.gapDeadline.IsZero() {
			c.gapDeadline = time.Now().Add(c.cfg.GapWait)
		}
		return
	}
	c.applyEvent(env)
	c.drain()
}

func (c *Client) applyEvent(env protocol.Envelope) {
	if err := c.state.Apply(env); err != nil {
		c.fail("%v", err)
		c.gapDeadline = time.Now().Add(retryFetchDelay)
		return
	}
	c.stats.EventsApplied++
	c.dirty = true
	c.resolve(env.OpID)
}

func (c *Client) drain() {
	progressed := false
	for {
		next, ok := c.buf[c.state.Seq+1]
		if !ok {
			break
		}
		delete(c.buf, next.Seq)
		c.applyEvent(next)
		progressed = true
	}
	for seq, ev := range c.buf {
		if seq <= c.state.Seq {
			delete(c.buf, seq)
			c.resolve(ev.OpID)
		}
	}
	switch {
	case len(c.buf) == 0:
		c.gapDeadline = time.Time{}
	case progressed || c.gapDeadline.IsZero():
		c.gapDeadline = time.Now().Add(c.cfg.GapWait)
	}
}

func (c *Client) resolve(opID *uuid.UUID) {
	if opID == nil {
		return
	}
	for i, p := range c.pending {
		if p.ID == *opID {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			c.dirty = true
			return
		}
	}
}

func (c *Client) loadSnapshot(snap protocol.Snapshot) {
	c.state.Load(snap)
	c.initialized = true
	c.dirty = true
	c.drain()
}

func (c *Client) onReady() {
	c.live = true
	c.rebase()
	c.resendPending()
}

func (c *Client) resendPending() {
	for _, p := range c.pending {
		if !c.sendOp(p) {
			return
		}
	}
}

func (c *Client) sendOp(p PendingOp) bool {
	if !c.live {
		return false
	}
	raw, err := p.raw()
	if err != nil {
		c.fail("marshal payload: %v", err)
		return false
	}
	id := p.ID
	c.stats.OpsSent++
	return c.write(protocol.Envelope{V: protocol.Version, Type: p.Type, OpID: &id, Payload: raw})
}

func (c *Client) rebase() {
	if c.view != nil && !c.dirty {
		return
	}
	v := buildView(c.state)
	kept := c.pending[:0]
	for _, p := range c.pending {
		if v.apply(p) {
			kept = append(kept, p)
			continue
		}
		c.stats.LocalDrops++
	}
	c.pending = kept
	c.view = v
	c.dirty = false
}

func (c *Client) randomOp() bool {
	if !c.initialized || len(c.pending) >= c.cfg.MaxPending {
		return false
	}
	c.rebase()
	op := c.gen.next(c.view)
	if !c.view.apply(op) {
		c.fail("generated op %s is not applicable to the optimistic view", op.Type)
		return false
	}
	c.pending = append(c.pending, op)
	c.stats.OpsCreated++
	c.sendOp(op)
	return true
}

func (c *Client) onNack(env protocol.Envelope) {
	var nack boardcontent.Nack
	if err := json.Unmarshal(env.Payload, &nack); err != nil {
		c.fail("decode nack: %v", err)
		return
	}
	c.stats.Nacks[nack.Reason]++
	if env.OpID == nil {
		c.fail("nack without op_id: %s", nack.Reason)
		return
	}
	idx := -1
	for i, p := range c.pending {
		if p.ID == *env.OpID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	c.pending = append(c.pending[:idx], c.pending[idx+1:]...)
	c.dirty = true
	switch nack.Reason {
	case boardcontent.ReasonVersionConflict, boardcontent.ReasonNotFound:
	case boardcontent.ReasonResyncRequired:
		c.stats.ResyncRequired++
		c.reloadSnapshot(context.Background())
		c.resendPending()
	default:
		c.fail("unexpected nack %q: %s", nack.Reason, nack.Message)
	}
}

func (c *Client) get(ctx context.Context, path string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.pickEndpoint()+path, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func (c *Client) reloadSnapshot(ctx context.Context) {
	var snap protocol.Snapshot
	status, err := c.get(ctx, "/api/boards/"+c.cfg.BoardID.String(), &snap)
	if err != nil || status != http.StatusOK {
		if ctx.Err() == nil {
			c.cfg.Logf("%s: snapshot reload failed: status %d err %v", c.cfg.Name, status, err)
			c.gapDeadline = time.Now().Add(retryFetchDelay)
		}
		return
	}
	c.stats.SnapshotReloads++
	c.loadSnapshot(snap)
}

func (c *Client) fetchEvents(ctx context.Context, source fetchSource) {
	startSeq := c.state.Seq
	for {
		var page protocol.EventsPage
		path := fmt.Sprintf("/api/boards/%s/events?after_seq=%d", c.cfg.BoardID, c.state.Seq)
		status, err := c.get(ctx, path, &page)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				c.cfg.Logf("%s: fetch events failed: %v", c.cfg.Name, err)
				c.gapDeadline = time.Now().Add(retryFetchDelay)
			}
			return
		case status == http.StatusGone:
			c.stats.Gone++
			c.reloadSnapshot(ctx)
			c.resendPending()
			return
		case status != http.StatusOK:
			c.fail("fetch events: status %d", status)
			c.gapDeadline = time.Now().Add(retryFetchDelay * 2)
			return
		}
		before := c.state.Seq
		for _, ev := range page.Events {
			c.acceptEvent(ev)
		}
		if !page.HasMore || len(page.Events) == 0 || c.state.Seq == before {
			break
		}
	}
	applied := int(c.state.Seq - startSeq)
	if source == fetchGap {
		c.stats.GapFetchEvents += applied
	} else {
		c.stats.PollEvents += applied
	}
	if len(c.buf) > 0 && c.gapDeadline.IsZero() {
		c.gapDeadline = time.Now().Add(c.cfg.GapWait)
	}
}
