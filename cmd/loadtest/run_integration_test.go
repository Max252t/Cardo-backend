package main

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/argon2id"
	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/jwtauth"
	"github.com/Max252t/Cardo-backend/internal/members"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/presencestore"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/ratelimit"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/testutil"
	"github.com/Max252t/Cardo-backend/internal/users"
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

func startAPI(t *testing.T) string {
	t.Helper()
	gin.SetMode(gin.ReleaseMode)
	store := testutil.NewStore(t)
	jwt := jwtauth.New(jwtauth.Config{
		Secret:   []byte("0123456789abcdef0123456789abcdef"),
		Issuer:   "loadtest",
		Audience: "loadtest",
		TTL:      time.Hour,
	}, nil)
	authSvc := auth.NewService(auth.Deps{
		Store:      store,
		Hasher:     argon2id.New(argon2id.Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}),
		Access:     jwt,
		Limiter:    ratelimit.NewMemoryLimiter(5, time.Minute, nil),
		RefreshTTL: time.Hour,
	})
	network := pubsub.NewMemoryNetwork()
	broker := network.NewBroker()
	publisher := pubsub.NewEventPublisher(broker)
	boardSvc := boards.NewService(store, boards.WithPublisher(publisher))
	clock := realtime.SystemClock{}
	hub, err := realtime.NewHub(realtime.Deps{
		Bus:      broker,
		Presence: presence.New(presencestore.NewMemoryStore(), pubsub.NewPresenceNotifier(broker), clock, "a"),
		Ops:      ops.NewProcessor(store, ops.WithPublisher(publisher)),
		Sync:     boardSvc,
		Members:  store,
		Tokens:   jwt,
		Clock:    clock,
	}, realtime.Config{})
	require.NoError(t, err)
	go pubsub.Pump(broker, hub.Dispatch)
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:    authSvc,
		Tokens:  jwt,
		Users:   users.NewService(store),
		Boards:  boardSvc,
		Claims:  jwt,
		Sockets: hub,
		Members: members.NewService(store, members.WithPublisher(publisher)),
	})
	require.NoError(t, err)
	srv := httptest.NewServer(router)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = hub.Shutdown(ctx)
		srv.Close()
		_ = broker.Close()
	})
	return srv.URL
}

func TestLoadTestRunsEndToEndAgainstARealStack(t *testing.T) {
	url := startAPI(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	report, err := run(ctx, options{
		URL:                 url,
		Clients:             6,
		Rate:                40,
		Duration:            3 * time.Second,
		Warmup:              500 * time.Millisecond,
		ResyncEvents:        60,
		ResyncTrials:        2,
		RegisterParallelism: 3,
		Seed:                7,
		P95Target:           5 * time.Second,
		ResyncTarget:        10 * time.Second,
		Password:            defaultPassword,
	}, io.Discard)
	require.NoError(t, err)

	assert.Empty(t, report.Failures)
	assert.Greater(t, report.Ops.Sent, int64(80))
	assert.Zero(t, report.Ops.MissingEvents)
	assert.Zero(t, report.Ops.SendErrors)
	assert.Greater(t, report.Latency.Count, int(report.Ops.Sent))
	assert.Positive(t, report.Latency.P95Ms)
	assert.Positive(t, report.LatencyPeer.Count)
	assert.Positive(t, report.LatencySelf.Count)
	assert.Equal(t, 6, report.Connect.Count)
	require.Len(t, report.Resync, 2)
	for _, trial := range report.Resync {
		assert.True(t, trial.CaughtUp)
		assert.Equal(t, "delta", trial.Handshake.Kind)
		assert.GreaterOrEqual(t, trial.Missed, int64(30))
		assert.Positive(t, trial.Handshake.ReadyMs)
	}
	assert.Equal(t, "snapshot", report.SnapshotSync.Kind)
	assert.True(t, report.Passed())
}
