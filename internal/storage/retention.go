package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Max252t/Cardo-backend/internal/retention"
)

const (
	eventsPruneLockKey int64 = 0x63617264_72657431
	opsPruneLockKey    int64 = 0x63617264_72657432

	pruneBoardPage = 200
	unlockTimeout  = 5 * time.Second
)

var _ retention.Store = (*Store)(nil)

const pruneBoardsPageSQL = `
SELECT id, seq, deleted_at IS NOT NULL FROM boards
WHERE id > $1 AND (seq > $2 OR (deleted_at IS NOT NULL AND seq > 0))
ORDER BY id
LIMIT $3`

const newestExpiredSeqSQL = `
SELECT seq FROM board_events
WHERE board_id = $1 AND created_at < $2
ORDER BY created_at DESC
LIMIT 1`

const pruneBoardEventsBatchSQL = `
DELETE FROM board_events
WHERE board_id = $1 AND seq IN (
  SELECT seq FROM board_events WHERE board_id = $1 AND seq <= $2 ORDER BY seq LIMIT $3
)`

const pruneProcessedOpsBatchSQL = `
WITH page AS (
  SELECT op_id FROM processed_ops WHERE created_at < $1 ORDER BY created_at LIMIT $2
)
DELETE FROM processed_ops o USING page p WHERE o.op_id = p.op_id`

type boardRef struct {
	id      uuid.UUID
	seq     int64
	deleted bool
}

func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time, keepMin, batch int) (retention.Result, error) {
	return s.tryRunLocked(ctx, eventsPruneLockKey, func(ctx context.Context, q Querier) (int64, error) {
		return pruneEvents(ctx, q, olderThan, keepMin, batch)
	})
}

func (s *Store) PruneProcessedOps(ctx context.Context, olderThan time.Time, batch int) (retention.Result, error) {
	return s.tryRunLocked(ctx, opsPruneLockKey, func(ctx context.Context, q Querier) (int64, error) {
		return pruneProcessedOps(ctx, q, olderThan, batch)
	})
}

func pruneEvents(ctx context.Context, q Querier, olderThan time.Time, keepMin, batch int) (int64, error) {
	var total int64
	cursor := uuid.Nil
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		boards, err := boardsPage(ctx, q, cursor, keepMin)
		if err != nil {
			return total, err
		}
		for _, b := range boards {
			n, err := pruneBoardEvents(ctx, q, b, olderThan, keepMin, batch)
			total += n
			if err != nil {
				return total, err
			}
		}
		if len(boards) < pruneBoardPage {
			return total, nil
		}
		cursor = boards[len(boards)-1].id
	}
}

func boardsPage(ctx context.Context, q Querier, after uuid.UUID, keepMin int) ([]boardRef, error) {
	rows, err := q.Query(ctx, pruneBoardsPageSQL, after, int64(keepMin), pruneBoardPage)
	if err != nil {
		return nil, fmt.Errorf("list boards for pruning: %w", err)
	}
	defer rows.Close()
	var out []boardRef
	for rows.Next() {
		var b boardRef
		if err := rows.Scan(&b.id, &b.seq, &b.deleted); err != nil {
			return nil, fmt.Errorf("scan board for pruning: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list boards for pruning: %w", err)
	}
	return out, nil
}

func pruneBoardEvents(ctx context.Context, q Querier, b boardRef, olderThan time.Time, keepMin, batch int) (int64, error) {
	limit := b.seq - int64(keepMin)
	if b.deleted {
		limit = b.seq
	}
	if limit < 1 {
		return 0, nil
	}
	var expired *int64
	if err := q.QueryRow(ctx, newestExpiredSeqSQL, b.id, olderThan).Scan(&expired); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("find expired board events: %w", err)
	}
	if expired == nil {
		return 0, nil
	}
	limit = min(limit, *expired)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		tag, err := q.Exec(ctx, pruneBoardEventsBatchSQL, b.id, limit, batch)
		if err != nil {
			return total, fmt.Errorf("prune board events: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}

func pruneProcessedOps(ctx context.Context, q Querier, olderThan time.Time, batch int) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		tag, err := q.Exec(ctx, pruneProcessedOpsBatchSQL, olderThan, batch)
		if err != nil {
			return total, fmt.Errorf("prune processed ops: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < int64(batch) {
			return total, nil
		}
	}
}

func (s *Store) tryRunLocked(ctx context.Context, key int64, fn func(ctx context.Context, q Querier) (int64, error)) (res retention.Result, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return retention.Result{}, fmt.Errorf("acquire retention connection: %w", err)
	}
	cleanup := true
	defer func() {
		if r := recover(); r != nil {
			discardConn(conn)
			panic(r)
		}
		if cleanup {
			conn.Release()
		}
	}()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		return retention.Result{}, fmt.Errorf("try retention lock: %w", err)
	}
	if !locked {
		return retention.Result{Skipped: true}, nil
	}

	deleted, runErr := fn(ctx, conn)
	res = retention.Result{Deleted: deleted}

	unlockErr := unlockJob(ctx, conn, key)
	if unlockErr != nil {
		cleanup = false
		discardConn(conn)
	}
	return res, errors.Join(runErr, unlockErr)
}

func unlockJob(ctx context.Context, conn *pgxpool.Conn, key int64) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unlockTimeout)
	defer cancel()
	var unlocked bool
	if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked); err != nil {
		return fmt.Errorf("release retention lock: %w", err)
	}
	if !unlocked {
		return errors.New("release retention lock: lock was not held")
	}
	return nil
}

func discardConn(conn *pgxpool.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
	defer cancel()
	_ = conn.Hijack().Close(ctx)
}
