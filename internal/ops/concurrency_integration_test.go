package ops_test

import (
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/rank"
)

func TestFiftyConcurrentMovesOfOneCardYieldExactlyOneWinner(t *testing.T) {
	f := newConcurrentFixture(t)
	cols := []uuid.UUID{f.column(t, "A", nil), f.column(t, "B", nil), f.column(t, "C", nil)}
	card := f.card(t, cols[0], "contested")
	for i := range 3 {
		f.card(t, cols[i], fmt.Sprintf("neighbour-%d", i))
	}
	seqBefore := f.boardSeq(t)
	publishedBefore := len(f.pub.types())
	cardBefore := f.cardRow(t, card)
	require.Equal(t, int64(1), cardBefore.Version)

	const workers = 50
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardMove, uuid.New(), protocol.CardMove{
			CardID: card, ToColumnID: cols[i%len(cols)], BaseVersion: ptr(int64(1)),
		})
	})

	var winners []domain.Event
	conflicts := 0
	for _, r := range results {
		switch {
		case r.out.Event != nil:
			require.Nil(t, r.out.Nack)
			winners = append(winners, *r.out.Event)
		case r.out.Nack != nil:
			assert.Equal(t, protocol.ReasonVersionConflict, r.out.Nack.Reason)
			assert.GreaterOrEqual(t, r.out.Nack.AtSeq, seqBefore+1, "a loser's nack is evaluated after the winner committed")
			assert.False(t, r.out.Replayed)
			conflicts++
		default:
			t.Fatal("outcome has neither event nor nack")
		}
	}
	require.Len(t, winners, 1, "exactly one concurrent move may succeed")
	assert.Equal(t, workers-1, conflicts)

	win := winners[0]
	assert.Equal(t, protocol.EventCardMoved, win.Type)
	assert.Equal(t, seqBefore+1, win.Seq)

	moved := decode[protocol.CardMoved](t, win)
	final := f.cardRow(t, card)
	assert.Equal(t, int64(2), final.Version, "version is bumped exactly once")
	assert.Equal(t, moved.ToColumnID, final.ColumnID)
	assert.Equal(t, moved.Rank, final.Rank)
	assert.Equal(t, int64(2), moved.Version)

	assert.Equal(t, seqBefore+1, f.requireGapFreeSeq(t))
	assert.Len(t, f.pub.types(), publishedBefore+1, "only the winning move is published")

	events, nacks := f.processedOps(t)
	assert.Equal(t, int(seqBefore)+1, events)
	assert.Equal(t, workers-1, nacks)

	assertColumnsHaveUniqueSortedRanks(t, f)
}

func TestConcurrentColumnMovesWithOneBaseVersion(t *testing.T) {
	f := newConcurrentFixture(t)
	target := f.column(t, "target", nil)
	other := f.column(t, "other", nil)

	const workers = 20
	results := f.applyConcurrently(t, workers, func(int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpColumnMove, uuid.New(), protocol.ColumnMove{
			ColumnID: target, AfterID: &other, BaseVersion: ptr(int64(1)),
		})
	})
	wins, conflicts := countOutcomes(t, results, protocol.ReasonVersionConflict)
	assert.Equal(t, 1, wins)
	assert.Equal(t, workers-1, conflicts)
	assert.Equal(t, int64(2), f.columnRow(t, target).Version)
	assert.Equal(t, []uuid.UUID{other, target}, f.columnOrder(t))
	f.requireGapFreeSeq(t)
}

func countOutcomes(t *testing.T, results []result, nackReason string) (wins, nacks int) {
	t.Helper()
	for _, r := range results {
		switch {
		case r.out.Event != nil:
			wins++
		case r.out.Nack != nil:
			assert.Equal(t, nackReason, r.out.Nack.Reason)
			nacks++
		}
	}
	return wins, nacks
}

func assertColumnsHaveUniqueSortedRanks(t *testing.T, f *fixture) {
	t.Helper()
	s := f.snapshot(t)
	colRanks := make([]string, 0, len(s.Columns))
	for _, c := range s.Columns {
		colRanks = append(colRanks, c.Rank)
	}
	assertStrictlyIncreasing(t, colRanks, "column ranks")
	byColumn := map[uuid.UUID][]string{}
	for _, c := range s.Cards {
		byColumn[c.ColumnID] = append(byColumn[c.ColumnID], c.Rank)
	}
	for col, ranks := range byColumn {
		assertStrictlyIncreasing(t, ranks, "card ranks in column "+col.String())
		for _, r := range ranks {
			assert.NoError(t, rank.Validate(r), r)
			assert.LessOrEqual(t, len(r), rank.MaxLength)
		}
	}
}

