package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type echoConn struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (e *echoConn) send(env protocol.Envelope) {
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.conn.WriteMessage(websocket.TextMessage, data)
}

type echoServer struct {
	srv      *httptest.Server
	mu       sync.Mutex
	conns    []*echoConn
	seq      int64
	onlyBack bool
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	e := &echoServer{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ec := &echoConn{conn: conn}
		e.mu.Lock()
		e.conns = append(e.conns, ec)
		e.mu.Unlock()
		defer func() { _ = conn.Close() }()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var env protocol.Envelope
			if json.Unmarshal(data, &env) != nil {
				return
			}
			e.handle(ec, env)
		}
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *echoServer) wsURL() string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http")
}

func (e *echoServer) handle(from *echoConn, env protocol.Envelope) {
	if env.Type == protocol.TypeHello {
		var hello protocol.Hello
		_ = json.Unmarshal(env.Payload, &hello)
		if hello.LastSeq == nil {
			snap, _ := protocol.NewEnvelope(protocol.TypeSyncSnapshot, protocol.Snapshot{Seq: e.currentSeq()})
			from.send(snap)
		} else {
			delta, _ := protocol.NewEnvelope(protocol.TypeSyncDelta, protocol.SyncDelta{Events: []protocol.Envelope{}})
			from.send(delta)
		}
		ready, _ := protocol.NewEnvelope(protocol.TypeSyncReady, nil)
		from.send(ready)
		return
	}
	e.mu.Lock()
	e.seq++
	ev := protocol.Envelope{V: protocol.Version, Type: "card.created", OpID: env.OpID, Seq: e.seq, Payload: env.Payload}
	targets := append([]*echoConn(nil), e.conns...)
	if e.onlyBack {
		targets = []*echoConn{from}
	}
	e.mu.Unlock()
	for _, c := range targets {
		c.send(ev)
	}
}

func (e *echoServer) currentSeq() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.seq
}

func (e *echoServer) kickAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.conns {
		_ = c.conn.Close()
	}
}

func newClientsOn(t *testing.T, e *echoServer, col *collector, n int) []*wsClient {
	t.Helper()
	w := newWorld()
	clients := make([]*wsClient, n)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := range clients {
		clients[i] = newWSClient(i, account{Token: "t"}, e.wsURL(), col, w)
		_, err := clients[i].connect(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(clients[i].stop)
	}
	return clients
}

func someOp() plannedOp {
	return plannedOp{opType: boardcontent.OpCardCreate, id: uuid.New(), payload: boardcontent.CardCreate{CardID: uuid.New(), ColumnID: uuid.New(), Title: "t"}}
}

func TestSendTimeIsRecordedBeforeAnEventCanComeBack(t *testing.T) {
	e := newEchoServer(t)
	col := newCollector()
	col.recording.Store(true)
	clients := newClientsOn(t, e, col, 3)

	const perClient = 150
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perClient {
				assert.NoError(t, c.sendOp(someOp(), true))
			}
		}()
	}
	wg.Wait()
	require.Eventually(t, func() bool { return col.delivery(3).MissingEvents == 0 }, 10*time.Second, 10*time.Millisecond)

	d := col.delivery(3)
	assert.Equal(t, int64(3*perClient), d.OpsSent)
	assert.Equal(t, 3*perClient, d.OpsComplete)
	all, self, peer := col.distributions()
	assert.Equal(t, 9*perClient, all.Count)
	assert.Equal(t, 3*perClient, self.Count)
	assert.Equal(t, 6*perClient, peer.Count)
	assert.GreaterOrEqual(t, all.MinMs, 0.0)
	assert.Empty(t, col.failureList())
}

