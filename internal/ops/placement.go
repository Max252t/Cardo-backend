package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/rank"
)

type placement struct {
	rank      string
	rebalance []domain.RankedItem
}

func anchor(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func indexOf(items []domain.RankedItem, id uuid.UUID) int {
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

func insertPosition(others []domain.RankedItem, after, before uuid.UUID) int {
	if i := indexOf(others, after); i >= 0 {
		return i + 1
	}
	if i := indexOf(others, before); i >= 0 {
		return i
	}
	return len(others)
}

func place(current []domain.RankedItem, self uuid.UUID, after, before *uuid.UUID) (placement, error) {
	if anchor(after) == self {
		return placement{}, invalid("after_id: must not reference the item itself")
	}
	if anchor(before) == self {
		return placement{}, invalid("before_id: must not reference the item itself")
	}
	if r, ok := rankAt(current, self, after, before); ok {
		return placement{rank: r}, nil
	}
	keys, err := rank.Evenly(len(current))
	if err != nil {
		return placement{}, fmt.Errorf("rebalance ranks: %w", err)
	}
	respaced := make([]domain.RankedItem, len(current))
	for i, it := range current {
		respaced[i] = domain.RankedItem{ID: it.ID, Rank: keys[i]}
	}
	r, ok := rankAt(respaced, self, after, before)
	if !ok {
		return placement{}, errors.New("rebalance ranks: no rank available after respacing")
	}
	return placement{rank: r, rebalance: respaced}, nil
}

func rankAt(current []domain.RankedItem, self uuid.UUID, after, before *uuid.UUID) (string, bool) {
	others := make([]domain.RankedItem, 0, len(current))
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
	r, err := rank.Between(lower, upper)
	if err != nil || len(r) > rank.MaxLength {
		return "", false
	}
	return r, true
}

type rankScope struct {
	load     func(context.Context) ([]domain.RankedItem, error)
	rerank   func(context.Context, []domain.RankedItem) error
	announce func(context.Context, placement) error
}

func placeIn(ctx context.Context, scope rankScope, self uuid.UUID, after, before *uuid.UUID) (placement, error) {
	current, err := scope.load(ctx)
	if err != nil {
		return placement{}, err
	}
	pl, err := place(current, self, after, before)
	if err != nil {
		return placement{}, err
	}
	if pl.rebalance == nil {
		return pl, nil
	}
	if err := scope.rerank(ctx, pl.rebalance); err != nil {
		return placement{}, err
	}
	if err := scope.announce(ctx, pl); err != nil {
		return placement{}, err
	}
	return pl, nil
}
