package ops_test

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/rank"
)

type anchorCase struct {
	name   string
	after  string
	before string
	want   []string
}

const (
	anchorNone     = ""
	anchorMissing  = "missing"
	anchorOtherCol = "other-column"
	anchorArchived = "archived"
)

var anchorCases = []anchorCase{
	{"no anchors goes to the tail", anchorNone, anchorNone, []string{"A", "B", "C", "D", "N"}},
	{"after first", "A", anchorNone, []string{"A", "N", "B", "C", "D"}},
	{"after middle", "B", anchorNone, []string{"A", "B", "N", "C", "D"}},
	{"after last", "D", anchorNone, []string{"A", "B", "C", "D", "N"}},
	{"before first", anchorNone, "A", []string{"N", "A", "B", "C", "D"}},
	{"before middle", anchorNone, "C", []string{"A", "B", "N", "C", "D"}},
	{"before last", anchorNone, "D", []string{"A", "B", "C", "N", "D"}},
	{"both neighbours", "B", "C", []string{"A", "B", "N", "C", "D"}},
	{"both at the edges", "D", anchorNone, []string{"A", "B", "C", "D", "N"}},
	{"after wins over a distant before", "A", "D", []string{"A", "N", "B", "C", "D"}},
	{"after wins over an earlier before", "C", "A", []string{"A", "B", "C", "N", "D"}},
	{"missing after falls back to before", anchorMissing, "C", []string{"A", "B", "N", "C", "D"}},
	{"missing before is ignored", "B", anchorMissing, []string{"A", "B", "N", "C", "D"}},
	{"both missing goes to the tail", anchorMissing, anchorMissing, []string{"A", "B", "C", "D", "N"}},
	{"after in another column falls back to the tail", anchorOtherCol, anchorNone, []string{"A", "B", "C", "D", "N"}},
	{"before in another column falls back to the tail", anchorNone, anchorOtherCol, []string{"A", "B", "C", "D", "N"}},
	{"foreign after falls back to before", anchorOtherCol, "B", []string{"A", "N", "B", "C", "D"}},
	{"archived after is ignored", anchorArchived, anchorNone, []string{"A", "B", "C", "D", "N"}},
	{"archived before is ignored", anchorNone, anchorArchived, []string{"A", "B", "C", "D", "N"}},
	{"archived after falls back to before", anchorArchived, "A", []string{"N", "A", "B", "C", "D"}},
}

type anchorWorld struct {
	f        *fixture
	byTitle  map[string]uuid.UUID
	archived uuid.UUID
	foreign  uuid.UUID
}

func newAnchorWorld(t *testing.T) *anchorWorld {
	t.Helper()
	f := newFixture(t)
	w := &anchorWorld{f: f, byTitle: map[string]uuid.UUID{}}
	other := f.column(t, "other", nil)
	w.foreign = f.card(t, other, "X")
	return w
}

func (w *anchorWorld) freshColumn(t *testing.T) uuid.UUID {
	t.Helper()
	col := w.f.column(t, "col", nil)
	for _, title := range []string{"A", "B", "C", "D"} {
		w.byTitle[title] = w.f.card(t, col, title)
	}
	w.archived = w.f.card(t, col, "E")
	w.f.mustEvent(t, boardcontent.OpCardArchive, boardcontent.CardArchive{CardID: w.archived})
	return col
}

func (w *anchorWorld) resolve(name string) *uuid.UUID {
	switch name {
	case anchorNone:
		return nil
	case anchorMissing:
		id := uuid.New()
		return &id
	case anchorOtherCol:
		return &w.foreign
	case anchorArchived:
		return &w.archived
	}
	id := w.byTitle[name]
	return &id
}

func titlesIn(f *fixture, t *testing.T, col uuid.UUID) []string {
	t.Helper()
	return cardTitles(f.snapshot(t), col)
}