func assertStrictlyIncreasing(t *testing.T, ranks []string, what string) {
	t.Helper()
	for i := 1; i < len(ranks); i++ {
		if ranks[i-1] >= ranks[i] {
			t.Errorf("%s are not strictly increasing: %q then %q", what, ranks[i-1], ranks[i])
			return
		}
	}
}

func TestParallelCardInsertsAtOneAnchorAllSucceedInSerializedOrder(t *testing.T) {
	f := newConcurrentFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "first")
	last := f.card(t, col, "last")

	const workers = 40
	ids := make([]uuid.UUID, workers)
	for i := range ids {
		ids[i] = uuid.New()
	}
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{
			CardID: ids[i], ColumnID: col, Title: fmt.Sprintf("n%d", i), AfterID: &first, BeforeID: &last,
		})
	})

	seqOf := map[uuid.UUID]int64{}
	for i, r := range results {
		require.Nil(t, r.out.Nack, "insert %d", i)
		require.NotNil(t, r.out.Event)
		seqOf[ids[i]] = r.out.Event.Seq
	}

	order := f.cardsInColumn(t, col)
	require.Len(t, order, workers+2)
	assert.Equal(t, first, order[0])
	assert.Equal(t, last, order[len(order)-1])

	inserted := order[1 : len(order)-1]
	wantNewestFirst := append([]uuid.UUID(nil), ids...)
	sort.Slice(wantNewestFirst, func(i, j int) bool { return seqOf[wantNewestFirst[i]] > seqOf[wantNewestFirst[j]] })
	assert.Equal(t, wantNewestFirst, inserted, "each insert lands directly after the anchor, so the last committed insert is closest to it")

	assertColumnsHaveUniqueSortedRanks(t, f)
	f.requireGapFreeSeq(t)
}

func TestParallelCardInsertsAtTheEndKeepCommitOrder(t *testing.T) {
	f := newConcurrentFixture(t)
	col := f.column(t, "Todo", nil)

	const workers = 40
	ids := make([]uuid.UUID, workers)
	for i := range ids {
		ids[i] = uuid.New()
	}
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{
			CardID: ids[i], ColumnID: col, Title: fmt.Sprintf("n%d", i),
		})
	})
	seqOf := map[uuid.UUID]int64{}
	for i, r := range results {
		require.Nil(t, r.out.Nack)
		seqOf[ids[i]] = r.out.Event.Seq
	}
	want := append([]uuid.UUID(nil), ids...)
	sort.Slice(want, func(i, j int) bool { return seqOf[want[i]] < seqOf[want[j]] })
	assert.Equal(t, want, f.cardsInColumn(t, col))
	assertColumnsHaveUniqueSortedRanks(t, f)
}

func TestParallelInsertsAtTheFrontKeepReverseCommitOrder(t *testing.T) {
	f := newConcurrentFixture(t)
	col := f.column(t, "Todo", nil)
	anchor := f.card(t, col, "anchor")

	const workers = 30
	ids := make([]uuid.UUID, workers)
	for i := range ids {
		ids[i] = uuid.New()
	}
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{
			CardID: ids[i], ColumnID: col, Title: fmt.Sprintf("n%d", i), BeforeID: &anchor,
		})
	})
	seqOf := map[uuid.UUID]int64{}
	for i, r := range results {
		require.Nil(t, r.out.Nack)
		seqOf[ids[i]] = r.out.Event.Seq
	}
	order := f.cardsInColumn(t, col)
	require.Len(t, order, workers+1)
	assert.Equal(t, anchor, order[len(order)-1])
	want := append([]uuid.UUID(nil), ids...)
	sort.Slice(want, func(i, j int) bool { return seqOf[want[i]] < seqOf[want[j]] })
	assert.Equal(t, want, order[:workers], "each insert lands directly before the anchor, so earlier commits stay further from it")
	assertColumnsHaveUniqueSortedRanks(t, f)
}

