package simclient

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type viewFixture struct {
	state *State
	view  *view
	cols  [3]uuid.UUID
	cards [3]uuid.UUID
}

func newViewFixture(t *testing.T) *viewFixture {
	t.Helper()
	f := &viewFixture{state: NewState()}
	for i := range f.cols {
		f.cols[i] = uuid.New()
	}
	for i := range f.cards {
		f.cards[i] = uuid.New()
	}
	f.state.Load(protocol.Snapshot{
		Seq: 1,
		Columns: []boardcontent.Column{
			{ID: f.cols[0], Title: "A", Rank: "b", Version: 1},
			{ID: f.cols[1], Title: "B", Rank: "d", Version: 1},
			{ID: f.cols[2], Title: "C", Rank: "f", Version: 1},
		},
		Cards: []boardcontent.Card{
			{ID: f.cards[0], ColumnID: f.cols[0], Title: "k0", Rank: "b", Version: 1, DescriptionVersion: 1},
			{ID: f.cards[1], ColumnID: f.cols[0], Title: "k1", Rank: "d", Version: 1, DescriptionVersion: 1},
			{ID: f.cards[2], ColumnID: f.cols[0], Title: "k2", Rank: "f", Version: 1, DescriptionVersion: 1},
		},
	})
	f.view = buildView(f.state)
	return f
}

func (f *viewFixture) colIDs() []uuid.UUID { return columnIDs(f.view.cols) }

func (f *viewFixture) cardIDs(col int) []uuid.UUID {
	_, c := f.view.column(f.cols[col])
	return cardIDs(c.Cards)
}

func op(payload any) PendingOp { return PendingOp{ID: uuid.New(), Payload: payload} }

func TestBuildViewFollowsRankOrderAfterRebalance(t *testing.T) {
	f := newViewFixture(t)
	assert.Equal(t, []uuid.UUID{f.cols[0], f.cols[1], f.cols[2]}, f.colIDs())
	require.NoError(t, f.state.Apply(event(t, 2, boardcontent.EventColumnsRebalanced, nil, boardcontent.ColumnsRebalanced{
		Columns: []boardcontent.ColumnRank{{ColumnID: f.cols[0], Rank: "x"}, {ColumnID: f.cols[1], Rank: "a"}, {ColumnID: f.cols[2], Rank: "c"}},
	})))
	require.NoError(t, f.state.Apply(event(t, 3, boardcontent.EventColumnRebalanced, nil, boardcontent.ColumnRebalanced{
		ColumnID: f.cols[0],
		Cards:    []boardcontent.CardRank{{CardID: f.cards[0], Rank: "x"}, {CardID: f.cards[1], Rank: "m"}, {CardID: f.cards[2], Rank: "a"}},
	})))
	f.view = buildView(f.state)
	assert.Equal(t, []uuid.UUID{f.cols[1], f.cols[2], f.cols[0]}, f.colIDs())
	assert.Equal(t, []uuid.UUID{f.cards[2], f.cards[1], f.cards[0]}, f.cardIDs(0))
}

func TestColumnCreatePlacement(t *testing.T) {
	cases := map[string]struct {
		after, before func(f *viewFixture) *uuid.UUID
		wantIndex     int
	}{
		"after first":               {func(f *viewFixture) *uuid.UUID { return &f.cols[0] }, nil, 1},
		"before first":              {nil, func(f *viewFixture) *uuid.UUID { return &f.cols[0] }, 0},
		"after last":                {func(f *viewFixture) *uuid.UUID { return &f.cols[2] }, nil, 3},
		"between":                   {func(f *viewFixture) *uuid.UUID { return &f.cols[0] }, func(f *viewFixture) *uuid.UUID { return &f.cols[1] }, 1},
		"after wins":                {func(f *viewFixture) *uuid.UUID { return &f.cols[1] }, func(f *viewFixture) *uuid.UUID { return &f.cols[0] }, 2},
		"no anchors":                {nil, nil, 3},
		"stale anchors":             {func(*viewFixture) *uuid.UUID { id := uuid.New(); return &id }, func(*viewFixture) *uuid.UUID { id := uuid.New(); return &id }, 3},
		"stale after, valid before": {func(*viewFixture) *uuid.UUID { id := uuid.New(); return &id }, func(f *viewFixture) *uuid.UUID { return &f.cols[1] }, 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newViewFixture(t)
			created := &boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "N"}
			if tc.after != nil {
				created.AfterID = tc.after(f)
			}
			if tc.before != nil {
				created.BeforeID = tc.before(f)
			}
			require.True(t, f.view.apply(op(created)))
			ids := f.colIDs()
			require.Len(t, ids, 4)
			assert.Equal(t, created.ColumnID, ids[tc.wantIndex])
		})
	}
}

