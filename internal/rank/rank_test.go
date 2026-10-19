package rank_test

import (
	"math/rand/v2"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/rank"
)

func TestBetweenVectors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b, want string
	}{
		{"", "", "a0"},
		{"", "a0", "Zz"},
		{"", "Zz", "Zy"},
		{"a0", "", "a1"},
		{"a1", "", "a2"},
		{"a0", "a1", "a0V"},
		{"a1", "a2", "a1V"},
		{"a0V", "a1", "a0l"},
		{"Zz", "a0", "ZzV"},
		{"Zz", "a1", "a0"},
		{"", "Y00", "Xzzz"},
		{"bzz", "", "c000"},
		{"a0", "a0V", "a0G"},
		{"a0", "a0G", "a08"},
		{"b125", "b129", "b127"},
		{"a0", "a1V", "a1"},
		{"Zz", "a01", "a0"},
		{"", "a0V", "a0"},
		{"", "b999", "b99"},
		{"", "A000000000000000000000000001", "A000000000000000000000000000V"},
		{"zzzzzzzzzzzzzzzzzzzzzzzzzzy", "", "zzzzzzzzzzzzzzzzzzzzzzzzzzz"},
		{"zzzzzzzzzzzzzzzzzzzzzzzzzzz", "", "zzzzzzzzzzzzzzzzzzzzzzzzzzzV"},
	}
	for _, tt := range tests {
		got, err := rank.Between(tt.a, tt.b)
		require.NoError(t, err, "Between(%q, %q)", tt.a, tt.b)
		assert.Equal(t, tt.want, got, "Between(%q, %q)", tt.a, tt.b)
	}
}

func TestBetweenErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		err  error
	}{
		{"", "A00000000000000000000000000", rank.ErrInvalidKey},
		{"a00", "", rank.ErrInvalidKey},
		{"a00", "a1", rank.ErrInvalidKey},
		{"0", "1", rank.ErrInvalidKey},
		{"a1", "a0", rank.ErrOrder},
		{"a1", "a1", rank.ErrOrder},
		{"a", "", rank.ErrInvalidKey},
		{"a0!", "", rank.ErrInvalidKey},
	}
	for _, tt := range tests {
		_, err := rank.Between(tt.a, tt.b)
		assert.ErrorIs(t, err, tt.err, "Between(%q, %q)", tt.a, tt.b)
	}
}

func TestBetweenIsStrictlyOrderedForRandomPairs(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(1, 2))
	keys := []string{"a0"}
	for range 2000 {
		i := rng.IntN(len(keys) + 1)
		lo, hi := "", ""
		if i > 0 {
			lo = keys[i-1]
		}
		if i < len(keys) {
			hi = keys[i]
		}
		k, err := rank.Between(lo, hi)
		require.NoError(t, err)
		require.NoError(t, rank.Validate(k))
		if lo != "" {
			require.Less(t, lo, k)
		}
		if hi != "" {
			require.Less(t, k, hi)
		}
		keys = append(keys[:i], append([]string{k}, keys[i:]...)...)
	}
	assert.True(t, sort.StringsAreSorted(keys))
}

func TestAppendingKeepsKeysShort(t *testing.T) {
	t.Parallel()
	k := ""
	for range 10000 {
		next, err := rank.Between(k, "")
		require.NoError(t, err)
		k = next
	}
	assert.LessOrEqual(t, len(k), 4)
}

func TestRepeatedInsertAtSamePointGrows(t *testing.T) {
	t.Parallel()
	lo, hi := "a0", "a1"
	for range 400 {
		k, err := rank.Between(lo, hi)
		require.NoError(t, err)
		hi = k
	}
	assert.Greater(t, len(hi), rank.MaxLength, "repeated inserts at one point eventually need a rebalance")
}

func TestNBetween(t *testing.T) {
	t.Parallel()
	cases := []struct{ a, b string }{{"", ""}, {"a0", ""}, {"", "a0"}, {"a0", "a1"}, {"Zz", "b00"}}
	for _, c := range cases {
		for _, n := range []int{0, 1, 2, 5, 100} {
			keys, err := rank.NBetween(c.a, c.b, n)
			require.NoError(t, err)
			require.Len(t, keys, n)
			all := append([]string{}, keys...)
			if c.a != "" {
				all = append([]string{c.a}, all...)
			}
			if c.b != "" {
				all = append(all, c.b)
			}
			for i := 1; i < len(all); i++ {
				assert.Less(t, all[i-1], all[i], "n=%d a=%q b=%q", n, c.a, c.b)
			}
		}
	}
}

func TestEvenly(t *testing.T) {
	t.Parallel()
	keys, err := rank.Evenly(5)
	require.NoError(t, err)
	assert.Equal(t, []string{"a0", "a1", "a2", "a3", "a4"}, keys)

	keys, err = rank.Evenly(5000)
	require.NoError(t, err)
	assert.True(t, sort.StringsAreSorted(keys))
	for _, k := range keys {
		assert.LessOrEqual(t, len(k), 4)
	}
	empty, err := rank.Evenly(0)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestDigitsAreAsciiOrdered(t *testing.T) {
	t.Parallel()
	assert.Len(t, rank.Digits, 62)
	assert.True(t, sort.SliceIsSorted(strings.Split(rank.Digits, ""), func(i, j int) bool {
		return rank.Digits[i] < rank.Digits[j]
	}))
}
