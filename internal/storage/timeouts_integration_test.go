package storage_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestWithBoardLockTimesOutAsBusy(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool, storage.WithLockTimeout(100*time.Millisecond))
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("busy"))
	b := createBoard(t, store, owner.ID, "Busy")
	ctx := context.Background()

	holder, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	_, err = holder.Exec(ctx, `SELECT id FROM boards WHERE id = $1 FOR UPDATE`, b.ID)
	require.NoError(t, err)

	start := time.Now()
	called := false
	err = store.WithBoardLock(ctx, b.ID, func(context.Context, *storage.BoardTx) error {
		called = true
		return nil
	})
	assert.ErrorIs(t, err, domain.ErrBusy)
	assert.False(t, called)
	assert.Less(t, time.Since(start), 5*time.Second)

	require.NoError(t, holder.Rollback(ctx))
	assert.NoError(t, store.WithBoardLock(ctx, b.ID, func(context.Context, *storage.BoardTx) error { return nil }))
}

func TestWithBoardLockTimeoutIsTransactionLocal(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool, storage.WithLockTimeout(1500*time.Millisecond))
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("local"))
	b := createBoard(t, store, owner.ID, "Local")
	ctx := context.Background()

	err := store.WithBoardLock(ctx, b.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		var v string
		require.NoError(t, btx.Tx().QueryRow(ctx, `SHOW lock_timeout`).Scan(&v))
		assert.Equal(t, "1500ms", v)
		return nil
	})
	require.NoError(t, err)

	conn, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var v string
	require.NoError(t, conn.QueryRow(ctx, `SHOW lock_timeout`).Scan(&v))
	assert.Equal(t, "0", v)
}

func TestPoolStatementTimeout(t *testing.T) {
	dsn := testutil.NewDatabase(t)
	pool, err := storage.NewPool(context.Background(), dsn, 4, storage.WithStatementTimeout(100*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(context.Background(), `SELECT pg_sleep(2)`)
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr), "expected postgres error, got %v", err)
	assert.Equal(t, "57014", pgErr.Code)

	require.NoError(t, storage.Migrate(context.Background(), pool, discardLogger))
	var v string
	require.NoError(t, pool.QueryRow(context.Background(), `SHOW statement_timeout`).Scan(&v))
	assert.Equal(t, "100ms", v, "migration must restore the pool statement timeout")
	_, err = pool.Exec(context.Background(), `SELECT 1 FROM users LIMIT 1`)
	assert.NoError(t, err)
}
