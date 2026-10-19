package storage_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

var discardLogger = slog.New(slog.DiscardHandler)

func tableNames(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' ORDER BY table_name`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		names = append(names, n)
	}
	require.NoError(t, rows.Err())
	return names
}

func migrationState(t *testing.T, pool *pgxpool.Pool) (version int64, dirty bool, rows int) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty))
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM schema_migrations`).Scan(&rows))
	return version, dirty, rows
}

func TestMigrateCreatesSchema(t *testing.T) {
	pool := testutil.NewPool(t)
	require.NoError(t, storage.Migrate(context.Background(), pool, discardLogger))

	assert.ElementsMatch(t,
		[]string{"board_events", "board_members", "boards", "cards", "columns", "invites", "processed_ops", "refresh_tokens", "schema_migrations", "users"},
		tableNames(t, pool))

	version, dirty, rows := migrationState(t, pool)
	assert.Equal(t, int64(1), version)
	assert.False(t, dirty)
	assert.Equal(t, 1, rows)

	var ext int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM pg_extension WHERE extname = 'citext'`).Scan(&ext))
	assert.Equal(t, 1, ext)
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testutil.NewPool(t)
	for range 3 {
		require.NoError(t, storage.Migrate(context.Background(), pool, discardLogger))
	}
	version, dirty, rows := migrationState(t, pool)
	assert.Equal(t, int64(1), version)
	assert.False(t, dirty)
	assert.Equal(t, 1, rows)
}

func TestMigrateConcurrentCalls(t *testing.T) {
	dsn := testutil.NewDatabase(t)
	const instances = 3
	const callsPerInstance = 3

	pools := make([]*pgxpool.Pool, instances)
	for i := range pools {
		p, err := storage.NewPool(context.Background(), dsn, 10)
		require.NoError(t, err)
		t.Cleanup(p.Close)
		pools[i] = p
	}

	var wg sync.WaitGroup
	errs := make(chan error, instances*callsPerInstance)
	start := make(chan struct{})
	for _, p := range pools {
		for range callsPerInstance {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- storage.Migrate(context.Background(), p, discardLogger)
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		assert.NoError(t, err)
	}

	version, dirty, rows := migrationState(t, pools[0])
	assert.Equal(t, int64(1), version)
	assert.False(t, dirty)
	assert.Equal(t, 1, rows)
	assert.Contains(t, tableNames(t, pools[0]), "users")
}

func TestMigrateReleasesAdvisoryLock(t *testing.T) {
	pool := testutil.NewPool(t)
	require.NoError(t, storage.Migrate(context.Background(), pool, discardLogger))

	var held int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&held))
	assert.Zero(t, held, "no advisory locks must outlive Migrate")
}

func TestMigrateFailsWithCanceledContext(t *testing.T) {
	pool := testutil.NewPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Error(t, storage.Migrate(ctx, pool, discardLogger))
}

func TestSchemaConstraints(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()

	const insertUser = `INSERT INTO users (id, email, password_hash, display_name, avatar_color)
		VALUES (gen_random_uuid(), $1, 'h', $2, '#000000')`

	_, err := pool.Exec(ctx, insertUser, "Dup@Example.com", "A")
	require.NoError(t, err)

	_, err = pool.Exec(ctx, insertUser, "dup@example.com", "B")
	require.Error(t, err, "email uniqueness must be case-insensitive")

	_, err = pool.Exec(ctx, insertUser, "empty-name@example.com", "")
	require.Error(t, err, "empty display_name must be rejected")

	long := make([]byte, 101)
	for i := range long {
		long[i] = 'a'
	}
	_, err = pool.Exec(ctx, insertUser, "long-name@example.com", string(long))
	require.Error(t, err, "display_name over 100 chars must be rejected")

	var userID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM users WHERE email = 'dup@example.com'`).Scan(&userID))

	_, err = pool.Exec(ctx, `INSERT INTO boards (id, title, owner_id) VALUES (gen_random_uuid(), '', $1)`, userID)
	require.Error(t, err, "empty board title must be rejected")

	var boardID string
	require.NoError(t, pool.QueryRow(ctx,
		`INSERT INTO boards (id, title, owner_id) VALUES (gen_random_uuid(), 'T', $1) RETURNING id::text`, userID).Scan(&boardID))

	_, err = pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'admin')`, boardID, userID)
	require.Error(t, err, "unknown role must be rejected")

	_, err = pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'owner')`, boardID, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'viewer')`, boardID, userID)
	require.Error(t, err, "membership is unique per board and user")

	_, err = pool.Exec(ctx, `INSERT INTO board_events (board_id, seq, type, payload) VALUES ($1, 1, 'x', '{}')`, boardID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO board_events (board_id, seq, type, payload) VALUES ($1, 1, 'x', '{}')`, boardID)
	require.Error(t, err, "board_events.(board_id, seq) must be unique")

	_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	require.Error(t, err, "a user that owns a board cannot be deleted")
}
