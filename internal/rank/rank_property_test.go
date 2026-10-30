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

const (
	smallestInteger = "A" + "000000000000000000000000000"
	largestInteger  = "zzzzzzzzzzzzzzzzzzzzzzzzzzz"
)

func randomKey(rng *rand.Rand) string {
	for {
		var sb strings.Builder
		var head byte
		var length int
		if rng.IntN(2) == 0 {
			n := rng.IntN(26)
			head = byte('a' + n)
			length = n + 2
		} else {
			n := rng.IntN(26)
			head = byte('Z' - n)
			length = n + 2
		}
		sb.WriteByte(head)
		for i := 1; i < length; i++ {
			sb.WriteByte(rank.Digits[rng.IntN(len(rank.Digits))])
		}
		for range rng.IntN(5) {
			sb.WriteByte(rank.Digits[rng.IntN(len(rank.Digits))])
		}
		key := sb.String()
		if rank.Validate(key) == nil {
			return key
		}
	}
}

func isSortedStrictly(keys []string) bool {
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			return false
		}
	}
	return true
}

func TestBetweenPropertyOverRandomValidKeys(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(42, 4242))
	checked := 0
	for checked < 5000 {
		a, b := randomKey(rng), randomKey(rng)
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		mid, err := rank.Between(a, b)
		if err != nil {
			require.ErrorIs(t, err, rank.ErrExhausted, "Between(%q, %q)", a, b)
			continue
		}
		require.NoError(t, rank.Validate(mid), "Between(%q, %q) = %q", a, b, mid)
		require.Less(t, a, mid, "Between(%q, %q) = %q", a, b, mid)
		require.Less(t, mid, b, "Between(%q, %q) = %q", a, b, mid)
		checked++
	}
}

func TestBetweenOpenEndsPropertyOverRandomKeys(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(7, 77))
	for range 3000 {
		k := randomKey(rng)
		before, err := rank.Between("", k)
		if err == nil {
			require.NoError(t, rank.Validate(before))
			require.Less(t, before, k, "Between(\"\", %q) = %q", k, before)
		} else {
			require.ErrorIs(t, err, rank.ErrExhausted)
		}
		after, err := rank.Between(k, "")
		if err == nil {
			require.NoError(t, rank.Validate(after))
			require.Greater(t, after, k, "Between(%q, \"\") = %q", k, after)
		} else {
			require.ErrorIs(t, err, rank.ErrExhausted)
		}
	}
}

func TestBetweenOpenEndsAtExtremes(t *testing.T) {
	t.Parallel()

	after, err := rank.Between(largestInteger, "")
	require.NoError(t, err)
	assert.Greater(t, after, largestInteger)
	require.NoError(t, rank.Validate(after))

	justAboveSmallest := smallestInteger[:len(smallestInteger)-1] + "1"
	before, err := rank.Between("", justAboveSmallest)
	require.NoError(t, err)
	assert.Less(t, before, justAboveSmallest)
	require.NoError(t, rank.Validate(before))

	k := justAboveSmallest
	for range 20 {
		next, err := rank.Between("", k)
		require.NoError(t, err)
		require.Less(t, next, k)
		require.NoError(t, rank.Validate(next))
		k = next
	}
}

func TestBetweenOnlyOpenEnds(t *testing.T) {
	t.Parallel()
	k, err := rank.Between("", "")
	require.NoError(t, err)
	assert.Equal(t, "a0", k)
	require.NoError(t, rank.Validate(k))
}

func TestBetweenIsDeterministic(t *testing.T) {
	t.Parallel()
	first, err := rank.Between("a0", "a1")
	require.NoError(t, err)
	for range 10 {
		again, err := rank.Between("a0", "a1")
		require.NoError(t, err)
		assert.Equal(t, first, again)
	}
}

func TestBetweenAdjacentKeysStillFindRoom(t *testing.T) {
	t.Parallel()
	pairs := [][2]string{{"a0", "a1"}, {"az", "b00"}, {"Zz", "a0"}, {"a0", "a01"}, {"a0V", "a0W"}, {"b0z", "b10"}}
	for _, p := range pairs {
		mid, err := rank.Between(p[0], p[1])
		require.NoError(t, err, "%v", p)
		assert.Less(t, p[0], mid)
		assert.Less(t, mid, p[1])
		require.NoError(t, rank.Validate(mid))
	}
}

func TestBetweenInvalidInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		err  error
	}{
		{"invalid lower", "!!", "a1", rank.ErrInvalidKey},
		{"invalid upper", "a0", "a1!", rank.ErrInvalidKey},
		{"empty-looking whitespace", " ", "", rank.ErrInvalidKey},
		{"unicode digit", "a٠", "", rank.ErrInvalidKey},
		{"integer part too short", "c0", "", rank.ErrInvalidKey},
		{"smallest integer as lower", smallestInteger, "a0", rank.ErrInvalidKey},
		{"trailing zero fraction", "a00", "a1", rank.ErrInvalidKey},
		{"reversed", "a5", "a2", rank.ErrOrder},
		{"equal", "a5", "a5", rank.ErrOrder},
		{"reversed prefix", "a0V", "a0", rank.ErrOrder},
		{"byte order not natural order", "a1", "Zz", rank.ErrOrder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := rank.Between(tt.a, tt.b)
			require.ErrorIs(t, err, tt.err)
			assert.Empty(t, got)
		})
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	valid := []string{"a0", "a1", "az", "Zz", "a0V", "b125", "Y00", "c000", "a0" + strings.Repeat("1", 30), largestInteger, "zzzzzzzzzzzzzzzzzzzzzzzzzzzV"}
	for _, k := range valid {
		assert.NoError(t, rank.Validate(k), k)
	}

	invalid := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"head is digit", "0"},
		{"head is digit with tail", "0abc"},
		{"integer part too short", "a"},
		{"integer part too short for b", "b0"},
		{"integer part too short for Y", "Y0"},
		{"trailing zero in fraction", "a00"},
		{"trailing zero after fraction digits", "a1V0"},
		{"smallest integer", smallestInteger},
		{"punctuation", "a0!"},
		{"space", "a0 "},
		{"underscore", "a0_"},
		{"dash", "a-1"},
		{"non ascii letter", "a0é"},
		{"null byte", "a0\x00"},
		{"newline", "a0\n"},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.ErrorIs(t, rank.Validate(tt.key), rank.ErrInvalidKey)
		})
	}
}

func TestValidateAcceptsEverythingBetweenProduces(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(3, 9))
	keys := []string{}
	for range 3000 {
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
		require.NoError(t, rank.Validate(k), k)
		keys = append(keys[:i], append([]string{k}, keys[i:]...)...)
	}
	assert.True(t, isSortedStrictly(keys), "keys must stay strictly ordered with no duplicates")
}

func TestNBetweenMonotonicAndInRange(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(11, 13))
	for range 300 {
		a, b := randomKey(rng), randomKey(rng)
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		n := rng.IntN(40) + 2
		keys, err := rank.NBetween(a, b, n)
		if err != nil {
			require.ErrorIs(t, err, rank.ErrExhausted)
			continue
		}
		require.Len(t, keys, n)
		all := append(append([]string{a}, keys...), b)
		require.True(t, isSortedStrictly(all), "NBetween(%q, %q, %d) = %v", a, b, n, keys)
		for _, k := range keys {
			require.NoError(t, rank.Validate(k), k)
		}
	}
}

func TestNBetweenOpenEnds(t *testing.T) {
	t.Parallel()
	for _, n := range []int{2, 3, 10, 200} {
		after, err := rank.NBetween("a5", "", n)
		require.NoError(t, err)
		require.Len(t, after, n)
		assert.True(t, isSortedStrictly(append([]string{"a5"}, after...)), "n=%d after", n)

		before, err := rank.NBetween("", "a5", n)
		require.NoError(t, err)
		require.Len(t, before, n)
		assert.True(t, isSortedStrictly(append(append([]string{}, before...), "a5")), "n=%d before", n)

		both, err := rank.NBetween("", "", n)
		require.NoError(t, err)
		require.Len(t, both, n)
		assert.True(t, isSortedStrictly(both), "n=%d both", n)
	}
}

func TestNBetweenNonPositiveCount(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, -1, -100} {
		keys, err := rank.NBetween("a0", "a1", n)
		require.NoError(t, err)
		assert.NotNil(t, keys)
		assert.Empty(t, keys)
	}
}

func TestNBetweenSingleMatchesBetween(t *testing.T) {
	t.Parallel()
	want, err := rank.Between("a0", "a1")
	require.NoError(t, err)
	got, err := rank.NBetween("a0", "a1", 1)
	require.NoError(t, err)
	assert.Equal(t, []string{want}, got)
}

func TestNBetweenErrors(t *testing.T) {
	t.Parallel()
	_, err := rank.NBetween("a5", "a2", 3)
	assert.ErrorIs(t, err, rank.ErrOrder)
	_, err = rank.NBetween("a5", "a5", 3)
	assert.ErrorIs(t, err, rank.ErrOrder)
	_, err = rank.NBetween("a00", "a5", 3)
	assert.ErrorIs(t, err, rank.ErrInvalidKey)
	_, err = rank.NBetween("a1", "a2!", 1)
	assert.ErrorIs(t, err, rank.ErrInvalidKey)
	_, err = rank.NBetween("", "!", 5)
	assert.ErrorIs(t, err, rank.ErrInvalidKey)
}

