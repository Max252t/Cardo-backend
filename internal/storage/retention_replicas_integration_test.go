package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/retention"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func secondReplica(t *testing.T, pool *pgxpool.Pool, opts ...storage.Option) *storage.Store {
	t.Helper()
	p, err := storage.NewPool(context.Background(), pool.Config().ConnString(), 10)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return storage.New(p, opts...)
}

func lockedByAnother(t *testing.T, pool *pgxpool.Pool, key int64) bool {
	t.Helper()
	var held bool
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT EXISTS (
		   SELECT 1 FROM pg_locks
		   WHERE locktype = 'advisory' AND granted AND objsubid = 1
		     AND ((classid::bigint << 32) | objid::bigint) = $1
		 )`, key).Scan(&held))
	return held
}

func holdEventRow(t *testing.T, pool *pgxpool.Pool, boardID uuid.UUID, seq int64) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	var got int64
	require.NoError(t, tx.QueryRow(ctx, `SELECT seq FROM board_events WHERE board_id = $1 AND seq = $2 FOR UPDATE`, boardID, seq).Scan(&got))
	return tx
}

func seedBulkEvents(t *testing.T, pool *pgxpool.Pool, boardID uuid.UUID, n int, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx,
		`INSERT INTO board_events (board_id, seq, type, payload, created_at)
		 SELECT $1, g, 't.bulk', '{}'::jsonb, now() - make_interval(secs => $3) + g * interval '1 millisecond'
		 FROM generate_series(1, $2::int) g`, boardID, n, age.Seconds())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE boards SET seq = $2 WHERE id = $1`, boardID, n)
	require.NoError(t, err)
}

func TestPruneEventsOnlyOneReplicaRunsAtATime(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	replicaA := storage.New(pool)
	replicaB := secondReplica(t, pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, replicaA, testutil.UniqueEmail("rep"))
	b := createBoard(t, replicaA, owner.ID, "b")
	appendEvents(t, replicaA, b.ID, 10)
	ageEvents(t, pool, b.ID, 10, 10*24*time.Hour)

	blocker := holdEventRow(t, pool, b.ID, 3)

	type outcome struct {
		res retention.Result
		err error
	}
	first := make(chan outcome, 1)
	go func() {
		res, err := replicaA.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 0, 100)
		first <- outcome{res, err}
	}()

	require.Eventually(t, func() bool { return lockedByAnother(t, pool, testEventsLockKey) }, 10*time.Second, 20*time.Millisecond)

	res, err := replicaB.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 0, 100)
	require.NoError(t, err)
	assert.True(t, res.Skipped)
	assert.Zero(t, res.Deleted)

	res, err = replicaB.PruneProcessedOps(ctx, time.Now(), 10)
	require.NoError(t, err)
	assert.False(t, res.Skipped, "the other job is independent")

	require.NoError(t, blocker.Rollback(ctx))
	select {
	case got := <-first:
		require.NoError(t, got.err)
		assert.False(t, got.res.Skipped)
		assert.EqualValues(t, 10, got.res.Deleted)
	case <-time.After(15 * time.Second):
		t.Fatal("the first replica never finished")
	}

	assert.False(t, lockedByAnother(t, pool, testEventsLockKey), "the lock is released after the run")
	res, err = replicaB.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 0, 100)
	require.NoError(t, err)
	assert.False(t, res.Skipped)
	assert.Zero(t, res.Deleted)
}

func TestPruneReleasesLockWhenContextIsCanceledMidBatch(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	other := secondReplica(t, pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("cancel"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 6)
	ageEvents(t, pool, b.ID, 6, 10*24*time.Hour)
	blocker := holdEventRow(t, pool, b.ID, 2)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := store.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 0, 100)
		done <- err
	}()
	require.Eventually(t, func() bool { return lockedByAnother(t, pool, testEventsLockKey) }, 10*time.Second, 20*time.Millisecond)

	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("prune did not stop after cancellation")
	}
	require.NoError(t, blocker.Rollback(context.Background()))

	require.Eventually(t, func() bool { return !lockedByAnother(t, pool, testEventsLockKey) }, 10*time.Second, 50*time.Millisecond)
	res, err := other.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 0, 100)
	require.NoError(t, err)
	assert.False(t, res.Skipped)
	assert.Empty(t, retainedSeqs(t, pool, b.ID))
}

