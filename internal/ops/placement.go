package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
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

func checkAnchors(self uuid.UUID, after, before *uuid.UUID) error {
	if anchor(after) == self {
		return invalid("after_id: must not reference the item itself")
	}
	if anchor(before) == self {
		return invalid("before_id: must not reference the item itself")
	}
	return nil
}

func place(current []domain.RankedItem, self uuid.UUID, after, before *uuid.UUID) (placement, error) {
	if err := checkAnchors(self, after, before); err != nil {
		return placement{}, err
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

type rankProbe interface {
	window(ctx context.Context, anchor uuid.UUID, dir boardtx.RankDirection, exclude uuid.UUID) (anchorRank, neighbour string, found bool, err error)
	last(ctx context.Context, exclude uuid.UUID) (string, bool, error)
}

func probeBounds(ctx context.Context, p rankProbe, self uuid.UUID, after, before *uuid.UUID) (string, string, error) {
	if id := anchor(after); id != uuid.Nil {
		anchorRank, next, ok, err := p.window(ctx, id, boardtx.RankAfter, self)
		if err != nil {
			return "", "", err
		}
		if ok {
			return anchorRank, next, nil
		}
	}
	if id := anchor(before); id != uuid.Nil {
		anchorRank, prev, ok, err := p.window(ctx, id, boardtx.RankBefore, self)
		if err != nil {
			return "", "", err
		}
		if ok {
			return prev, anchorRank, nil
		}
	}
	tail, _, err := p.last(ctx, self)
	return tail, "", err
}

func probePlace(ctx context.Context, p rankProbe, self uuid.UUID, after, before *uuid.UUID) (string, bool, error) {
	lower, upper, err := probeBounds(ctx, p, self, after, before)
	if err != nil {
		return "", false, err
	}
	r, err := rank.Between(lower, upper)
	if err != nil || len(r) > rank.MaxLength {
		return "", false, nil
	}
	return r, true, nil
}

type rankScope struct {
	probe    rankProbe
	load     func(context.Context) ([]domain.RankedItem, error)
	rerank   func(context.Context, []domain.RankedItem) error
	announce func(context.Context, placement) error
}

func placeIn(ctx context.Context, scope rankScope, self uuid.UUID, after, before *uuid.UUID) (placement, error) {
	if err := checkAnchors(self, after, before); err != nil {
		return placement{}, err
	}
	if scope.probe != nil {
		r, ok, err := probePlace(ctx, scope.probe, self, after, before)
		if err != nil {
			return placement{}, err
		}
		if ok {
			return placement{rank: r}, nil
		}
	}
	return placeWithRebalance(ctx, scope, self, after, before)
}

func placeWithRebalance(ctx context.Context, scope rankScope, self uuid.UUID, after, before *uuid.UUID) (placement, error) {
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
