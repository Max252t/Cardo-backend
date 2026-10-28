package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

const (
	testEventsLockKey int64 = 0x63617264_72657431
	testOpsLockKey    int64 = 0x63617264_72657432
)

func appendEvents(t *testing.T, store *storage.Store, boardID uuid.UUID, n int) {
	t.Helper()
	err := store.WithBoardLock(context.Background(), boardID, func(ctx context.Context, btx boardtx.Tx) error {
		for range n {
			if _, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "t.tick"}); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
}

func ageEvents(t *testing.T, pool *pgxpool.Pool, boardID uuid.UUID, upToSeq int64, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE board_events SET created_at = now() - make_interval(secs => $3) + seq * interval '1 millisecond' WHERE board_id = $1 AND seq <= $2`,
		boardID, upToSeq, age.Seconds())
	require.NoError(t, err)
}

func retainedSeqs(t *testing.T, pool *pgxpool.Pool, boardID uuid.UUID) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT seq FROM board_events WHERE board_id = $1 ORDER BY seq`, boardID)
	require.NoError(t, err)
	defer rows.Close()
	var seqs []int64
	for rows.Next() {
		var s int64
		require.NoError(t, rows.Scan(&s))
		seqs = append(seqs, s)
	}
	require.NoError(t, rows.Err())
	return seqs
}

func seqSpan(from, to int64) []int64 {
	var out []int64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func TestPruneEventsKeepsNewestPerBoardAndRecentEvents(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("prune"))
	busy := createBoard(t, store, owner.ID, "busy")
	quiet := createBoard(t, store, owner.ID, "quiet")
	fresh := createBoard(t, store, owner.ID, "fresh")

	appendEvents(t, store, busy.ID, 40)
	appendEvents(t, store, quiet.ID, 5)
	appendEvents(t, store, fresh.ID, 20)
	ageEvents(t, pool, busy.ID, 40, 10*24*time.Hour)
	ageEvents(t, pool, quiet.ID, 5, 10*24*time.Hour)

	res, err := store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 10, 7)
	require.NoError(t, err)
	assert.False(t, res.Skipped)
	assert.EqualValues(t, 30, res.Deleted)

	assert.Equal(t, seqSpan(31, 40), retainedSeqs(t, pool, busy.ID), "only the newest 10 events of a busy board survive")
	assert.Equal(t, seqSpan(1, 5), retainedSeqs(t, pool, quiet.ID), "a board below the minimum keeps everything")
	assert.Equal(t, seqSpan(1, 20), retainedSeqs(t, pool, fresh.ID), "events inside the window are never pruned")

	res, err = store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 10, 7)
	require.NoError(t, err)
	assert.EqualValues(t, 0, res.Deleted)
}

func TestPruneEventsKeepMinZeroRemovesAllExpired(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("prune0"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 12)
	ageEvents(t, pool, b.ID, 12, 3*time.Hour)

	res, err := store.PruneEvents(context.Background(), time.Now().Add(-time.Hour), 0, 5)
	require.NoError(t, err)
	assert.EqualValues(t, 12, res.Deleted)
	assert.Empty(t, retainedSeqs(t, pool, b.ID))
}

func TestPruneEventsRemovesOldestFirstAndKeepsSeqContiguous(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("prefix"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 30)
	ageEvents(t, pool, b.ID, 20, 9*24*time.Hour)

	_, err := store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 5, 3)
	require.NoError(t, err)
	assert.Equal(t, seqSpan(21, 30), retainedSeqs(t, pool, b.ID))
}

func TestPruneProcessedOpsDeletesOnlyExpiredInBatches(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("ops"))
	b := createBoard(t, store, owner.ID, "b")

	var old, fresh []uuid.UUID
	err := store.WithBoardLock(context.Background(), b.ID, func(ctx context.Context, btx boardtx.Tx) error {
		for i := range 25 {
			id := uuid.New()
			if _, err := btx.SaveProcessedOp(ctx, id, owner.ID, map[string]any{"v": 1}); err != nil {
				return err
			}
			if i < 17 {
				old = append(old, id)
			} else {
				fresh = append(fresh, id)
			}
		}
		return nil
	})
	require.NoError(t, err)
	_, err = pool.Exec(context.Background(), `UPDATE processed_ops SET created_at = now() - interval '30 hours' WHERE op_id = ANY($1)`, old)
	require.NoError(t, err)

	res, err := store.PruneProcessedOps(context.Background(), time.Now().Add(-24*time.Hour), 4)
	require.NoError(t, err)
	assert.EqualValues(t, 17, res.Deleted)

	var left int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_ops WHERE op_id = ANY($1)`, fresh).Scan(&left))
	assert.Equal(t, len(fresh), left)
}

func TestPruneSkippedWhileAnotherInstanceHoldsLock(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()

	holder, err := pool.Acquire(ctx)
	require.NoError(t, err)
	defer holder.Release()
	var locked bool
	require.NoError(t, holder.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, testEventsLockKey).Scan(&locked))
	require.True(t, locked)

	res, err := store.PruneEvents(ctx, time.Now(), 0, 10)
	require.NoError(t, err)
	assert.True(t, res.Skipped)
	assert.Zero(t, res.Deleted)

	res, err = store.PruneProcessedOps(ctx, time.Now(), 10)
	require.NoError(t, err)
	assert.False(t, res.Skipped, "the jobs use distinct lock keys")

	var unlocked bool
	require.NoError(t, holder.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, testEventsLockKey).Scan(&unlocked))
	require.True(t, unlocked)

	res, err = store.PruneEvents(ctx, time.Now(), 0, 10)
	require.NoError(t, err)
	assert.False(t, res.Skipped)
}

func TestPruneReleasesLockAfterRun(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()

	_, err := store.PruneEvents(ctx, time.Now(), 0, 10)
	require.NoError(t, err)
	_, err = store.PruneProcessedOps(ctx, time.Now(), 10)
	require.NoError(t, err)

	for _, key := range []int64{testEventsLockKey, testOpsLockKey} {
		conn, err := pool.Acquire(ctx)
		require.NoError(t, err)
		var locked bool
		require.NoError(t, conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked))
		assert.True(t, locked)
		_, err = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key)
		require.NoError(t, err)
		conn.Release()
	}
}

func TestPruneStopsWhenContextIsCanceled(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := store.PruneEvents(ctx, time.Now(), 0, 10)
	require.Error(t, err)
}