func TestPruneReleasesLockWhenTheJobFails(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("fail"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 3)
	ageEvents(t, pool, b.ID, 3, 9*24*time.Hour)
	_, err := pool.Exec(ctx, `ALTER TABLE board_events RENAME TO board_events_gone`)
	require.NoError(t, err)

	res, err := store.PruneEvents(ctx, time.Now(), 0, 10)
	require.Error(t, err)
	assert.Zero(t, res.Deleted)
	assert.False(t, lockedByAnother(t, pool, testEventsLockKey))

	_, err = pool.Exec(ctx, `ALTER TABLE processed_ops RENAME TO processed_ops_gone`)
	require.NoError(t, err)
	_, err = store.PruneProcessedOps(ctx, time.Now(), 10)
	require.Error(t, err)
	assert.False(t, lockedByAnother(t, pool, testOpsLockKey))
}

func TestPruneDoesNotBlockWritersOnTheBoardMidBatch(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool, storage.WithLockTimeout(2*time.Second))
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("nolock"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 8)
	ageEvents(t, pool, b.ID, 8, 10*24*time.Hour)
	blocker := holdEventRow(t, pool, b.ID, 4)

	done := make(chan error, 1)
	go func() {
		_, err := store.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 2, 100)
		done <- err
	}()
	require.Eventually(t, func() bool { return lockedByAnother(t, pool, testEventsLockKey) }, 10*time.Second, 20*time.Millisecond)

	start := time.Now()
	appendEvents(t, store, b.ID, 3)
	assert.Less(t, time.Since(start), 2*time.Second)
	seq, _ := boardSeq(t, pool, b.ID)
	assert.EqualValues(t, 11, seq)

	require.NoError(t, blocker.Rollback(ctx))
	require.NoError(t, <-done)
	retained := retainedSeqs(t, pool, b.ID)
	assert.Contains(t, retained, int64(11))
	assert.Contains(t, retained, int64(10))
}

