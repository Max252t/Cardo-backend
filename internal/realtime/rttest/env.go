package rttest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/realtime"
)

var jwtSecret = []byte("0123456789abcdef0123456789abcdef")

type Env struct {
	Clock    *FakeClock
	Network  *pubsub.MemoryNetwork
	Presence *presence.MemoryStore
	Tokens   *auth.JWTManager
	Board    *Board
	Config   realtime.Config
	Sync     realtime.SyncSource
	Ops      realtime.OpApplier
	Members  realtime.MemberLookup
}

func NewEnv(t testing.TB) *Env {
	t.Helper()
	network := pubsub.NewMemoryNetwork()
	clock := NewFakeClock(time.Now())
	publisherBroker := network.NewBroker()
	t.Cleanup(func() { _ = publisherBroker.Close() })
	return &Env{
		Clock:    clock,
		Network:  network,
		Presence: presence.NewMemoryStore(),
		Tokens: auth.NewJWTManager(auth.JWTConfig{
			Secret:   jwtSecret,
			Issuer:   "test",
			Audience: "test",
			TTL:      15 * time.Minute,
		}, clock),
		Board: NewBoard(uuid.New(), pubsub.NewEventPublisher(publisherBroker)),
	}
}

func (e *Env) IssueToken(t testing.TB, userID uuid.UUID) string {
	t.Helper()
	token, _, err := e.Tokens.Issue(userID)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return token
}

type Instance struct {
	Name   string
	Hub    *realtime.Hub
	Server *httptest.Server
	Broker *pubsub.MemoryBroker
	env    *Env
}

type unusedAuth struct{ httpapi.AuthService }

type unusedUsers struct{ httpapi.UserService }

type unusedBoards struct{ httpapi.BoardService }

func (e *Env) StartInstance(t testing.TB, name string) *Instance {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	broker := e.Network.NewBroker()
	svc := presence.New(e.Presence, broker, e.Clock, name)
	hub, err := realtime.NewHub(realtime.Deps{
		Bus:      broker,
		Presence: svc,
		Ops:      e.opApplier(),
		Sync:     e.syncSource(),
		Members:  e.memberLookup(),
		Tokens:   e.Tokens,
		Clock:    e.Clock,
	}, e.Config)
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	go pubsub.Pump(broker, hub.Dispatch)
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:    unusedAuth{},
		Tokens:  e.Tokens,
		Users:   unusedUsers{},
		Boards:  unusedBoards{},
		Claims:  e.Tokens,
		Sockets: hub,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	srv := httptest.NewServer(router)
	inst := &Instance{Name: name, Hub: hub, Server: srv, Broker: broker, env: e}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hub.Shutdown(ctx)
		srv.Close()
		_ = broker.Close()
	})
	return inst
}

func (i *Instance) URL(boardID uuid.UUID) string {
	return "ws" + strings.TrimPrefix(i.Server.URL, "http") + "/ws/boards/" + boardID.String()
}

func AddUser(e *Env, name string, role domain.Role) uuid.UUID {
	id := uuid.New()
	e.Board.AddMember(id, name, role)
	return id
}

func (e *Env) syncSource() realtime.SyncSource {
	if e.Sync != nil {
		return e.Sync
	}
	return e.Board
}

func (i *Instance) Dial(t testing.TB, userID uuid.UUID) *Conn {
	t.Helper()
	conn, status, err := i.TryDial(i.env.Board.ID, "Bearer "+i.env.IssueToken(t, userID), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Conn{Conn: conn, t: t}
}

func (i *Instance) TryDial(boardID uuid.UUID, authorization string, header http.Header) (*websocket.Conn, int, error) {
	h := http.Header{}
	for k, v := range header {
		h[k] = v
	}
	if authorization != "" {
		h.Set("Authorization", authorization)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	conn, resp, err := dialer.Dial(i.URL(boardID), h)
	status := 0
	if resp != nil {
		status = resp.StatusCode
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}
	return conn, status, err
}

func (e *Env) opApplier() realtime.OpApplier {
	if e.Ops != nil {
		return e.Ops
	}
	return e.Board
}

func (e *Env) memberLookup() realtime.MemberLookup {
	if e.Members != nil {
		return e.Members
	}
	return e.Board
}

func (e *Env) NewPublisher(t testing.TB) *pubsub.EventPublisher {
	t.Helper()
	broker := e.Network.NewBroker()
	t.Cleanup(func() { _ = broker.Close() })
	return pubsub.NewEventPublisher(broker)
}

func (e *Env) DialBoard(t testing.TB, inst *Instance, boardID uuid.UUID, userID uuid.UUID) *Conn {
	t.Helper()
	conn, status, err := inst.TryDial(boardID, "Bearer "+e.IssueToken(t, userID), nil)
	if err != nil {
		t.Fatalf("dial websocket: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &Conn{Conn: conn, t: t}
}
