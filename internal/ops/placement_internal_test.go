package ops

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/storage"
)

func items(ranks ...string) []storage.RankedItem {
	out := make([]storage.RankedItem, len(ranks))
	for i, r := range ranks {
		out[i] = storage.RankedItem{ID: uuid.New(), Rank: r}
	}
	return out
}

func TestPlaceAnchors(t *testing.T) {
	t.Parallel()
	list := items("a0", "a1", "a2")
	self := uuid.New()
	ghost := uuid.New()

	tests := []struct {
		name          string
		after, before *uuid.UUID
		lower, upper  string
	}{
		{"end when no anchors", nil, nil, "a2", ""},
		{"after anchor wins", &list[0].ID, &list[2].ID, "a0", "a1"},
		{"missing before uses after", &list[1].ID, &ghost, "a1", "a2"},
		{"missing after uses before", &ghost, &list[1].ID, "a0", "a1"},
		{"before first", nil, &list[0].ID, "", "a0"},
		{"both missing goes to end", &ghost, &ghost, "a2", ""},
		{"nil uuid anchors are ignored", &uuid.Nil, &uuid.Nil, "a2", ""},
	}
	for _, tt := range tests {
		p, err := place(list, self, tt.after, tt.before)
		require.NoError(t, err, tt.name)
		assert.Nil(t, p.rebalance, tt.name)
		if tt.lower != "" {
			assert.Less(t, tt.lower, p.rank, tt.name)
		}
		if tt.upper != "" {
			assert.Less(t, p.rank, tt.upper, tt.name)
		}
	}
}

func TestPlaceIgnoresSelfInCurrentList(t *testing.T) {
	t.Parallel()
	list := items("a0", "a1", "a2")
	p, err := place(list, list[0].ID, &list[2].ID, nil)
	require.NoError(t, err)
	assert.Greater(t, p.rank, "a2")
}

func TestPlaceRebalancesLongOrInvalidRanks(t *testing.T) {
	t.Parallel()
	long := "a0" + strings.Repeat("0", 61) + "1"
	list := items("a0", long, "zzz")
	list[2].Rank = "not-a-rank"
	self := list[1].ID
	p, err := place(list, self, &list[0].ID, nil)
	require.NoError(t, err)
	require.Len(t, p.rebalance, 3)
	assert.Equal(t, list[0].ID, p.rebalance[0].ID)
	assert.Equal(t, self, p.rebalance[1].ID)
	assert.Equal(t, p.rebalance[1].Rank, p.rank)
	assert.Len(t, p.rerank, 3, "every row already in the list is reranked")

	newcomer := uuid.New()
	p, err = place(list, newcomer, nil, nil)
	require.NoError(t, err)
	require.Len(t, p.rebalance, 4)
	assert.Equal(t, newcomer, p.rebalance[3].ID)
	assert.Len(t, p.rerank, 3, "the newcomer is inserted, not reranked")
}
