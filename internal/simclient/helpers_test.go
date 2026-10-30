package simclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func rawJSON(t testing.TB, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return data
}

func event(t testing.TB, seq int64, typ string, opID *uuid.UUID, payload any) protocol.Envelope {
	t.Helper()
	return protocol.Envelope{V: protocol.Version, Type: typ, Seq: seq, OpID: opID, Payload: rawJSON(t, payload)}
}

func columnCreatedEvent(t testing.TB, seq int64, opID *uuid.UUID, id uuid.UUID, title, rank string) protocol.Envelope {
	return event(t, seq, boardcontent.EventColumnCreated, opID, boardcontent.ColumnCreated{
		Column: boardcontent.Column{ID: id, Title: title, Rank: rank, Version: 1},
	})
}

func cardCreatedEvent(t testing.TB, seq int64, opID *uuid.UUID, id, columnID uuid.UUID, title, rank string) protocol.Envelope {
	return event(t, seq, boardcontent.EventCardCreated, opID, boardcontent.CardCreated{
		Card: boardcontent.Card{ID: id, ColumnID: columnID, Title: title, Rank: rank, Version: 1, DescriptionVersion: 1},
	})
}

func nackEvent(t testing.TB, opID *uuid.UUID, reason string) protocol.Envelope {
	return protocol.Envelope{V: protocol.Version, Type: protocol.TypeNack, OpID: opID, Payload: rawJSON(t, boardcontent.Nack{Reason: reason, Message: reason})}
}

func ptr[T any](v T) *T { return &v }

type fakeAPI struct {
	t        testing.TB
	boardID  uuid.UUID
	srv      *httptest.Server
	mu       sync.Mutex
	eventReq []string
	auths    []string
	snapReqs int
	events   func(after int64) (int, protocol.EventsPage)
	snapshot func() (int, protocol.Snapshot)
}

func newFakeAPI(t testing.TB, boardID uuid.UUID) *fakeAPI {
	t.Helper()
	f := &fakeAPI{t: t, boardID: boardID}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/boards/"+boardID.String()+"/events", f.serveEvents)
	mux.HandleFunc("/api/boards/"+boardID.String(), f.serveSnapshot)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serveEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after_seq"), 10, 64)
	f.mu.Lock()
	f.eventReq = append(f.eventReq, r.URL.RawQuery)
	f.auths = append(f.auths, r.Header.Get("Authorization"))
	handler := f.events
	f.mu.Unlock()
	if handler == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	status, page := handler(after)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusOK {
		_ = json.NewEncoder(w).Encode(page)
	}
}

func (f *fakeAPI) serveSnapshot(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.snapReqs++
	handler := f.snapshot
	f.mu.Unlock()
	if handler == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	status, snap := handler()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status == http.StatusOK {
		_ = json.NewEncoder(w).Encode(snap)
	}
}

func (f *fakeAPI) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.eventReq...)
}

func (f *fakeAPI) snapshotRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapReqs
}

func newTestClient(t testing.TB, endpoints ...string) *Client {
	t.Helper()
	if len(endpoints) == 0 {
		endpoints = []string{"http://127.0.0.1:1"}
	}
	c := New(Config{
		Name:      "unit",
		BoardID:   uuid.New(),
		Endpoints: endpoints,
		Token:     "token-1",
		Seed:      1,
		GapWait:   40 * time.Millisecond,
		IdlePoll:  time.Hour,
	})
	c.startedAt = time.Now()
	c.nextPoll = time.Now().Add(time.Hour)
	return c
}

func newClientWithAPI(t testing.TB) (*Client, *fakeAPI) {
	t.Helper()
	c := newTestClient(t)
	api := newFakeAPI(t, c.cfg.BoardID)
	c.cfg.Endpoints = []string{api.srv.URL}
	return c, api
}

func loadEmpty(c *Client, seq int64) {
	c.loadSnapshot(protocol.Snapshot{Seq: seq})
}

func pendingOps(c *Client) []uuid.UUID {
	ids := make([]uuid.UUID, len(c.pending))
	for i, p := range c.pending {
		ids[i] = p.ID
	}
	return ids
}

type wsSession struct {
	conn *websocket.Conn
	mu   sync.Mutex
}

func (s *wsSession) send(env protocol.Envelope) {
	data, err := json.Marshal(env)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.conn.WriteMessage(websocket.TextMessage, data)
}

type fakeWS struct {
	t        testing.TB
	srv      *httptest.Server
	mu       sync.Mutex
	hellos   []protocol.Hello
	ops      []protocol.Envelope
	sessions []*wsSession
	snapshot protocol.Snapshot
	onOp     func(s *wsSession, env protocol.Envelope)
}

func newFakeWS(t testing.TB, boardID uuid.UUID) *fakeWS {
	t.Helper()
	f := &fakeWS{t: t}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws/boards/"+boardID.String(), func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		s := &wsSession{conn: conn}
		f.mu.Lock()
		f.sessions = append(f.sessions, s)
		f.mu.Unlock()
		defer func() { _ = conn.Close() }()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			env, err := protocol.ParseEnvelope(data)
			if err != nil {
				return
			}
			f.handle(s, env)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeWS) handle(s *wsSession, env protocol.Envelope) {
	if env.Type == protocol.TypeHello {
		var hello protocol.Hello
		_ = json.Unmarshal(env.Payload, &hello)
		f.mu.Lock()
		f.hellos = append(f.hellos, hello)
		snap := f.snapshot
		f.mu.Unlock()
		if hello.LastSeq == nil {
			snapEnv, _ := protocol.NewEnvelope(protocol.TypeSyncSnapshot, snap)
			s.send(snapEnv)
		} else {
			delta, _ := protocol.NewEnvelope(protocol.TypeSyncDelta, protocol.SyncDelta{Events: []protocol.Envelope{}})
			s.send(delta)
		}
		ready, _ := protocol.NewEnvelope(protocol.TypeSyncReady, nil)
		s.send(ready)
		return
	}
	f.mu.Lock()
	f.ops = append(f.ops, env)
	handler := f.onOp
	f.mu.Unlock()
	if handler != nil {
		handler(s, env)
	}
}

func (f *fakeWS) helloList() []protocol.Hello {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Hello(nil), f.hellos...)
}

func (f *fakeWS) opList() []protocol.Envelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.Envelope(nil), f.ops...)
}
