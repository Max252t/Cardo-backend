package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Max252t/Cardo-backend/internal/retention"
)

const (
	eventsPruneLockKey int64 = 0x63617264_72657431
	opsPruneLockKey    int64 = 0x63617264_72657432
)

var _ retention.Store = (*Store)(nil)

const pruneEventsBatchSQL = `
WITH page AS MATERIALIZED (
  SELECT board_id, seq, created_at FROM board_events
  WHERE created_at < $1 AND (created_at, board_id, seq) > ($2, $3, $4)
  ORDER BY created_at, board_id, seq
  LIMIT $5
), gone AS (
  DELETE FROM board_events e USING page p, boards b
  WHERE e.board_id = p.board_id AND e.seq = p.seq AND b.id = p.board_id AND p.seq <= b.seq - $6
  RETURNING 1
)
SELECT (SELECT count(*) FROM gone), (SELECT count(*) FROM page), l.created_at, l.board_id, l.seq
FROM (SELECT 1) one
LEFT JOIN LATERAL (
  SELECT created_at, board_id, seq FROM page ORDER BY created_at DESC, board_id DESC, seq DESC LIMIT 1
) l ON true`

const pruneProcessedOpsBatchSQL = `
WITH page AS (
  SELECT op_id FROM processed_ops WHERE created_at < $1 ORDER BY created_at LIMIT $2
)
DELETE FROM processed_ops o USING page p WHERE o.op_id = p.op_id`

type eventCursor struct {
	createdAt time.Time
	boardID   uuid.UUID
	seq       int64
}

func (s *Store) PruneEvents(ctx context.Context, olderThan time.Time, keepMin, batch int) (retention.Result, error) {
	return s.withJobLock(ctx, eventsPruneLockKey, func(ctx context.Context, q Querier) (int64, error) {
		return pruneEvents(ctx, q, olderThan, keepMin, batch)
	})
}

func (s *Store) PruneProcessedOps(ctx context.Context, olderThan time.Time, batch int) (retention.Result, error) {
	return s.withJobLock(ctx, opsPruneLockKey, func(ctx context.Context, q Querier) (int64, error) {
		return pruneProcessedOps(ctx, q, olderThan, batch)
	})
}

func pruneEvents(ctx context.Context, q Querier, olderThan time.Time, keepMin, batch int) (int64, error) {
	cursor := eventCursor{createdAt: time.Unix(0, 0).UTC()}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var deleted, scanned int64
		var lastAt *time.Time
		var lastBoard *uuid.UUID
		var lastSeq *int64
		if err := q.QueryRow(ctx, pruneEventsBatchSQL,
			olderThan, cursor.createdAt, cursor.boardID, cursor.seq, batch, keepMin,
		).Scan(&deleted, &scanned, &lastAt, &lastBoard, &lastSeq); err != nil {
			return total, fmt.Errorf("prune board events: %w", err)
		}
		total += deleted
		if scanned < int64(batch) || lastAt == nil || lastBoard == nil || lastSeq == nil {
			return total, nil
		}
		cursor = eventCursor{createdAt: *lastAt, boardID: *lastBoard, seq: *lastSeq}
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

func (s *Store) withJobLock(ctx context.Context, key int64, fn func(ctx context.Context, q Querier) (int64, error)) (retention.Result, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return retention.Result{}, fmt.Errorf("acquire retention connection: %w", err)
	}
	released := false
	defer func() {
		if !released {
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
	res := retention.Result{Deleted: deleted}

	unlockErr := unlockJob(context.WithoutCancel(ctx), conn, key)
	if unlockErr != nil {
		released = true
		discardConn(context.WithoutCancel(ctx), conn)
	}
	return res, errors.Join(runErr, unlockErr)
}

func unlockJob(ctx context.Context, conn *pgxpool.Conn, key int64) error {
	var unlocked bool
	if err := conn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&unlocked); err != nil {
		return fmt.Errorf("release retention lock: %w", err)
	}
	if !unlocked {
		return errors.New("release retention lock: lock was not held")
	}
	return nil
}

func discardConn(ctx context.Context, conn *pgxpool.Conn) {
	_ = conn.Hijack().Close(ctx)
}