func TestColumnCreateRejectsDuplicates(t *testing.T) {
	f := newViewFixture(t)
	assert.False(t, f.view.apply(op(&boardcontent.ColumnCreate{ColumnID: f.cols[0], Title: "dup"})))
	assert.Len(t, f.view.cols, 3)
}

func TestColumnRenameBumpsVersionAndRejectsUnknown(t *testing.T) {
	f := newViewFixture(t)
	assert.True(t, f.view.apply(op(&boardcontent.ColumnRename{ColumnID: f.cols[1], Title: "R"})))
	_, col := f.view.column(f.cols[1])
	assert.Equal(t, "R", col.Title)
	assert.Equal(t, int64(2), col.Version)
	assert.False(t, f.view.apply(op(&boardcontent.ColumnRename{ColumnID: uuid.New(), Title: "x"})))
}

func TestColumnMoveChecksTheBaseVersion(t *testing.T) {
	f := newViewFixture(t)
	good, stale := int64(1), int64(7)
	assert.False(t, f.view.apply(op(&boardcontent.ColumnMove{ColumnID: f.cols[0], AfterID: &f.cols[2], BaseVersion: &stale})))
	assert.False(t, f.view.apply(op(&boardcontent.ColumnMove{ColumnID: f.cols[0], AfterID: &f.cols[2]})))
	assert.Equal(t, []uuid.UUID{f.cols[0], f.cols[1], f.cols[2]}, f.colIDs())
	assert.True(t, f.view.apply(op(&boardcontent.ColumnMove{ColumnID: f.cols[0], AfterID: &f.cols[2], BaseVersion: &good})))
	assert.Equal(t, []uuid.UUID{f.cols[1], f.cols[2], f.cols[0]}, f.colIDs())
	_, col := f.view.column(f.cols[0])
	assert.Equal(t, int64(2), col.Version)
	assert.False(t, f.view.apply(op(&boardcontent.ColumnMove{ColumnID: f.cols[0], BeforeID: &f.cols[1], BaseVersion: &good})))
}

func TestColumnArchiveRemovesTheColumnAndBlocksDependants(t *testing.T) {
	f := newViewFixture(t)
	assert.True(t, f.view.apply(op(&boardcontent.ColumnArchive{ColumnID: f.cols[0]})))
	assert.Len(t, f.view.cols, 2)
	assert.False(t, f.view.apply(op(&boardcontent.ColumnArchive{ColumnID: f.cols[0]})))
	assert.False(t, f.view.apply(op(&boardcontent.CardCreate{CardID: uuid.New(), ColumnID: f.cols[0], Title: "x"})))
	assert.False(t, f.view.apply(op(&boardcontent.CardEdit{CardID: f.cards[0], Title: ptr("x")})))
}

