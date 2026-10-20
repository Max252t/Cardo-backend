package storage

import (
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RankedItem struct {
	ID   uuid.UUID
	Rank string
}

func scanRankedItem(row pgx.Row) (RankedItem, error) {
	var it RankedItem
	err := row.Scan(&it.ID, &it.Rank)
	return it, err
}

func splitRanked(items []RankedItem) ([]uuid.UUID, []string) {
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