func TestParallelColumnInsertsAtOneAnchor(t *testing.T) {
	f := newConcurrentFixture(t)
	first := f.column(t, "first", nil)
	last := f.column(t, "last", nil)

	const workers = 25
	ids := make([]uuid.UUID, workers)
	for i := range ids {
		ids[i] = uuid.New()
	}
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{
			ColumnID: ids[i], Title: fmt.Sprintf("c%d", i), AfterID: &first, BeforeID: &last,
		})
	})
	seqOf := map[uuid.UUID]int64{}
	for i, r := range results {
		require.Nil(t, r.out.Nack)
		seqOf[ids[i]] = r.out.Event.Seq
	}
	order := f.columnOrder(t)
	require.Len(t, order, workers+2)
	assert.Equal(t, first, order[0])
	assert.Equal(t, last, order[len(order)-1])
	want := append([]uuid.UUID(nil), ids...)
	sort.Slice(want, func(i, j int) bool { return seqOf[want[i]] > seqOf[want[j]] })
	assert.Equal(t, want, order[1:len(order)-1])
	assertColumnsHaveUniqueSortedRanks(t, f)
	f.requireGapFreeSeq(t)
}

func TestConcurrentMovesOfDifferentCardsIntoOneSlot(t *testing.T) {
	f := newConcurrentFixture(t)
	src := f.column(t, "src", nil)
	dst := f.column(t, "dst", nil)
	anchor := f.card(t, dst, "anchor")

	const workers = 20
	cards := make([]uuid.UUID, workers)
	for i := range cards {
		cards[i] = f.card(t, src, fmt.Sprintf("c%d", i))
	}
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardMove, uuid.New(), protocol.CardMove{
			CardID: cards[i], ToColumnID: dst, AfterID: &anchor, BaseVersion: ptr(int64(1)),
		})
	})
	for _, r := range results {
		require.Nil(t, r.out.Nack)
	}
	assert.Empty(t, f.cardsInColumn(t, src))
	assert.Len(t, f.cardsInColumn(t, dst), workers+1)
	assertColumnsHaveUniqueSortedRanks(t, f)
	f.requireGapFreeSeq(t)
}

func TestMixedConcurrentOperationsConvergeToTheEventLog(t *testing.T) {
	f := newConcurrentFixture(t)
	editors := []domain.User{f.member(t, domain.RoleEditor), f.member(t, domain.RoleEditor), f.member(t, domain.RoleEditor)}
	users := append([]uuid.UUID{f.owner.ID}, editors[0].ID, editors[1].ID, editors[2].ID)

	cols := make([]uuid.UUID, 4)
	for i := range cols {
		cols[i] = f.column(t, fmt.Sprintf("col-%d", i), nil)
	}
	cards := make([]uuid.UUID, 12)
	for i := range cards {
		cards[i] = f.card(t, cols[i%len(cols)], fmt.Sprintf("card-%d", i))
	}
	baseSeq := f.boardSeq(t)

	const workers = 8
	const perWorker = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, workers*perWorker)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := range perWorker {
				user := users[(w+i)%len(users)]
				card := cards[(w*7+i*5)%len(cards)]
				col := cols[(w+i*3)%len(cols)]
				var op protocol.Op
				switch (w + i) % 6 {
				case 0:
					op = mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: fmt.Sprintf("new-%d-%d", w, i), AfterID: &card})
				case 1:
					op = mustOp(t, protocol.OpCardEdit, uuid.New(), protocol.CardEdit{CardID: card, Title: ptr(fmt.Sprintf("t-%d-%d", w, i))})
				case 2:
					op = mustOp(t, protocol.OpCardMove, uuid.New(), protocol.CardMove{CardID: card, ToColumnID: col, BeforeID: &cards[(w+i)%len(cards)], BaseVersion: ptr(int64(1 + (w+i)%3))})
				case 3:
					op = mustOp(t, protocol.OpColumnRename, uuid.New(), protocol.ColumnRename{ColumnID: col, Title: fmt.Sprintf("r-%d-%d", w, i)})
				case 4:
					op = mustOp(t, protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{ColumnID: uuid.New(), Title: fmt.Sprintf("nc-%d-%d", w, i), AfterID: &col})
				default:
					op = mustOp(t, protocol.OpCardEdit, uuid.New(), protocol.CardEdit{CardID: card, Description: ptr(fmt.Sprintf("d-%d-%d", w, i)), BaseDescriptionVersion: ptr(int64(1 + (w+i)%2))})
				}
				_, err := f.proc.Apply(t.Context(), user, f.board.ID, op)
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	finalSeq := f.requireGapFreeSeq(t)
	assert.Greater(t, finalSeq, baseSeq)
	assertColumnsHaveUniqueSortedRanks(t, f)

	model := newModel()
	for _, ev := range f.eventsAfter(t, 0) {
		model.apply(t, ev)
	}
	model.requireMatches(t, f.snapshot(t))
}