func TestCardCreatePlacementAndValidation(t *testing.T) {
	f := newViewFixture(t)
	fresh := uuid.New()
	desc := "hello"
	require.True(t, f.view.apply(op(&boardcontent.CardCreate{CardID: fresh, ColumnID: f.cols[0], Title: "n", Description: &desc, AfterID: &f.cards[0]})))
	assert.Equal(t, []uuid.UUID{f.cards[0], fresh, f.cards[1], f.cards[2]}, f.cardIDs(0))
	_, _, card := f.view.card(fresh)
	assert.Equal(t, "hello", card.Description)
	assert.Equal(t, int64(1), card.Version)
	assert.Equal(t, int64(1), card.DescVersion)

	second := uuid.New()
	require.True(t, f.view.apply(op(&boardcontent.CardCreate{CardID: second, ColumnID: f.cols[1], Title: "n2", BeforeID: &f.cards[0]})))
	assert.Equal(t, []uuid.UUID{second}, f.cardIDs(1))

	assert.False(t, f.view.apply(op(&boardcontent.CardCreate{CardID: fresh, ColumnID: f.cols[1], Title: "dup"})))
	assert.False(t, f.view.apply(op(&boardcontent.CardCreate{CardID: uuid.New(), ColumnID: uuid.New(), Title: "orphan"})))
}

func TestCardEditRespectsDescriptionBaseVersion(t *testing.T) {
	f := newViewFixture(t)
	desc, title := "d", "t"
	good, stale := int64(1), int64(4)
	assert.False(t, f.view.apply(op(&boardcontent.CardEdit{CardID: f.cards[0], Description: &desc})))
	assert.False(t, f.view.apply(op(&boardcontent.CardEdit{CardID: f.cards[0], Description: &desc, BaseDescriptionVersion: &stale})))
	_, _, card := f.view.card(f.cards[0])
	assert.Equal(t, int64(1), card.Version)

	assert.True(t, f.view.apply(op(&boardcontent.CardEdit{CardID: f.cards[0], Title: &title})))
	assert.Equal(t, int64(2), card.Version)
	assert.Equal(t, int64(1), card.DescVersion)

	assert.True(t, f.view.apply(op(&boardcontent.CardEdit{CardID: f.cards[0], Description: &desc, Title: &title, BaseDescriptionVersion: &good})))
	assert.Equal(t, int64(3), card.Version)
	assert.Equal(t, int64(2), card.DescVersion)
	assert.Equal(t, "d", card.Description)

	assert.False(t, f.view.apply(op(&boardcontent.CardEdit{CardID: uuid.New(), Title: &title})))
}

func TestCardMoveAcrossAndWithinColumns(t *testing.T) {
	f := newViewFixture(t)
	v1, stale := int64(1), int64(9)
	assert.False(t, f.view.apply(op(&boardcontent.CardMove{CardID: f.cards[0], ToColumnID: f.cols[1], BaseVersion: &stale})))
	assert.False(t, f.view.apply(op(&boardcontent.CardMove{CardID: f.cards[0], ToColumnID: f.cols[1]})))
	assert.False(t, f.view.apply(op(&boardcontent.CardMove{CardID: f.cards[0], ToColumnID: uuid.New(), BaseVersion: &v1})))
	assert.Equal(t, []uuid.UUID{f.cards[0], f.cards[1], f.cards[2]}, f.cardIDs(0))

	assert.True(t, f.view.apply(op(&boardcontent.CardMove{CardID: f.cards[0], ToColumnID: f.cols[1], BaseVersion: &v1})))
	assert.Equal(t, []uuid.UUID{f.cards[1], f.cards[2]}, f.cardIDs(0))
	assert.Equal(t, []uuid.UUID{f.cards[0]}, f.cardIDs(1))
	_, _, card := f.view.card(f.cards[0])
	assert.Equal(t, int64(2), card.Version)

	assert.True(t, f.view.apply(op(&boardcontent.CardMove{CardID: f.cards[1], ToColumnID: f.cols[0], AfterID: &f.cards[2], BaseVersion: &v1})))
	assert.Equal(t, []uuid.UUID{f.cards[2], f.cards[1]}, f.cardIDs(0))
}

