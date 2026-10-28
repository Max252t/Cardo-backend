package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func scanRankedItem(row pgx.Row) (domain.RankedItem, error) {
	var it domain.RankedItem
	err := row.Scan(&it.ID, &it.Rank)
	return it, err
}

func (b *BoardTx) queryRank(ctx context.Context, sql string, args ...any) (string, bool, error) {
	var rank string
	err := b.tx.QueryRow(ctx, sql, args...).Scan(&rank)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return rank, true, nil
}

func windowSQL(tmpl string, dir boardtx.RankDirection) string {
	if dir == boardtx.RankBefore {
		return fmt.Sprintf(tmpl, "<", "DESC")
	}
	return fmt.Sprintf(tmpl, ">", "ASC")
}

func (b *BoardTx) queryWindow(ctx context.Context, sql string, args ...any) (string, string, bool, error) {
	var anchor, neighbour string
	err := b.tx.QueryRow(ctx, sql, args...).Scan(&anchor, &neighbour)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return anchor, neighbour, true, nil
}

func splitRanked(items []domain.RankedItem) ([]uuid.UUID, []string) {
	ids := make([]uuid.UUID, len(items))
	ranks := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
		ranks[i] = it.Rank
	}
	return ids, ranks
}

func checkReranked(table string, affected int64, want int, err error) error {
	if err != nil {
		return fmt.Errorf("rerank %s: %w", table, err)
	}
	if affected != int64(want) {
		return fmt.Errorf("rerank %s: updated %d of %d rows", table, affected, want)
	}
	return nil
}