func TestNBetweenKeepsKeysShortInTightRange(t *testing.T) {
	t.Parallel()
	keys, err := rank.NBetween("a0", "a1", 1000)
	require.NoError(t, err)
	require.Len(t, keys, 1000)
	longest := 0
	for _, k := range keys {
		longest = max(longest, len(k))
	}
	assert.LessOrEqual(t, longest, 6, "balanced bisection must not degrade into a chain")
}

func TestEvenlySortedUniqueAndShort(t *testing.T) {
	t.Parallel()
	for _, n := range []int{1, 2, 3, 10, 61, 62, 63, 500, 4000} {
		keys, err := rank.Evenly(n)
		require.NoError(t, err)
		require.Len(t, keys, n)
		require.True(t, isSortedStrictly(keys), "n=%d", n)
		seen := map[string]struct{}{}
		for _, k := range keys {
			require.NoError(t, rank.Validate(k), k)
			require.LessOrEqual(t, len(k), 6, "n=%d key %q", n, k)
			_, dup := seen[k]
			require.False(t, dup, "duplicate key %q", k)
			seen[k] = struct{}{}
		}
	}
}

func TestEvenlyEdgeCounts(t *testing.T) {
	t.Parallel()
	one, err := rank.Evenly(1)
	require.NoError(t, err)
	assert.Equal(t, []string{"a0"}, one)

	for _, n := range []int{0, -1} {
		none, err := rank.Evenly(n)
		require.NoError(t, err)
		assert.NotNil(t, none)
		assert.Empty(t, none)
	}
}

func TestEvenlyKeysLeaveRoomOnBothSides(t *testing.T) {
	t.Parallel()
	keys, err := rank.Evenly(200)
	require.NoError(t, err)

	first, err := rank.Between("", keys[0])
	require.NoError(t, err)
	assert.Less(t, first, keys[0])

	last, err := rank.Between(keys[len(keys)-1], "")
	require.NoError(t, err)
	assert.Greater(t, last, keys[len(keys)-1])

	for i := 1; i < len(keys); i++ {
		mid, err := rank.Between(keys[i-1], keys[i])
		require.NoError(t, err)
		require.Less(t, keys[i-1], mid)
		require.Less(t, mid, keys[i])
	}
}

func TestRepeatedInsertsAtOnePointGrowSlowlyThenExceedMaxLength(t *testing.T) {
	t.Parallel()
	for _, direction := range []string{"just after the lower neighbour", "just before the upper neighbour"} {
		t.Run(direction, func(t *testing.T) {
			t.Parallel()
			lo, hi := "a0", "a1"
			lengths := make([]int, 0, 600)
			firstOver := -1
			for i := 1; i <= 600; i++ {
				k, err := rank.Between(lo, hi)
				require.NoError(t, err)
				require.Less(t, lo, k)
				require.Less(t, k, hi)
				require.NoError(t, rank.Validate(k))
				if direction == "just after the lower neighbour" {
					hi = k
				} else {
					lo = k
				}
				lengths = append(lengths, len(k))
				if firstOver < 0 && len(k) > rank.MaxLength {
					firstOver = i
				}
			}
			assert.True(t, sort.IntsAreSorted(lengths), "key length never shrinks while inserting at one point")
			assert.LessOrEqual(t, lengths[9], 5, "ten inserts at one point stay short")
			assert.Greater(t, lengths[len(lengths)-1], rank.MaxLength)
			require.Positive(t, firstOver, "the length cap must eventually be exceeded")
			assert.Greater(t, firstOver, 100, "hundreds of inserts at one point are needed before a rebalance")
			assert.Less(t, firstOver, 600)
			for i, l := range lengths {
				assert.LessOrEqual(t, l, i/5+3, "growth must stay close to linear at roughly one char per six inserts (insert %d)", i+1)
			}
		})
	}
}

func TestInsertsSpreadAcrossAListStayShort(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(5, 6))
	keys := []string{}
	for range 5000 {
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
		keys = append(keys[:i], append([]string{k}, keys[i:]...)...)
	}
	longest := 0
	for _, k := range keys {
		longest = max(longest, len(k))
	}
	assert.Less(t, longest, rank.MaxLength, "random insert positions must not need a rebalance")
}

func TestPrependingKeepsKeysShort(t *testing.T) {
	t.Parallel()
	k := "a0"
	for range 3000 {
		next, err := rank.Between("", k)
		require.NoError(t, err)
		require.Less(t, next, k)
		require.NoError(t, rank.Validate(next))
		k = next
	}
	assert.LessOrEqual(t, len(k), 6)
}