func TestCardArchiveRemovesTheCard(t *testing.T) {
	f := newViewFixture(t)
	assert.True(t, f.view.apply(op(&boardcontent.CardArchive{CardID: f.cards[1]})))
	assert.Equal(t, []uuid.UUID{f.cards[0], f.cards[2]}, f.cardIDs(0))
	assert.False(t, f.view.apply(op(&boardcontent.CardArchive{CardID: f.cards[1]})))
	assert.False(t, f.view.apply(op(&boardcontent.CardMove{CardID: f.cards[1], ToColumnID: f.cols[1], BaseVersion: ptr(int64(1))})))
}

func TestUnknownPayloadTypesAreRejected(t *testing.T) {
	f := newViewFixture(t)
	assert.False(t, f.view.apply(op("string")))
	assert.False(t, f.view.apply(op(boardcontent.CardArchive{CardID: f.cards[0]})))
}

func TestPendingOpRawMarshalsThePayload(t *testing.T) {
	id := uuid.New()
	raw, err := PendingOp{Payload: &boardcontent.CardArchive{CardID: id}}.raw()
	require.NoError(t, err)
	var got boardcontent.CardArchive
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, id, got.CardID)
}

func TestGeneratorIsDeterministicForASeed(t *testing.T) {
	run := func(seed int64) []string {
		rng := rand.New(rand.NewSource(seed))
		g := &generator{rng: rng, name: "g", profile: Profile{MaxColumns: 5}}
		v := &view{}
		var out []string
		for range 400 {
			o := g.next(v)
			require.True(t, v.apply(o))
			raw, err := o.raw()
			require.NoError(t, err)
			out = append(out, o.ID.String()+o.Type+string(raw))
		}
		return out
	}
	assert.Equal(t, run(42), run(42))
	assert.NotEqual(t, run(42), run(43))
}

func TestEveryGeneratedOperationAppliesToTheOptimisticView(t *testing.T) {
	for _, profile := range []Profile{{MaxColumns: 6}, {MaxColumns: 2}, {MaxColumns: 6, HotSpot: 0.9}, {MaxColumns: 6, HotSpot: 1}} {
		g := &generator{rng: rand.New(rand.NewSource(7)), name: "g", profile: profile}
		v := &view{}
		types := map[string]int{}
		for range 4000 {
			o := g.next(v)
			require.True(t, v.apply(o), "op %s must apply to the view it was generated from", o.Type)
			types[o.Type]++
			assert.LessOrEqual(t, len(v.cols), max(profile.MaxColumns, 4))
		}
		assert.Positive(t, types[boardcontent.OpCardCreate])
		if profile.HotSpot < 1 {
			assert.Positive(t, types[boardcontent.OpCardMove])
			assert.Positive(t, types[boardcontent.OpCardEdit])
			assert.Positive(t, types[boardcontent.OpColumnMove])
		}
	}
}

func TestHotSpotCreatesAlwaysTargetTheFirstColumn(t *testing.T) {
	g := &generator{rng: rand.New(rand.NewSource(3)), name: "g", profile: Profile{MaxColumns: 4, HotSpot: 1}}
	v := &view{}
	first := g.next(v)
	require.True(t, v.apply(first))
	require.Equal(t, boardcontent.OpColumnCreate, first.Type)
	target := v.cols[0].ID
	for range 50 {
		o := g.next(v)
		require.Equal(t, boardcontent.OpCardCreate, o.Type)
		require.True(t, v.apply(o))
		assert.Equal(t, target, o.Payload.(*boardcontent.CardCreate).ColumnID)
	}
}

func TestGeneratedIdentifiersAreUniqueVersion4UUIDs(t *testing.T) {
	g := &generator{rng: rand.New(rand.NewSource(1)), name: "g"}
	seen := map[uuid.UUID]bool{}
	for range 2000 {
		id := g.newID()
		assert.Equal(t, uuid.Version(4), id.Version())
		assert.Equal(t, uuid.RFC4122, id.Variant())
		assert.False(t, seen[id])
		seen[id] = true
	}
}