func TestPruneEventsBigDatasetDoesNotStarveBoardLocks(t *testing.T) {
	if testing.Short() {
		t.Skip("big dataset")
	}
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool, storage.WithLockTimeout(3*time.Second))
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("big"))
	bulk := createBoard(t, store, owner.ID, "bulk")
	live := createBoard(t, store, owner.ID, "live")
	seedBulkEvents(t, pool, bulk.ID, 30000, 10*24*time.Hour)

	type outcome struct {
		res retention.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := store.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 100, 50)
		done <- outcome{res, err}
	}()

	applied := 0
	var final outcome
	for finished := false; !finished; {
		select {
		case final = <-done:
			finished = true
		default:
			started := time.Now()
			err := store.WithBoardLock(ctx, bulk.ID, func(ctx context.Context, btx boardtx.Tx) error {
				_, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "t.live"})
				return err
			})
			require.NoError(t, err)
			assert.Less(t, time.Since(started), 3*time.Second)
			applied++
			appendEvents(t, store, live.ID, 1)
		}
	}
	require.NoError(t, final.err)
	assert.False(t, final.res.Skipped)
	var live2, bulkLeft int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE seq > 30000), count(*) FILTER (WHERE seq <= 30000) FROM board_events WHERE board_id = $1`, bulk.ID).Scan(&live2, &bulkLeft))
	assert.Equal(t, applied, live2, "events inside the window are never pruned")
	assert.LessOrEqual(t, bulkLeft, 100)
	assert.EqualValues(t, 30000-bulkLeft, final.res.Deleted)
}

func TestPruneEventsMultipleBoardsWithDifferentSeqCounts(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("multi"))

	sizes := []int{1, 4, 5, 6, 12, 33}
	boards := make([]uuid.UUID, len(sizes))
	for i, n := range sizes {
		boards[i] = createBoard(t, store, owner.ID, "b").ID
		appendEvents(t, store, boards[i], n)
		ageEvents(t, pool, boards[i], int64(n), 9*24*time.Hour)
	}

	res, err := store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 5, 4)
	require.NoError(t, err)

	var wantDeleted int64
	for i, n := range sizes {
		keep := min(n, 5)
		wantDeleted += int64(n - keep)
		assert.Equal(t, seqSpan(int64(n-keep+1), int64(n)), retainedSeqs(t, pool, boards[i]), "board with %d events", n)
	}
	assert.Equal(t, wantDeleted, res.Deleted)
}

func TestPruneEventsKeepsExactlyKeepMinBoundary(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("edge"))
	exact := createBoard(t, store, owner.ID, "exact")
	plusOne := createBoard(t, store, owner.ID, "plus")
	appendEvents(t, store, exact.ID, 10)
	appendEvents(t, store, plusOne.ID, 11)
	ageEvents(t, pool, exact.ID, 10, 9*24*time.Hour)
	ageEvents(t, pool, plusOne.ID, 11, 9*24*time.Hour)

	res, err := store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 10, 3)
	require.NoError(t, err)
	assert.EqualValues(t, 1, res.Deleted)
	assert.Equal(t, seqSpan(1, 10), retainedSeqs(t, pool, exact.ID))
	assert.Equal(t, seqSpan(2, 11), retainedSeqs(t, pool, plusOne.ID))
}

func TestPruneEventsHandlesSoftDeletedBoards(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("soft"))
	b := createBoard(t, store, owner.ID, "gone")
	appendEvents(t, store, b.ID, 9)
	ageEvents(t, pool, b.ID, 9, 9*24*time.Hour)
	_, err := pool.Exec(ctx, `UPDATE boards SET deleted_at = now() WHERE id = $1`, b.ID)
	require.NoError(t, err)

	res, err := store.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 4, 2)
	require.NoError(t, err)
	assert.EqualValues(t, 9, res.Deleted, "a deleted board ignores the keep minimum")
	assert.Empty(t, retainedSeqs(t, pool, b.ID))
}

func TestPruneEventsCutoffBoundaryIsExclusive(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("cut"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 3)
	cutoff := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	for seq, at := range map[int64]time.Time{1: cutoff.Add(-time.Microsecond), 2: cutoff, 3: cutoff.Add(time.Microsecond)} {
		_, err := pool.Exec(ctx, `UPDATE board_events SET created_at = $3 WHERE board_id = $1 AND seq = $2`, b.ID, seq, at)
		require.NoError(t, err)
	}

	res, err := store.PruneEvents(ctx, cutoff, 0, 10)
	require.NoError(t, err)
	assert.EqualValues(t, 1, res.Deleted)
	assert.Equal(t, seqSpan(2, 3), retainedSeqs(t, pool, b.ID))
}

func TestPruneProcessedOpsCutoffBoundaryIsExclusive(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("opcut"))
	b := createBoard(t, store, owner.ID, "b")
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	err := store.WithBoardLock(ctx, b.ID, func(ctx context.Context, btx boardtx.Tx) error {
		for _, id := range ids {
			if _, err := btx.SaveProcessedOp(ctx, id, owner.ID, map[string]any{"v": 1}); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	cutoff := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	for i, at := range []time.Time{cutoff.Add(-time.Microsecond), cutoff, cutoff.Add(time.Microsecond)} {
		_, err := pool.Exec(ctx, `UPDATE processed_ops SET created_at = $2 WHERE op_id = $1`, ids[i], at)
		require.NoError(t, err)
	}

	res, err := store.PruneProcessedOps(ctx, cutoff, 5)
	require.NoError(t, err)
	assert.EqualValues(t, 1, res.Deleted)

	var left []uuid.UUID
	rows, err := pool.Query(ctx, `SELECT op_id FROM processed_ops ORDER BY created_at`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		left = append(left, id)
	}
	assert.Equal(t, ids[1:], left)

	res, err = store.PruneProcessedOps(ctx, cutoff, 5)
	require.NoError(t, err)
	assert.Zero(t, res.Deleted)
}

func TestPruneProcessedOpsExactMultipleOfBatchSize(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("opbatch"))
	b := createBoard(t, store, owner.ID, "b")
	err := store.WithBoardLock(ctx, b.ID, func(ctx context.Context, btx boardtx.Tx) error {
		for range 12 {
			if _, err := btx.SaveProcessedOp(ctx, uuid.New(), owner.ID, map[string]any{"v": 1}); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE processed_ops SET created_at = now() - interval '2 days'`)
	require.NoError(t, err)

	res, err := store.PruneProcessedOps(ctx, time.Now().Add(-24*time.Hour), 4)
	require.NoError(t, err)
	assert.EqualValues(t, 12, res.Deleted)
}

func TestPruneEventsExactMultipleOfBatchSize(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("evbatch"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 12)
	ageEvents(t, pool, b.ID, 12, 9*24*time.Hour)

	res, err := store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 0, 4)
	require.NoError(t, err)
	assert.EqualValues(t, 12, res.Deleted)
	assert.Empty(t, retainedSeqs(t, pool, b.ID))
}

func TestPruneEventsKeepMinLargerThanSeqKeepsEverything(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("large"))
	b := createBoard(t, store, owner.ID, "b")
	appendEvents(t, store, b.ID, 6)
	ageEvents(t, pool, b.ID, 6, 30*24*time.Hour)

	res, err := store.PruneEvents(context.Background(), time.Now(), 1000, 2)
	require.NoError(t, err)
	assert.Zero(t, res.Deleted)
	assert.Equal(t, seqSpan(1, 6), retainedSeqs(t, pool, b.ID))
}
