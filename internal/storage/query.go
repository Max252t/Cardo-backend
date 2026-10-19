package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const activeByIDInBoard = "id = $1 AND board_id = $2 AND archived_at IS NULL"

func queryAll[T any](ctx context.Context, q Querier, scan func(pgx.Row) (T, error), sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (T, error) {
		return scan(row)
	})
}

func scanUUID(row pgx.Row) (uuid.UUID, error) {
	var id uuid.UUID
	err := row.Scan(&id)
	return id, err
}
