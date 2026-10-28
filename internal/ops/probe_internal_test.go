package ops

import (
	"context"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/rank"
)

type memoryRanks struct {
	sorted []domain.RankedItem
}

func memoryProbe(list []domain.RankedItem) *memoryRanks {
	sorted := slices.Clone(list)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Rank < sorted[j].Rank })
	return &memoryRanks{sorted: sorted}
}

func (m *memoryRanks) window(_ context.Context, anchor uuid.UUID, dir boardtx.RankDirection, exclude uuid.UUID) (string, string, bool, error) {
	for i, it := range m.sorted {
		if it.ID != anchor {
			continue
		}
		if dir == boardtx.RankAfter {
			for _, n := range m.sorted[i+1:] {
				if n.ID != exclude {
					return it.Rank, n.Rank, true, nil
				}
			}
		} else {
			for j := i - 1; j >= 0; j-- {
				if m.sorted[j].ID != exclude {
					return it.Rank, m.sorted[j].Rank, true, nil
				}
			}
		}
		return it.Rank, "", true, nil
	}
	return "", "", false, nil
}

func (m *memoryRanks) last(_ context.Context, exclude uuid.UUID) (string, bool, error) {
	for i := len(m.sorted) - 1; i >= 0; i-- {
		if m.sorted[i].ID != exclude {
			return m.sorted[i].Rank, true, nil
		}
	}
	return "", false, nil
}

func spacedItems(t *testing.T, n int) []domain.RankedItem {
	t.Helper()
	keys, err := rank.Evenly(n)
	require.NoError(t, err)
	out := make([]domain.RankedItem, n)
	for i, k := range keys {
		out[i] = domain.RankedItem{ID: uuid.New(), Rank: k}
	}
	return out
}

func TestProbePlacementMatchesFullListPlacement(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(7))
	ctx := context.Background()
	for round := range 400 {
		list := spacedItems(t, rng.Intn(9))
		self := uuid.New()
		if len(list) > 0 && rng.Intn(2) == 0 {
			self = list[rng.Intn(len(list))].ID
		}
		pick := func() *uuid.UUID {
			switch rng.Intn(4) {
			case 0:
				return nil
			case 1:
				id := uuid.New()
				return &id
			}
			if len(list) == 0 {
				return nil
			}
			id := list[rng.Intn(len(list))].ID
			if id == self {
				return nil
			}
			return &id
		}
		after, before := pick(), pick()

		want, err := place(list, self, after, before)
		require.NoError(t, err)
		got, ok, err := probePlace(ctx, memoryProbe(list), self, after, before)
		require.NoError(t, err)
		require.True(t, ok, "round %d", round)
		assert.Equal(t, want.rank, got, "round %d", round)
	}
}

func countingScope(list []domain.RankedItem, loads, reranks *int) rankScope {
	return rankScope{
		probe: memoryProbe(list),
		load: func(context.Context) ([]domain.RankedItem, error) {
			*loads++
			return list, nil
		},
		rerank: func(context.Context, []domain.RankedItem) error {
			*reranks++
			return nil
		},
		announce: func(context.Context, placement) error { return nil },
	}
}

func TestPlaceInWithRoomNeverLoadsTheWholeList(t *testing.T) {
	t.Parallel()
	list := spacedItems(t, 50)
	var loads, reranks int
	scope := countingScope(list, &loads, &reranks)

	for _, tc := range []struct{ after, before *uuid.UUID }{
		{nil, nil},
		{&list[10].ID, nil},
		{nil, &list[20].ID},
		{&list[0].ID, &list[1].ID},
	} {
		pl, err := placeIn(context.Background(), scope, uuid.New(), tc.after, tc.before)
		require.NoError(t, err)
		assert.NotEmpty(t, pl.rank)
		assert.Nil(t, pl.rebalance)
	}
	assert.Zero(t, loads)
	assert.Zero(t, reranks)
}

func TestPlaceInFallsBackToFullListWhenTheWindowHasNoRoom(t *testing.T) {
	t.Parallel()
	lower := "a0" + strings.Repeat("V", rank.MaxLength-2)
	upper := lower[:len(lower)-1] + "W"
	list := []domain.RankedItem{{ID: uuid.New(), Rank: lower}, {ID: uuid.New(), Rank: upper}}
	var loads, reranks int
	scope := countingScope(list, &loads, &reranks)

	pl, err := placeIn(context.Background(), scope, uuid.New(), &list[0].ID, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, loads)
	assert.Equal(t, 1, reranks)
	require.Len(t, pl.rebalance, 2)
	assert.Less(t, pl.rebalance[0].Rank, pl.rank)
	assert.Less(t, pl.rank, pl.rebalance[1].Rank)
}

func TestPlaceInRejectsSelfAnchorsBeforeTouchingStorage(t *testing.T) {
	t.Parallel()
	self := uuid.New()
	scope := rankScope{
		probe: memoryProbe(nil),
		load:  func(context.Context) ([]domain.RankedItem, error) { t.Fatal("must not load"); return nil, nil },
	}
	_, err := placeIn(context.Background(), scope, self, &self, nil)
	require.Error(t, err)
	_, err = placeIn(context.Background(), scope, self, nil, &self)
	require.Error(t, err)
}