func TestCardCreatePlacementForEveryAnchorCombination(t *testing.T) {
	w := newAnchorWorld(t)
	for _, tc := range anchorCases {
		t.Run(tc.name, func(t *testing.T) {
			col := w.freshColumn(t)
			w.f.mustEvent(t, boardcontent.OpCardCreate, boardcontent.CardCreate{
				CardID: uuid.New(), ColumnID: col, Title: "N", AfterID: w.resolve(tc.after), BeforeID: w.resolve(tc.before),
			})
			assert.Equal(t, tc.want, titlesIn(w.f, t, col))
		})
	}
}

func TestCardMovePlacementForEveryAnchorCombination(t *testing.T) {
	w := newAnchorWorld(t)
	starts := map[string]func(t *testing.T, col uuid.UUID) uuid.UUID{
		"from the tail of the same column": func(t *testing.T, col uuid.UUID) uuid.UUID {
			return w.f.card(t, col, "N")
		},
		"from the head of the same column": func(t *testing.T, col uuid.UUID) uuid.UUID {
			id := uuid.New()
			first := w.byTitle["A"]
			w.f.mustEvent(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: id, ColumnID: col, Title: "N", BeforeID: &first})
			return id
		},
		"from another column": func(t *testing.T, _ uuid.UUID) uuid.UUID {
			other := w.f.column(t, "source", nil)
			return w.f.card(t, other, "N")
		},
	}
	for startName, start := range starts {
		for _, tc := range anchorCases {
			t.Run(startName+"/"+tc.name, func(t *testing.T) {
				col := w.freshColumn(t)
				mover := start(t, col)
				w.f.mustEvent(t, boardcontent.OpCardMove, boardcontent.CardMove{
					CardID: mover, ToColumnID: col, AfterID: w.resolve(tc.after), BeforeID: w.resolve(tc.before),
					BaseVersion: ptr(w.f.cardRow(t, mover).Version),
				})
				assert.Equal(t, tc.want, titlesIn(w.f, t, col))
				assert.Equal(t, col, w.f.cardRow(t, mover).ColumnID)
			})
		}
	}
}

func TestCardMoveWithinColumnToEveryPosition(t *testing.T) {
	w := newAnchorWorld(t)
	titles := []string{"A", "B", "C", "D"}
	for _, mover := range titles {
		for target := 0; target < len(titles); target++ {
			t.Run(mover+"/slot"+string(rune('0'+target)), func(t *testing.T) {
				col := w.freshColumn(t)
				var rest []string
				for _, title := range titles {
					if title != mover {
						rest = append(rest, title)
					}
				}
				want := append(append(append([]string{}, rest[:target]...), mover), rest[target:]...)
				move := boardcontent.CardMove{CardID: w.byTitle[mover], ToColumnID: col, BaseVersion: ptr(int64(1))}
				if target == 0 {
					before := w.byTitle[rest[0]]
					move.BeforeID = &before
				} else {
					after := w.byTitle[rest[target-1]]
					move.AfterID = &after
				}
				w.f.mustEvent(t, boardcontent.OpCardMove, move)
				assert.Equal(t, want, titlesIn(w.f, t, col))
				assert.EqualValues(t, 2, w.f.cardRow(t, w.byTitle[mover]).Version)
			})
		}
	}
}

func TestCardPlacementRejectsSelfAnchors(t *testing.T) {
	w := newAnchorWorld(t)
	col := w.freshColumn(t)
	self := w.byTitle["B"]
	seq := w.f.boardSeq(t)

	w.f.mustNack(t, boardcontent.ReasonValidation, boardcontent.OpCardMove, boardcontent.CardMove{CardID: self, ToColumnID: col, AfterID: &self, BaseVersion: ptr(int64(1))})
	w.f.mustNack(t, boardcontent.ReasonValidation, boardcontent.OpCardMove, boardcontent.CardMove{CardID: self, ToColumnID: col, BeforeID: &self, BaseVersion: ptr(int64(1))})
	w.f.requireUnchanged(t, seq)
	assert.Equal(t, []string{"A", "B", "C", "D"}, titlesIn(w.f, t, col))
}

