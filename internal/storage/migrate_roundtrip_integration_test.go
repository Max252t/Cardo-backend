package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
	"github.com/Max252t/Cardo-backend/migrations"
)

func indexNames(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename = $1 ORDER BY indexname`, table)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		out = append(out, n)
	}
	require.NoError(t, rows.Err())
	return out
}

func newMigrator(t *testing.T, pool *pgxpool.Pool) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations.FS, ".")
	require.NoError(t, err)
	cfg := pool.Config().ConnConfig.Copy()
	delete(cfg.RuntimeParams, "statement_timeout")
	db := stdlib.OpenDB(*cfg)
	driver, err := migratepgx.WithInstance(db, &migratepgx.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func TestMigration000003RoundTrip(t *testing.T) {
	pool := testutil.NewPool(t)
	require.NoError(t, storage.Migrate(context.Background(), pool, discardLogger))
	assert.Contains(t, indexNames(t, pool, "board_events"), "board_events_board_created_idx")
	assert.NotContains(t, indexNames(t, pool, "board_events"), "board_events_created_at_idx")

	m := newMigrator(t, pool)
	require.NoError(t, m.Steps(-1))
	version, dirty, err := m.Version()
	require.NoError(t, err)
	assert.EqualValues(t, 2, version)
	assert.False(t, dirty)
	assert.Contains(t, indexNames(t, pool, "board_events"), "board_events_created_at_idx")
	assert.NotContains(t, indexNames(t, pool, "board_events"), "board_events_board_created_idx")

	require.NoError(t, m.Steps(1))
	version, dirty, err = m.Version()
	require.NoError(t, err)
	assert.EqualValues(t, 3, version)
	assert.False(t, dirty)
	assert.Contains(t, indexNames(t, pool, "board_events"), "board_events_board_created_idx")
	assert.NotContains(t, indexNames(t, pool, "board_events"), "board_events_created_at_idx")
}

func TestMigration000003KeepsExistingEvents(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("mig"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 5)

	m := newMigrator(t, pool)
	require.NoError(t, m.Steps(-1))
	assert.Equal(t, seqSpan(1, 5), retainedSeqs(t, pool, b.ID))
	require.NoError(t, m.Steps(1))
	assert.Equal(t, seqSpan(1, 5), retainedSeqs(t, pool, b.ID))
}

func TestPruneProbesUseTheBoardIndexes(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	ctx := context.Background()
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("plan"))
	b := createBoard(t, store, owner.ID, "b")
	seedBulkEvents(t, pool, b.ID, 20000, 10*24*time.Hour)
	_, err := pool.Exec(ctx, `ANALYZE board_events`)
	require.NoError(t, err)

	explain := func(sql string, args ...any) string {
		rows, err := pool.Query(ctx, "EXPLAIN "+sql, args...)
		require.NoError(t, err)
		defer rows.Close()
		var plan string
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line))
			plan += line + "\n"
		}
		require.NoError(t, rows.Err())
		return plan
	}
	expired := explain(`SELECT seq FROM board_events WHERE board_id = $1 AND created_at < $2 ORDER BY created_at DESC LIMIT 1`, b.ID, time.Now())
	assert.Contains(t, expired, "board_events_board_created_idx")
	batch := explain(`DELETE FROM board_events WHERE board_id = $1 AND seq IN (SELECT seq FROM board_events WHERE board_id = $1 AND seq <= $2 ORDER BY seq LIMIT $3)`, b.ID, int64(100), 10)
	assert.Contains(t, batch, "board_events_pkey")
	assert.NotContains(t, batch, "Seq Scan on board_events")
}
