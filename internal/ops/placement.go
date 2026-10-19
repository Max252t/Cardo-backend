package ops

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/rank"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type placement struct {
	rank      string
	rebalance []storage.RankedItem
	rerank    []storage.RankedItem
}

func anchor(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func indexOf(items []storage.RankedItem, id uuid.UUID) int {
	if id == uuid.Nil {
		return -1
	}
	for i, it := range items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

func insertPosition(others []storage.RankedItem, after, before uuid.UUID) int {
	if i := indexOf(others, after); i >= 0 {
		return i + 1
	}
	if i := indexOf(others, before); i >= 0 {
		return i
	}
	return len(others)
}

func place(current []storage.RankedItem, self uuid.UUID, after, before *uuid.UUID) (placement, error) {
	others := make([]storage.RankedItem, 0, len(current))
	for _, it := range current {
		if it.ID != self {
			others = append(others, it)
		}
	}
	pos := insertPosition(others, anchor(after), anchor(before))
	lower, upper := "", ""
	if pos > 0 {
		lower = others[pos-1].Rank
	}
	if pos < len(others) {
		upper = others[pos].Rank
	}
	if r, err := rank.Between(lower, upper); err == nil && len(r) <= rank.MaxLength {
		return placement{rank: r}, nil
	}
	return rebalance(current, others, self, pos)
}

func rebalance(current, others []storage.RankedItem, self uuid.UUID, pos int) (placement, error) {
	keys, err := rank.Evenly(len(others) + 1)
	if err != nil {
		return placement{}, fmt.Errorf("rebalance ranks: %w", err)
	}
	ordered := make([]storage.RankedItem, 0, len(others)+1)
	ordered = append(ordered, others[:pos]...)
	ordered = append(ordered, storage.RankedItem{ID: self})
	ordered = append(ordered, others[pos:]...)
	inCurrent := make(map[uuid.UUID]bool, len(current))
	for _, it := range current {
		inCurrent[it.ID] = true
	}
	p := placement{rebalance: make([]storage.RankedItem, len(ordered))}
	for i, it := range ordered {
		it.Rank = keys[i]
		p.rebalance[i] = it
		if it.ID == self {
			p.rank = it.Rank
		}
		if inCurrent[it.ID] {
			p.rerank = append(p.rerank, it)
		}
	}
	return p, nil
}