type modelCard struct {
	column             uuid.UUID
	title, description string
	rank               string
	version            int64
	descVersion        int64
}

type modelColumn struct {
	title   string
	rank    string
	version int64
}

type model struct {
	columns map[uuid.UUID]*modelColumn
	cards   map[uuid.UUID]*modelCard
}

func newModel() *model {
	return &model{columns: map[uuid.UUID]*modelColumn{}, cards: map[uuid.UUID]*modelCard{}}
}

func (m *model) apply(t *testing.T, ev domain.Event) {
	t.Helper()
	switch ev.Type {
	case protocol.EventColumnCreated:
		p := decode[protocol.ColumnCreated](t, ev)
		m.columns[p.ID] = &modelColumn{title: p.Title, rank: p.Rank, version: p.Version}
	case protocol.EventColumnRenamed:
		p := decode[protocol.ColumnRenamed](t, ev)
		c := m.columns[p.ColumnID]
		c.title, c.version = p.Title, p.Version
	case protocol.EventColumnMoved:
		p := decode[protocol.ColumnMoved](t, ev)
		c := m.columns[p.ColumnID]
		c.rank, c.version = p.Rank, p.Version
	case protocol.EventColumnArchived:
		p := decode[protocol.ColumnArchived](t, ev)
		delete(m.columns, p.ColumnID)
		for _, id := range p.CardIDs {
			delete(m.cards, id)
		}
	case protocol.EventColumnsRebalanced:
		p := decode[protocol.ColumnsRebalanced](t, ev)
		for _, r := range p.Columns {
			m.columns[r.ColumnID].rank = r.Rank
		}
	case protocol.EventCardCreated:
		p := decode[protocol.CardCreated](t, ev)
		m.cards[p.ID] = &modelCard{column: p.ColumnID, title: p.Title, description: p.Description, rank: p.Rank, version: p.Version, descVersion: p.DescriptionVersion}
	case protocol.EventCardEdited:
		p := decode[protocol.CardEdited](t, ev)
		c := m.cards[p.CardID]
		if p.Title != nil {
			c.title = *p.Title
		}
		if p.Description != nil {
			c.description = *p.Description
		}
		c.version, c.descVersion = p.Version, p.DescriptionVersion
	case protocol.EventCardMoved:
		p := decode[protocol.CardMoved](t, ev)
		c := m.cards[p.CardID]
		c.column, c.rank, c.version = p.ToColumnID, p.Rank, p.Version
	case protocol.EventCardArchived:
		p := decode[protocol.CardArchived](t, ev)
		delete(m.cards, p.CardID)
	case protocol.EventColumnRebalanced:
		p := decode[protocol.ColumnRebalanced](t, ev)
		for _, r := range p.Cards {
			m.cards[r.CardID].rank = r.Rank
		}
	default:
		t.Fatalf("unexpected event type %s", ev.Type)
	}
}

func (m *model) requireMatches(t *testing.T, s domain.BoardSnapshot) {
	t.Helper()
	require.Len(t, s.Columns, len(m.columns))
	for _, c := range s.Columns {
		mc, ok := m.columns[c.ID]
		require.True(t, ok, "column %s missing from the replayed log", c.ID)
		assert.Equal(t, mc.title, c.Title)
		assert.Equal(t, mc.rank, c.Rank)
		assert.Equal(t, mc.version, c.Version)
	}
	require.Len(t, s.Cards, len(m.cards))
	for _, c := range s.Cards {
		mc, ok := m.cards[c.ID]
		require.True(t, ok, "card %s missing from the replayed log", c.ID)
		assert.Equal(t, mc.column, c.ColumnID)
		assert.Equal(t, mc.title, c.Title)
		assert.Equal(t, mc.description, c.Description)
		assert.Equal(t, mc.rank, c.Rank)
		assert.Equal(t, mc.version, c.Version)
		assert.Equal(t, mc.descVersion, c.DescriptionVersion)
	}
}
