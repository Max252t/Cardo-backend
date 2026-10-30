package convergence_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/jwtauth"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/presencestore"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type transport int

const (
	transportMemory transport = iota
	transportRedis
)

func (t transport) String() string {
	if t == transportRedis {
		return "redis"
	}
	return "memory"
}

type dropFilter struct {
	mu       sync.Mutex
	rng      *rand.Rand
	prob     float64
	blackout bool
	dropped  atomic.Int64
}

func newDropFilter(seed int64) *dropFilter {
	return &dropFilter{rng: rand.New(rand.NewSource(seed))}
}

func (d *dropFilter) setProbability(p float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prob = p
}

func (d *dropFilter) setBlackout(on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.blackout = on
}

func (d *dropFilter) drop(m pubsub.Message) bool {
	if !strings.HasPrefix(m.Channel, "board:") {
		return false
	}
	d.mu.Lock()
	lose := d.blackout || (d.prob > 0 && d.rng.Float64() < d.prob)
	d.mu.Unlock()
	if lose {
		d.dropped.Add(1)
	}
	return lose
}

type lossyBroker struct {
	pubsub.Broker
	out chan pubsub.Message
}

func newLossyBroker(inner pubsub.Broker, drop func(pubsub.Message) bool, seen func(pubsub.Message)) *lossyBroker {
	l := &lossyBroker{Broker: inner, out: make(chan pubsub.Message, 4096)}
	go func() {
		defer close(l.out)
		for m := range inner.Messages() {
			seen(m)
			if drop(m) {
				continue
			}
			l.out <- m
		}
	}()
	return l
}

func (l *lossyBroker) Messages() <-chan pubsub.Message {
	return l.out
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type unusedAuth struct{ httpapi.AuthService }

type unusedUsers struct{ httpapi.UserService }

type instance struct {
	hub    *realtime.Hub
	server *httptest.Server
}

type stack struct {
	store     *storage.Store
	jwt       *jwtauth.Manager
	reader    *boards.Service
	instances []*instance
	filter    *dropFilter
	logs      *syncBuffer
	logger    *slog.Logger
	hubLogs   []*hubLog
	owner     domain.User
	members   []domain.User
	boardID   uuid.UUID
}

func (s *stack) endpoints() []string {
	out := make([]string, len(s.instances))
	for i, inst := range s.instances {
		out[i] = inst.server.URL
	}
	return out
}

func (s *stack) token(t testing.TB, userID uuid.UUID) string {
	t.Helper()
	token, _, err := s.jwt.Issue(userID)
	require.NoError(t, err)
	return token
}

func (s *stack) snapshot(ctx context.Context) (domain.BoardSnapshot, error) {
	return s.reader.Snapshot(ctx, s.owner.ID, s.boardID)
}

func newStack(t *testing.T, tr transport, users, instances int, seed int64) *stack {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	ctx := context.Background()
	store := testutil.NewStore(t)
	jwt := jwtauth.New(jwtauth.Config{
		Secret:   []byte("0123456789abcdef0123456789abcdef"),
		Issuer:   "convergence",
		Audience: "convergence",
		TTL:      time.Hour,
	}, nil)
	s := &stack{store: store, jwt: jwt, filter: newDropFilter(seed), boardID: uuid.New(), logs: &syncBuffer{}}
	s.logger = slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server logs:\n%s", s.logs.String())
		}
	})
	s.reader = boards.NewService(store)

	for i := range users {
		u := testutil.CreateUser(t, store, testutil.UniqueEmail("sim"))
		if i == 0 {
			s.owner = u
			continue
		}
		s.members = append(s.members, u)
	}
	_, err := store.CreateBoardWithOwner(ctx, domain.Board{ID: s.boardID, Title: "Convergence", OwnerID: s.owner.ID})
	require.NoError(t, err)
	for _, m := range s.members {
		_, err := store.Pool().Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'editor')`, s.boardID, m.ID)
		require.NoError(t, err)
	}

	var rdb *redis.Client
	var network *pubsub.MemoryNetwork
	var presenceStore presence.Store
	switch tr {
	case transportRedis:
		rdb = testutil.NewRedis(t)
		presenceStore = presencestore.NewRedisStore(rdb)
	default:
		network = pubsub.NewMemoryNetwork()
		network.SetDropFilter(s.filter.drop)
		presenceStore = presencestore.NewMemoryStore()
	}
	for i := range instances {
		s.instances = append(s.instances, s.startInstance(t, i, tr, rdb, network, presenceStore))
	}
	return s
}

func (s *stack) startInstance(t *testing.T, idx int, tr transport, rdb *redis.Client, network *pubsub.MemoryNetwork, presenceStore presence.Store) *instance {
	t.Helper()
	var broker pubsub.Broker
	var bus pubsub.Broker
	if tr == transportRedis {
		broker = pubsub.NewRedisBroker(context.Background(), rdb)
		log := s.hubLog(idx)
		bus = newLossyBroker(broker, s.filter.drop, log.observe)
	} else {
		broker = network.NewBroker()
		bus = broker
	}
	publisher := pubsub.NewEventPublisher(broker)
	boardService := boards.NewService(s.store, boards.WithPublisher(publisher), boards.WithLogger(s.logger))
	processor := ops.NewProcessor(s.store, ops.WithPublisher(publisher), ops.WithLogger(s.logger))
	clock := realtime.SystemClock{}
	name := "instance-" + string(rune('a'+idx))
	hub, err := realtime.NewHub(realtime.Deps{
		Bus:      bus,
		Presence: presence.New(presenceStore, pubsub.NewPresenceNotifier(broker), clock, name),
		Ops:      processor,
		Sync:     boardService,
		Members:  s.store,
		Tokens:   s.jwt,
		Clock:    clock,
		Logger:   s.logger,
	}, realtime.Config{OpsPerSecond: 1000, OpsBurst: 1000})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	go pubsub.Pump(bus, hub.Dispatch)
	go hub.Run(ctx)
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:    unusedAuth{},
		Tokens:  s.jwt,
		Users:   unusedUsers{},
		Boards:  boardService,
		Claims:  s.jwt,
		Sockets: hub,
	})
	require.NoError(t, err)
	srv := httptest.NewServer(router)
	t.Cleanup(func() {
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = hub.Shutdown(shutdown)
		srv.Close()
		_ = broker.Close()
	})
	return &instance{hub: hub, server: srv}
}

type hubLog struct {
	mu   sync.Mutex
	seqs map[int64]bool
}

func (h *hubLog) observe(m pubsub.Message) {
	if !strings.HasPrefix(m.Channel, "board:") {
		return
	}
	var env struct {
		Seq int64 `json:"seq"`
	}
	if json.Unmarshal(m.Payload, &env) != nil || env.Seq == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seqs[env.Seq] = true
}

func (h *hubLog) missing(upTo int64) []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []int64
	for seq := int64(1); seq <= upTo; seq++ {
		if !h.seqs[seq] {
			out = append(out, seq)
		}
	}
	return out
}

func (s *stack) hubLog(idx int) *hubLog {
	for len(s.hubLogs) <= idx {
		s.hubLogs = append(s.hubLogs, &hubLog{seqs: make(map[int64]bool)})
	}
	return s.hubLogs[idx]
}