func TestColumnPlacementForEveryAnchorCombination(t *testing.T) {
	f := newFixture(t)
	ids := map[string]uuid.UUID{}
	var prev *uuid.UUID
	for _, title := range []string{"A", "B", "C", "D"} {
		id := f.column(t, title, prev)
		ids[title] = id
		prev = &id
	}
	archived := f.column(t, "E", prev)
	f.mustEvent(t, boardcontent.OpColumnArchive, boardcontent.ColumnArchive{ColumnID: archived})
	titles := func() []string {
		var out []string
		for _, c := range f.snapshot(t).Columns {
			out = append(out, c.Title)
		}
		return out
	}
	resolve := func(name string) *uuid.UUID {
		switch name {
		case anchorNone:
			return nil
		case anchorMissing:
			id := uuid.New()
			return &id
		case anchorOtherCol:
			return nil
		case anchorArchived:
			return &archived
		}
		id := ids[name]
		return &id
	}
	for _, tc := range anchorCases {
		if tc.after == anchorOtherCol || tc.before == anchorOtherCol {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			id := uuid.New()
			f.mustEvent(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: id, Title: "N", AfterID: resolve(tc.after), BeforeID: resolve(tc.before)})
			assert.Equal(t, tc.want, titles())
			f.mustEvent(t, boardcontent.OpColumnArchive, boardcontent.ColumnArchive{ColumnID: id})
			assert.Equal(t, []string{"A", "B", "C", "D"}, titles())
		})
	}
}

func TestColumnMovePlacementForEveryPosition(t *testing.T) {
	titles := []string{"A", "B", "C", "D"}
	f := newFixture(t)
	for _, mover := range titles {
		for target := 0; target < len(titles); target++ {
			t.Run(mover+"/slot"+string(rune('0'+target)), func(t *testing.T) {
				ids := map[string]uuid.UUID{}
				for _, c := range f.snapshot(t).Columns {
					f.mustEvent(t, boardcontent.OpColumnArchive, boardcontent.ColumnArchive{ColumnID: c.ID})
				}
				var prev *uuid.UUID
				for _, title := range titles {
					id := f.column(t, title, prev)
					ids[title] = id
					prev = &id
				}
				var rest []string
				for _, title := range titles {
					if title != mover {
						rest = append(rest, title)
					}
				}
				want := append(append(append([]string{}, rest[:target]...), mover), rest[target:]...)
				move := boardcontent.ColumnMove{ColumnID: ids[mover], BaseVersion: ptr(int64(1))}
				if target == 0 {
					before := ids[rest[0]]
					move.BeforeID = &before
				} else {
					after := ids[rest[target-1]]
					move.AfterID = &after
				}
				f.mustEvent(t, boardcontent.OpColumnMove, move)
				var got []string
				for _, c := range f.snapshot(t).Columns {
					got = append(got, c.Title)
				}
				assert.Equal(t, want, got)
			})
		}
	}
}

func insertAt(list []uuid.UUID, self uuid.UUID, after, before *uuid.UUID) []uuid.UUID {
	others := make([]uuid.UUID, 0, len(list))
	for _, id := range list {
		if id != self {
			others = append(others, id)
		}
	}
	index := func(a *uuid.UUID) int {
		if a == nil {
			return -1
		}
		for i, id := range others {
			if id == *a {
				return i
			}
		}
		return -1
	}
	pos := len(others)
	if i := index(after); i >= 0 {
		pos = i + 1
	} else if i := index(before); i >= 0 {
		pos = i
	}
	out := make([]uuid.UUID, 0, len(list))
	out = append(out, others[:pos]...)
	out = append(out, self)
	return append(out, others[pos:]...)
}