func TestEventsAreNotCorrelatedWhenNotRecording(t *testing.T) {
	e := newEchoServer(t)
	e.onlyBack = true
	col := newCollector()
	clients := newClientsOn(t, e, col, 1)
	require.NoError(t, clients[0].sendOp(someOp(), false))
	require.Eventually(t, func() bool { return clients[0].received.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	all, _, _ := col.distributions()
	assert.Zero(t, all.Count)
	assert.Zero(t, col.sentOps.Load())
}

func TestHandshakeKindsAndCatchUpSequence(t *testing.T) {
	e := newEchoServer(t)
	e.onlyBack = true
	col := newCollector()
	c := newWSClient(0, account{Token: "t"}, e.wsURL(), col, newWorld())
	t.Cleanup(c.stop)
	ctx := context.Background()

	res, err := c.connect(ctx, nil)
	require.NoError(t, err)
	assert.Equal(t, "snapshot", res.Kind)
	assert.Positive(t, res.ReadyMs)

	for range 5 {
		require.NoError(t, c.sendOp(someOp(), false))
	}
	require.Eventually(t, func() bool { return c.lastSeq.Load() == 5 }, 5*time.Second, 5*time.Millisecond)

	c.disconnect()
	last := c.lastSeq.Load()
	res, err = c.connect(ctx, &last)
	require.NoError(t, err)
	assert.Equal(t, "delta", res.Kind)
	assert.Equal(t, int64(5), c.lastSeq.Load())
	assert.Empty(t, col.failureList(), "a deliberate disconnect is not a failure")
}

func TestUnexpectedDisconnectIsRecordedButStopIsNot(t *testing.T) {
	e := newEchoServer(t)
	col := newCollector()
	clients := newClientsOn(t, e, col, 2)
	e.kickAll()
	require.Eventually(t, func() bool { return len(col.failureList()) == 2 }, 5*time.Second, 10*time.Millisecond)
	for _, f := range col.failureList() {
		assert.Contains(t, f, "connection ended")
	}
	for _, c := range clients {
		c.stop()
	}
	assert.Len(t, col.failureList(), 2)
}

func TestSendOnADisconnectedClientFails(t *testing.T) {
	c := newWSClient(0, account{}, "ws://127.0.0.1:1", newCollector(), newWorld())
	err := c.sendOp(someOp(), true)
	require.Error(t, err)
	assert.Zero(t, c.col.sentOps.Load())
}

func TestConnectToAnUnreachableServerFails(t *testing.T) {
	c := newWSClient(3, account{}, "ws://127.0.0.1:1", newCollector(), newWorld())
	_, err := c.connect(context.Background(), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dial client 3")
}

func TestConnectHonoursContextCancellationDuringHandshake(t *testing.T) {
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(silent.Close)
	c := newWSClient(0, account{}, "ws"+strings.TrimPrefix(silent.URL, "http"), newCollector(), newWorld())
	t.Cleanup(c.stop)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.connect(ctx, nil)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 3*time.Second)
}

func newHandleFixture() (*wsClient, *session, *collector) {
	col := newCollector()
	c := newWSClient(0, account{}, "", col, newWorld())
	s := &session{start: time.Now(), ready: make(chan time.Duration, 1), closed: make(chan struct{})}
	return c, s, col
}

func envelope(t *testing.T, typ string, seq int64, opID *uuid.UUID, payload any) protocol.Envelope {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return protocol.Envelope{V: protocol.Version, Type: typ, Seq: seq, OpID: opID, Payload: raw}
}

func TestHandleDeltaCountsEventsAndAdvancesTheSequence(t *testing.T) {
	c, s, _ := newHandleFixture()
	delta := protocol.SyncDelta{Events: []protocol.Envelope{
		envelope(t, "card.created", 4, nil, map[string]string{}),
		envelope(t, "card.created", 5, nil, map[string]string{}),
	}}
	c.handle(s, envelope(t, protocol.TypeSyncDelta, 0, nil, delta), s.start.Add(20*time.Millisecond))
	assert.Equal(t, "delta", s.kind)
	assert.Equal(t, 2, s.events)
	assert.Equal(t, int64(5), c.lastSeq.Load())
	assert.Equal(t, int64(2), c.received.Load())
	assert.Equal(t, 20*time.Millisecond, s.firstSync)
}

func TestHandleSnapshotRecordsTheSequenceAndSize(t *testing.T) {
	c, s, col := newHandleFixture()
	snap := protocol.Snapshot{Seq: 40, Columns: make([]boardcontent.Column, 2), Cards: make([]boardcontent.Card, 3)}
	c.handle(s, envelope(t, protocol.TypeSyncSnapshot, 0, nil, snap), s.start.Add(time.Millisecond))
	assert.Equal(t, "snapshot", s.kind)
	assert.Equal(t, 5, s.events)
	assert.Equal(t, int64(40), c.lastSeq.Load())
	assert.Equal(t, int64(40), col.maxSeq.Load())
}

func TestHandleNeverMovesTheSequenceBackwards(t *testing.T) {
	c, s, col := newHandleFixture()
	c.advance(10)
	c.advance(7)
	assert.Equal(t, int64(10), c.lastSeq.Load())
	c.handle(s, envelope(t, protocol.TypeSyncSnapshot, 0, nil, protocol.Snapshot{Seq: 3}), time.Now())
	assert.Equal(t, int64(10), c.lastSeq.Load())
	assert.Equal(t, int64(10), col.maxSeq.Load())
}

func TestHandleReadyDeliversOnceAndNeverBlocks(t *testing.T) {
	c, s, _ := newHandleFixture()
	at := s.start.Add(15 * time.Millisecond)
	c.handle(s, protocol.Envelope{V: protocol.Version, Type: protocol.TypeSyncReady}, at)
	c.handle(s, protocol.Envelope{V: protocol.Version, Type: protocol.TypeSyncReady}, at.Add(time.Second))
	assert.Equal(t, 15*time.Millisecond, <-s.ready)
	assert.Empty(t, s.ready)
}

func TestHandleLiveEventRecordsLatencyOnlyWhileRecording(t *testing.T) {
	c, s, col := newHandleFixture()
	id := uuid.New()
	sent := time.Now()
	col.recording.Store(true)
	col.opSent(id, 1, sent)
	c.handle(s, envelope(t, "card.created", 9, &id, map[string]string{}), sent.Add(25*time.Millisecond))
	all, _, peer := col.distributions()
	assert.Equal(t, 1, all.Count)
	assert.Equal(t, 1, peer.Count)
	assert.InDelta(t, 25, all.P50Ms, 0.001)
	assert.Equal(t, int64(9), c.lastSeq.Load())

	col.recording.Store(false)
	c.handle(s, envelope(t, "card.created", 10, &id, map[string]string{}), sent.Add(time.Second))
	all, _, _ = col.distributions()
	assert.Equal(t, 1, all.Count)
}

func TestHandleNackErrorAndMalformedMessages(t *testing.T) {
	c, s, col := newHandleFixture()
	id := uuid.New()
	col.recording.Store(true)
	col.opSent(id, 0, time.Now())
	c.handle(s, envelope(t, protocol.TypeNack, 0, &id, boardcontent.Nack{Reason: "version_conflict"}), time.Now())
	assert.Equal(t, map[string]int{"version_conflict": 1}, col.nackCounts())
	assert.Empty(t, col.failureList())

	c.handle(s, envelope(t, protocol.TypeNack, 0, nil, boardcontent.Nack{Reason: "x"}), time.Now())
	c.handle(s, protocol.Envelope{V: protocol.Version, Type: protocol.TypeNack, OpID: &id, Payload: json.RawMessage(`"bad"`)}, time.Now())
	c.handle(s, envelope(t, protocol.TypeError, 0, nil, protocol.ErrorPayload{Code: "boom"}), time.Now())
	c.handle(s, protocol.Envelope{V: protocol.Version, Type: protocol.TypeSyncDelta, Payload: json.RawMessage(`1`)}, time.Now())
	c.handle(s, protocol.Envelope{V: protocol.Version, Type: protocol.TypeSyncSnapshot, Payload: json.RawMessage(`1`)}, time.Now())
	assert.Len(t, col.failureList(), 5)

	c.handle(s, protocol.Envelope{V: protocol.Version, Type: protocol.TypePresenceJoin}, time.Now())
	c.handle(s, protocol.Envelope{V: protocol.Version, Type: protocol.TypeAuthRefreshed}, time.Now())
	assert.Len(t, col.failureList(), 5)
}

func TestOnlyTheOwnerClientFeedsTheWorld(t *testing.T) {
	w := newWorld()
	col := newCollector()
	owner := newWSClient(0, account{}, "", col, w)
	member := newWSClient(1, account{}, "", col, w)
	s := &session{start: time.Now(), ready: make(chan time.Duration, 1)}
	created := envelope(t, boardcontent.EventColumnCreated, 1, nil, boardcontent.ColumnCreated{Column: boardcontent.Column{ID: uuid.New()}})
	member.handle(s, created, time.Now())
	assert.Zero(t, w.columnCount())
	owner.handle(s, created, time.Now())
	assert.Equal(t, 1, w.columnCount())
}
