package testutil

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

const (
	ExternalPostgresEnv = "TEST_DATABASE_URL"
	ExternalRedisEnv    = "TEST_REDIS_URL"
)

const (
	postgresImage = "postgres:16-alpine"
	redisImage    = "redis:7-alpine"
	startTimeout  = 3 * time.Minute
)

type shared struct {
	once sync.Once
	ctr  testcontainers.Container
	dsn  string
	err  error
}

var (
	pgContainer    shared
	redisContainer shared
	dbCounter      atomic.Int64
)

func RunMain(m *testing.M) {
	code := m.Run()
	Terminate()
	os.Exit(code)
}

func Terminate() {
	for _, s := range []*shared{&pgContainer, &redisContainer} {
		if s.ctr != nil {
			_ = testcontainers.TerminateContainer(s.ctr)
		}
	}
}

var (
	dockerOnce sync.Once
	dockerErr  error
)

func dockerRequired() bool {
	return strings.EqualFold(os.Getenv("CI"), "true") || os.Getenv("REQUIRE_DOCKER") == "1"
}

func dockerHealth() error {
	dockerOnce.Do(func() {
		dockerErr = probeDocker()
	})
	return dockerErr
}

func probeDocker() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("docker provider panicked: %v", r)
		}
	}()
	provider, err := testcontainers.ProviderDocker.GetProvider()
	if err != nil {
		return err
	}
	defer func() { _ = provider.Close() }()
	return provider.Health(context.Background())
}

func requireDocker(t testing.TB) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in short mode")
	}
	err := dockerHealth()
	if err == nil {
		return
	}
	if dockerRequired() {
		t.Fatalf("docker is required (CI=true or REQUIRE_DOCKER=1) but unavailable: %v", err)
	}
	t.Skipf("docker is not available, skipping integration test: %v", err)
}

func RequireDocker(t testing.TB) {
	t.Helper()
	requireDocker(t)
}

func startPostgres(ctx context.Context) (testcontainers.Container, string, error) {
	ctr, err := tcpostgres.Run(ctx, postgresImage, tcpostgres.BasicWaitStrategies())
	if err != nil {
		return ctr, "", fmt.Errorf("start postgres container: %w", err)
	}
	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return ctr, "", fmt.Errorf("postgres connection string: %w", err)
	}
	return ctr, dsn, nil
}

func startRedis(ctx context.Context) (testcontainers.Container, string, error) {
	ctr, err := tcredis.Run(ctx, redisImage)
	if err != nil {
		return ctr, "", fmt.Errorf("start redis container: %w", err)
	}
	uri, err := ctr.ConnectionString(ctx)
	if err != nil {
		return ctr, "", fmt.Errorf("redis connection string: %w", err)
	}
	return ctr, uri, nil
}

func (s *shared) start(fn func(context.Context) (testcontainers.Container, string, error)) {
	s.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()
		s.ctr, s.dsn, s.err = fn(ctx)
	})
}

func postgresAdminDSN(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in short mode")
	}
	if dsn := os.Getenv(ExternalPostgresEnv); dsn != "" {
		return dsn
	}
	requireDocker(t)
	pgContainer.start(startPostgres)
	if pgContainer.err != nil {
		t.Fatalf("postgres container: %v", pgContainer.err)
	}
	return pgContainer.dsn
}

func NewDatabase(t testing.TB) string {
	t.Helper()
	adminDSN := postgresAdminDSN(t)

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect to postgres admin database: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	name := fmt.Sprintf("cardo_test_%d_%d", os.Getpid(), dbCounter.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		_, _ = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	})

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse postgres dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func NewPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dsn := NewDatabase(t)
	pool, err := storage.NewPool(context.Background(), dsn, 20)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func NewMigratedPool(t testing.TB) *pgxpool.Pool {
	t.Helper()
	pool := NewPool(t)
	if err := storage.Migrate(context.Background(), pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func NewStore(t testing.TB) *storage.Store {
	t.Helper()
	return storage.New(NewMigratedPool(t))
}

func redisURL(t testing.TB) string {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in short mode")
	}
	if u := os.Getenv(ExternalRedisEnv); u != "" {
		return u
	}
	requireDocker(t)
	redisContainer.start(startRedis)
	if redisContainer.err != nil {
		t.Fatalf("redis container: %v", redisContainer.err)
	}
	return redisContainer.dsn
}

func NewRedis(t testing.TB) *redis.Client {
	t.Helper()
	opts, err := redis.ParseURL(redisURL(t))
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	client := redis.NewClient(opts)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return client
}

func CreateUser(t testing.TB, store *storage.Store, email string) domain.User {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}
	u, err := store.CreateUser(context.Background(), domain.User{
		ID:           id,
		Email:        email,
		PasswordHash: "hash",
		DisplayName:  "User " + email,
		AvatarColor:  "#112233",
	})
	if err != nil {
		t.Fatalf("create user %s: %v", email, err)
	}
	return u
}

func UniqueEmail(prefix string) string {
	return fmt.Sprintf("%s-%s@example.com", prefix, uuid.NewString()[:8])
}