func remove(list []uuid.UUID, id uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(list))
	for _, v := range list {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

func pickAnchor(rng *rand.Rand, pool []uuid.UUID) *uuid.UUID {
	switch n := rng.Intn(10); {
	case n < 3 || len(pool) == 0:
		return nil
	case n < 4:
		id := uuid.New()
		return &id
	}
	id := pool[rng.Intn(len(pool))]
	return &id
}

func TestRandomCardOperationsMatchTheFullListModel(t *testing.T) {
	for _, seed := range []int64{1, 2, 3} {
		t.Run("seed"+string(rune('0'+seed)), func(t *testing.T) {
			f := newFixture(t)
			rng := rand.New(rand.NewSource(seed))
			columns := []uuid.UUID{f.column(t, "c1", nil), f.column(t, "c2", nil), f.column(t, "c3", nil)}
			model := map[uuid.UUID][]uuid.UUID{}
			var everyCard, active []uuid.UUID
			colOf := map[uuid.UUID]uuid.UUID{}

			verify := func(step int) {
				for _, c := range columns {
					require.Equal(t, nilToEmpty(model[c]), nilToEmpty(f.cardsInColumn(t, c)), "step %d column %s", step, c)
				}
			}
			for step := range 220 {
				switch n := rng.Intn(10); {
				case n < 4 || len(active) < 2:
					id := uuid.New()
					col := columns[rng.Intn(len(columns))]
					after, before := pickAnchor(rng, everyCard), pickAnchor(rng, everyCard)
					f.mustEvent(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: id, ColumnID: col, Title: "c", AfterID: after, BeforeID: before})
					model[col] = insertAt(model[col], id, after, before)
					everyCard = append(everyCard, id)
					active = append(active, id)
					colOf[id] = col
				case n < 9:
					id := active[rng.Intn(len(active))]
					col := columns[rng.Intn(len(columns))]
					after, before := pickAnchor(rng, everyCard), pickAnchor(rng, everyCard)
					move := boardcontent.CardMove{CardID: id, ToColumnID: col, AfterID: after, BeforeID: before, BaseVersion: ptr(f.cardRow(t, id).Version)}
					if (after != nil && *after == id) || (before != nil && *before == id) {
						f.mustNack(t, boardcontent.ReasonValidation, boardcontent.OpCardMove, move)
					} else {
						f.mustEvent(t, boardcontent.OpCardMove, move)
						model[colOf[id]] = remove(model[colOf[id]], id)
						model[col] = insertAt(model[col], id, after, before)
						colOf[id] = col
					}
				default:
					id := active[rng.Intn(len(active))]
					f.mustEvent(t, boardcontent.OpCardArchive, boardcontent.CardArchive{CardID: id})
					model[colOf[id]] = remove(model[colOf[id]], id)
					active = remove(active, id)
				}
				verify(step)
			}
		})
	}
}

func nilToEmpty(in []uuid.UUID) []uuid.UUID {
	if in == nil {
		return []uuid.UUID{}
	}
	return in
}

func TestRandomColumnOperationsMatchTheFullListModel(t *testing.T) {
	for _, seed := range []int64{11, 12} {
		t.Run("seed"+string(rune('0'+seed%10)), func(t *testing.T) {
			f := newFixture(t)
			rng := rand.New(rand.NewSource(seed))
			model := []uuid.UUID{}
			var every, active []uuid.UUID

			for step := range 150 {
				switch n := rng.Intn(10); {
				case n < 5 || len(active) < 2:
					id := uuid.New()
					after, before := pickAnchor(rng, every), pickAnchor(rng, every)
					f.mustEvent(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: id, Title: "c", AfterID: after, BeforeID: before})
					model = insertAt(model, id, after, before)
					every = append(every, id)
					active = append(active, id)
				case n < 9:
					id := active[rng.Intn(len(active))]
					after, before := pickAnchor(rng, every), pickAnchor(rng, every)
					move := boardcontent.ColumnMove{ColumnID: id, AfterID: after, BeforeID: before, BaseVersion: ptr(f.columnRow(t, id).Version)}
					if (after != nil && *after == id) || (before != nil && *before == id) {
						f.mustNack(t, boardcontent.ReasonValidation, boardcontent.OpColumnMove, move)
					} else {
						f.mustEvent(t, boardcontent.OpColumnMove, move)
						model = insertAt(model, id, after, before)
					}
				default:
					id := active[rng.Intn(len(active))]
					f.mustEvent(t, boardcontent.OpColumnArchive, boardcontent.ColumnArchive{ColumnID: id})
					model = remove(model, id)
					active = remove(active, id)
				}
				require.Equal(t, model, nilToEmpty(f.columnOrder(t)), "step %d", step)
			}
		})
	}
}

func TestCardRankChurnForcesRebalanceAndKeepsOrderAndBounds(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "first")
	last := f.card(t, col, "last")
	order := []uuid.UUID{first, last}
	rebalances := 0

	for i := range 400 {
		id := uuid.New()
		out := f.apply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: id, ColumnID: col, Title: "n", AfterID: &first})
		require.Nil(t, out.Nack)
		order = insertAt(order, id, &first, nil)
		for _, ev := range out.Extra {
			if ev.Type == boardcontent.EventColumnRebalanced {
				rebalances++
				payload := decode[boardcontent.ColumnRebalanced](t, ev)
				assert.Len(t, payload.Cards, len(order)-1, "the rebalance covers every card that existed before the insert")
			}
		}
		if i%50 == 49 {
			require.Equal(t, order, f.cardsInColumn(t, col))
		}
	}
	require.Positive(t, rebalances)
	require.Equal(t, order, f.cardsInColumn(t, col))

	var ranks []string
	for _, c := range f.snapshot(t).Cards {
		ranks = append(ranks, c.Rank)
		assert.LessOrEqual(t, len(c.Rank), rank.MaxLength)
		assert.NoError(t, rank.Validate(c.Rank))
	}
	assert.True(t, sort.StringsAreSorted(ranks))
	assert.Len(t, uniqueStrings(ranks), len(ranks))
	f.requireGapFreeSeq(t)
}

func uniqueStrings(in []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, s := range in {
		out[s] = struct{}{}
	}
	return out
}

func TestCardMoveChurnForcesRebalanceAndKeepsOrder(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	other := f.column(t, "Other", &col)
	anchorA := f.card(t, col, "a")
	anchorB := f.card(t, col, "b")
	order := []uuid.UUID{anchorA, anchorB}
	var pending []uuid.UUID
	for range 8 {
		pending = append(pending, f.card(t, other, "m"))
	}
	rebalances := 0
	for round := range 500 {
		id := pending[round%len(pending)]
		out := f.apply(t, boardcontent.OpCardMove, boardcontent.CardMove{CardID: id, ToColumnID: col, AfterID: &anchorA, BaseVersion: ptr(f.cardRow(t, id).Version)})
		require.Nil(t, out.Nack)
		for _, ev := range out.Extra {
			if ev.Type == boardcontent.EventColumnRebalanced {
				rebalances++
			}
		}
		order = insertAt(order, id, &anchorA, nil)
		require.Equal(t, order, f.cardsInColumn(t, col), "round %d", round)
	}
	assert.Positive(t, rebalances)
}

func TestColumnRankChurnForcesRebalanceAndKeepsOrder(t *testing.T) {
	f := newFixture(t)
	first := f.column(t, "first", nil)
	last := f.column(t, "last", &first)
	order := []uuid.UUID{first, last}
	rebalances := 0

	for range 400 {
		id := uuid.New()
		out := f.apply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: id, Title: "n", AfterID: &first})
		require.Nil(t, out.Nack)
		order = insertAt(order, id, &first, nil)
		for _, ev := range out.Extra {
			if ev.Type == boardcontent.EventColumnsRebalanced {
				rebalances++
			}
		}
	}
	require.Positive(t, rebalances)
	assert.Equal(t, order, f.columnOrder(t))
	for _, c := range f.snapshot(t).Columns {
		assert.LessOrEqual(t, len(c.Rank), rank.MaxLength)
	}
	f.requireGapFreeSeq(t)
}

func TestPlacementAfterRebalanceUsesTheCompactRanksAgain(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "first")
	last := f.card(t, col, "last")
	for {
		out := f.apply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "n", AfterID: &first})
		if len(out.Extra) > 0 {
			break
		}
	}
	before := f.snapshot(t).Seq
	out := f.apply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "plain", BeforeID: &last})
	assert.Empty(t, out.Extra, "right after a rebalance there is room again")
	assert.Equal(t, before+1, f.snapshot(t).Seq)
}
