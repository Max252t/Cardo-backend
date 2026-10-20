package ops_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/rank"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

func TestDescriptionOptimisticLockBetweenTwoEditors(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")
	alice := f.member(t, domain.RoleEditor)
	bob := f.member(t, domain.RoleEditor)

	out := f.applyAs(t, alice.ID, uuid.New(), protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("alice"), BaseDescriptionVersion: ptr(int64(1))})
	require.NotNil(t, out.Event)
	edited := decode[protocol.CardEdited](t, *out.Event)
	assert.Equal(t, int64(2), edited.DescriptionVersion)
	assert.Equal(t, int64(2), edited.Version)
	assert.Equal(t, alice.ID, edited.ActorID)

	stale := f.applyAs(t, bob.ID, uuid.New(), protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("bob"), BaseDescriptionVersion: ptr(int64(1))})
	require.NotNil(t, stale.Nack)
	assert.Equal(t, protocol.ReasonVersionConflict, stale.Nack.Reason)
	assert.Equal(t, "alice", f.cardRow(t, card).Description)

	retry := f.applyAs(t, bob.ID, uuid.New(), protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("bob wins now"), BaseDescriptionVersion: ptr(int64(2))})
	require.NotNil(t, retry.Event)
	state := f.cardRow(t, card)
	assert.Equal(t, "bob wins now", state.Description)
	assert.Equal(t, int64(3), state.DescriptionVersion)
	assert.Equal(t, int64(3), state.Version)
	assert.Equal(t, bob.ID, state.UpdatedBy)
}

func TestTitleEditsDoNotTouchDescriptionVersion(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")
	f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("text"), BaseDescriptionVersion: ptr(int64(1))})

	for i := range 3 {
		ev := f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr(fmt.Sprintf("title %d", i))})
		edited := decode[protocol.CardEdited](t, ev)
		assert.Equal(t, int64(2), edited.DescriptionVersion)
		assert.Nil(t, edited.Description, "unchanged fields are omitted from the event")
		assert.Equal(t, int64(3+i), edited.Version)
	}
	state := f.cardRow(t, card)
	assert.Equal(t, "text", state.Description)
	assert.Equal(t, int64(2), state.DescriptionVersion)
}

func TestEditWithTitleAndDescriptionAppliesBothAtomically(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")

	ev := f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr("both"), Description: ptr("both text"), BaseDescriptionVersion: ptr(int64(1))})
	edited := decode[protocol.CardEdited](t, ev)
	require.NotNil(t, edited.Title)
	require.NotNil(t, edited.Description)
	assert.Equal(t, "both", *edited.Title)
	assert.Equal(t, "both text", *edited.Description)
	assert.Equal(t, int64(2), edited.Version, "one op bumps version once")
	assert.Equal(t, int64(2), edited.DescriptionVersion)
}

func TestDescriptionCanBeClearedAndKeepsWhitespace(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")
	f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("  line1\n\tline2  "), BaseDescriptionVersion: ptr(int64(1))})
	assert.Equal(t, "  line1\n\tline2  ", f.cardRow(t, card).Description)

	f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr(""), BaseDescriptionVersion: ptr(int64(2))})
	state := f.cardRow(t, card)
	assert.Empty(t, state.Description)
	assert.Equal(t, int64(3), state.DescriptionVersion)
}

func TestTitleIsTrimmedAndUnicodeLengthIsCountedInRunes(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	id := uuid.New()
	f.mustEvent(t, protocol.OpCardCreate, protocol.CardCreate{CardID: id, ColumnID: col, Title: "  " + strings.Repeat("я", 500) + "  "})
	assert.Equal(t, strings.Repeat("я", 500), f.cardRow(t, id).Title)
	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardEdit, protocol.CardEdit{CardID: id, Title: ptr(strings.Repeat("я", 501))})
}

func TestConcurrentTitleEditsAreLastWriterWins(t *testing.T) {
	f := newConcurrentFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "start")

	const workers = 20
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardEdit, uuid.New(), protocol.CardEdit{CardID: card, Title: ptr(fmt.Sprintf("title-%02d", i))})
	})
	var last domain.Event
	titles := map[int64]string{}
	for _, r := range results {
		require.Nil(t, r.out.Nack, "title edits never conflict")
		require.NotNil(t, r.out.Event)
		titles[r.out.Event.Seq] = *decode[protocol.CardEdited](t, *r.out.Event).Title
		if r.out.Event.Seq > last.Seq {
			last = *r.out.Event
		}
	}
	assert.Len(t, titles, workers)
	state := f.cardRow(t, card)
	assert.Equal(t, titles[last.Seq], state.Title, "the title committed last wins")
	assert.Equal(t, int64(1+workers), state.Version)
	assert.Equal(t, int64(1), state.DescriptionVersion)
	f.requireGapFreeSeq(t)
}

func TestConcurrentDescriptionEditsWithOneBaseVersionHaveOneWinner(t *testing.T) {
	f := newConcurrentFixture(t)
	editors := []domain.User{f.member(t, domain.RoleEditor), f.member(t, domain.RoleEditor), f.member(t, domain.RoleEditor)}
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")

	const workers = 15
	results := f.applyConcurrently(t, workers, func(i int) (uuid.UUID, protocol.Op) {
		return editors[i%len(editors)].ID, mustOp(t, protocol.OpCardEdit, uuid.New(), protocol.CardEdit{
			CardID: card, Description: ptr(fmt.Sprintf("draft-%d", i)), BaseDescriptionVersion: ptr(int64(1)),
		})
	})
	wins, conflicts := countOutcomes(t, results, protocol.ReasonVersionConflict)
	assert.Equal(t, 1, wins)
	assert.Equal(t, workers-1, conflicts)
	state := f.cardRow(t, card)
	assert.Equal(t, int64(2), state.DescriptionVersion)
	assert.True(t, strings.HasPrefix(state.Description, "draft-"))
}

func TestColumnArchiveCascadesToCards(t *testing.T) {
	f := newFixture(t)
	doomed := f.column(t, "Doomed", nil)
	keep := f.column(t, "Keep", nil)
	a := f.card(t, doomed, "A")
	b := f.card(t, doomed, "B")
	c := f.card(t, doomed, "C")
	kept := f.card(t, keep, "Kept")
	f.mustEvent(t, protocol.OpCardArchive, protocol.CardArchive{CardID: b})
	f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: c, Title: ptr("C2")})

	beforeA, beforeB, beforeC := f.cardRow(t, a), f.cardRow(t, b), f.cardRow(t, c)
	beforeKept, beforeKeepCol := f.cardRow(t, kept), f.columnRow(t, keep)
	beforeDoomed := f.columnRow(t, doomed)
	require.Equal(t, int64(1), beforeA.Version)
	require.Equal(t, int64(2), beforeB.Version)
	require.Equal(t, int64(2), beforeC.Version)

	editor := f.member(t, domain.RoleEditor)
	out := f.applyAs(t, editor.ID, uuid.New(), protocol.OpColumnArchive, protocol.ColumnArchive{ColumnID: doomed})
	require.NotNil(t, out.Event)
	assert.Empty(t, out.Extra)
	assert.Equal(t, protocol.EventColumnArchived, out.Event.Type)
	archived := decode[protocol.ColumnArchived](t, *out.Event)
	assert.Equal(t, doomed, archived.ColumnID)
	assert.ElementsMatch(t, []uuid.UUID{a, c}, archived.CardIDs, "only live cards are reported")
	assert.Equal(t, beforeDoomed.Version+1, archived.Version)
	assert.Equal(t, editor.ID, archived.ActorID)

	afterA, afterB, afterC := f.cardRow(t, a), f.cardRow(t, b), f.cardRow(t, c)
	assert.True(t, afterA.Archived)
	assert.True(t, afterC.Archived)
	assert.Equal(t, beforeA.Version+1, afterA.Version)
	assert.Equal(t, beforeC.Version+1, afterC.Version)
	assert.Equal(t, beforeB.Version, afterB.Version, "a card archived earlier is not touched again")
	assert.Equal(t, editor.ID, afterA.UpdatedBy)
	assert.Equal(t, beforeA.Rank, afterA.Rank)
	assert.Equal(t, beforeA.ColumnID, afterA.ColumnID)

	columnAfter := f.columnRow(t, doomed)
	assert.True(t, columnAfter.Archived)
	assert.Equal(t, beforeDoomed.Version+1, columnAfter.Version)

	assert.Equal(t, beforeKept, f.cardRow(t, kept))
	assert.Equal(t, beforeKeepCol, f.columnRow(t, keep))

	s := f.snapshot(t)
	assert.Equal(t, []uuid.UUID{keep}, []uuid.UUID{s.Columns[0].ID})
	require.Len(t, s.Cards, 1)
	assert.Equal(t, kept, s.Cards[0].ID)

	for _, id := range []uuid.UUID{a, c} {
		f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardEdit, protocol.CardEdit{CardID: id, Title: ptr("late")})
		f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardMove, protocol.CardMove{CardID: id, ToColumnID: keep, BaseVersion: ptr(int64(1))})
		f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardArchive, protocol.CardArchive{CardID: id})
	}
}

func TestEmptyColumnArchiveReportsAnEmptyCardList(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Empty", nil)
	ev := f.mustEvent(t, protocol.OpColumnArchive, protocol.ColumnArchive{ColumnID: col})
	assert.Contains(t, string(ev.Payload), `"card_ids":[]`)
	assert.Equal(t, int64(2), decode[protocol.ColumnArchived](t, ev).Version)
}

func TestColumnArchiveFreesRankSlotsForNewColumns(t *testing.T) {
	f := newFixture(t)
	first := f.column(t, "first", nil)
	second := f.column(t, "second", nil)
	f.mustEvent(t, protocol.OpColumnArchive, protocol.ColumnArchive{ColumnID: second})
	third := f.column(t, "third", nil)
	assert.Equal(t, []uuid.UUID{first, third}, f.columnOrder(t))
	assertColumnsHaveUniqueSortedRanks(t, f)
}

func TestCardArchiveBumpsVersionAndHidesFromSnapshot(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")
	other := f.card(t, col, "B")

	ev := f.mustEvent(t, protocol.OpCardArchive, protocol.CardArchive{CardID: card})
	archived := decode[protocol.CardArchived](t, ev)
	assert.Equal(t, card, archived.CardID)
	assert.Equal(t, col, archived.ColumnID)
	assert.Equal(t, int64(2), archived.Version)
	assert.Equal(t, []uuid.UUID{other}, f.cardsInColumn(t, col))
	assert.Equal(t, int64(1), f.cardRow(t, other).Version, "archiving a neighbour does not change other cards")
}

func TestCardMoveWithinColumnDoesNotChangeNeighbourVersions(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	a := f.card(t, col, "A")
	b := f.card(t, col, "B")
	c := f.card(t, col, "C")

	f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: a, ToColumnID: col, AfterID: &c, BaseVersion: ptr(int64(1))})
	assert.Equal(t, []uuid.UUID{b, c, a}, f.cardsInColumn(t, col))
	assert.Equal(t, int64(2), f.cardRow(t, a).Version)
	assert.Equal(t, int64(1), f.cardRow(t, b).Version)
	assert.Equal(t, int64(1), f.cardRow(t, c).Version)

	ev := f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: b, ToColumnID: col, AfterID: &a, BaseVersion: ptr(int64(1))})
	moved := decode[protocol.CardMoved](t, ev)
	assert.Equal(t, col, moved.FromColumnID)
	assert.Equal(t, col, moved.ToColumnID)
	assert.Equal(t, []uuid.UUID{c, a, b}, f.cardsInColumn(t, col))
}

func TestCardMovedEventCarriesTheStoredRank(t *testing.T) {
	f := newFixture(t)
	src := f.column(t, "src", nil)
	dst := f.column(t, "dst", nil)
	card := f.card(t, src, "A")
	f.card(t, dst, "B")

	ev := f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: card, ToColumnID: dst, BaseVersion: ptr(int64(1))})
	moved := decode[protocol.CardMoved](t, ev)
	state := f.cardRow(t, card)
	assert.Equal(t, state.Rank, moved.Rank)
	assert.Equal(t, dst, state.ColumnID)
	assert.NoError(t, rank.Validate(moved.Rank))
}

func TestCardRebalanceEmitsAnExtraEventWithoutBumpingVersions(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "first")
	middle := f.card(t, col, "middle")
	last := f.card(t, col, "last")
	f.setCardRank(t, first, "a0")
	f.setCardRank(t, middle, "a0"+strings.Repeat("0", 62)+"1")
	f.setCardRank(t, last, "a0"+strings.Repeat("0", 61)+"1")
	require.Greater(t, len(f.cardRow(t, middle).Rank), rank.MaxLength)

	versionsBefore := map[uuid.UUID]int64{first: 1, middle: 1, last: 1}
	seqBefore := f.boardSeq(t)
	publishedBefore := len(f.pub.batches)
	newCard := uuid.New()

	out := f.apply(t, protocol.OpCardCreate, protocol.CardCreate{CardID: newCard, ColumnID: col, Title: "wedge", AfterID: &middle, BeforeID: &last})
	require.Nil(t, out.Nack)
	require.NotNil(t, out.Event)
	require.Len(t, out.Extra, 1)

	primary, extra := *out.Event, out.Extra[0]
	assert.Equal(t, protocol.EventCardCreated, primary.Type)
	assert.Equal(t, protocol.EventColumnRebalanced, extra.Type)
	assert.Equal(t, seqBefore+1, extra.Seq, "the rebalance is appended before the primary event")
	assert.Equal(t, seqBefore+2, primary.Seq)
	require.NotNil(t, primary.OpID)
	assert.Nil(t, extra.OpID, "the rebalance is not the confirmation of the client op")
	require.NotNil(t, extra.ActorID)
	assert.Equal(t, f.owner.ID, *extra.ActorID)
	f.requireGapFreeSeq(t)

	created := decode[protocol.CardCreated](t, primary)
	rebalanced := decode[protocol.ColumnRebalanced](t, extra)
	assert.Equal(t, col, rebalanced.ColumnID)
	require.Len(t, rebalanced.Cards, 3, "the rebalance respaces only cards that already exist")
	assert.Equal(t, []uuid.UUID{first, middle, last}, []uuid.UUID{rebalanced.Cards[0].CardID, rebalanced.Cards[1].CardID, rebalanced.Cards[2].CardID})
	ranks := make([]string, len(rebalanced.Cards))
	for i, r := range rebalanced.Cards {
		ranks[i] = r.Rank
		assert.NoError(t, rank.Validate(r.Rank))
		assert.LessOrEqual(t, len(r.Rank), rank.MaxLength)
	}
	assertStrictlyIncreasing(t, ranks, "rebalanced ranks")
	assert.Less(t, ranks[1], created.Rank, "the new card lands after its anchor in the respaced list")
	assert.Less(t, created.Rank, ranks[2])
	assert.Equal(t, created.Rank, f.cardRow(t, newCard).Rank)

	for id, v := range versionsBefore {
		state := f.cardRow(t, id)
		assert.Equal(t, v, state.Version, "rebalancing must not bump card versions")
	}
	assert.Equal(t, []uuid.UUID{first, middle, newCard, last}, f.cardsInColumn(t, col))
	for _, r := range rebalanced.Cards {
		assert.Equal(t, r.Rank, f.cardRow(t, r.CardID).Rank)
	}

	require.Len(t, f.pub.batches, publishedBefore+1)
	batch := f.pub.batches[publishedBefore]
	require.Len(t, batch, 2)
	assert.Equal(t, []string{protocol.EventColumnRebalanced, protocol.EventCardCreated}, []string{batch[0].Type, batch[1].Type})
	assert.Equal(t, seqBefore+1, batch[0].Seq)
	assert.Equal(t, seqBefore+2, batch[1].Seq)
}

func TestCardMoveRebalanceKeepsVersionBumpToOne(t *testing.T) {
	f := newFixture(t)
	src := f.column(t, "src", nil)
	dst := f.column(t, "dst", nil)
	mover := f.card(t, src, "mover")
	x := f.card(t, dst, "x")
	y := f.card(t, dst, "y")
	f.setCardRank(t, x, "a0")
	f.setCardRank(t, y, "a0"+strings.Repeat("0", 61)+"1")

	out := f.apply(t, protocol.OpCardMove, protocol.CardMove{CardID: mover, ToColumnID: dst, AfterID: &x, BeforeID: &y, BaseVersion: ptr(int64(1))})
	require.Nil(t, out.Nack)
	require.Len(t, out.Extra, 1)
	assert.Equal(t, protocol.EventColumnRebalanced, out.Extra[0].Type)

	assert.Equal(t, int64(2), f.cardRow(t, mover).Version)
	assert.Equal(t, int64(1), f.cardRow(t, x).Version)
	assert.Equal(t, int64(1), f.cardRow(t, y).Version)
	assert.Equal(t, []uuid.UUID{x, mover, y}, f.cardsInColumn(t, dst))
	moved := decode[protocol.CardMoved](t, *out.Event)
	assert.Equal(t, f.cardRow(t, mover).Rank, moved.Rank)
}

func TestMoveWithinColumnWithRebalanceReranksTheMovedCardToo(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	a := f.card(t, col, "a")
	b := f.card(t, col, "b")
	c := f.card(t, col, "c")
	f.setCardRank(t, a, "a0")
	f.setCardRank(t, b, "a0"+strings.Repeat("0", 62)+"1")
	f.setCardRank(t, c, "a0"+strings.Repeat("0", 61)+"1")

	out := f.apply(t, protocol.OpCardMove, protocol.CardMove{CardID: c, ToColumnID: col, AfterID: &a, BeforeID: &b, BaseVersion: ptr(int64(1))})
	require.Nil(t, out.Nack)
	require.Len(t, out.Extra, 1)
	assert.Equal(t, []uuid.UUID{a, c, b}, f.cardsInColumn(t, col))
	assertColumnsHaveUniqueSortedRanks(t, f)
	assert.Equal(t, int64(2), f.cardRow(t, c).Version)
	assert.Equal(t, int64(1), f.cardRow(t, b).Version)
}

func TestInvalidStoredRanksTriggerRebalance(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	a := f.card(t, col, "a")
	b := f.card(t, col, "b")
	f.setCardRank(t, a, "a0")
	f.setCardRank(t, b, "not a rank")

	out := f.apply(t, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "c"})
	require.Nil(t, out.Nack)
	require.Len(t, out.Extra, 1)
	assertColumnsHaveUniqueSortedRanks(t, f)
	assert.Len(t, f.cardsInColumn(t, col), 3)
}

func TestColumnRebalanceEmitsColumnsRebalanced(t *testing.T) {
	f := newFixture(t)
	a := f.column(t, "a", nil)
	b := f.column(t, "b", nil)
	c := f.column(t, "c", nil)
	f.setColumnRank(t, a, "a0")
	f.setColumnRank(t, b, "a0"+strings.Repeat("0", 62)+"1")
	f.setColumnRank(t, c, "a0"+strings.Repeat("0", 61)+"1")
	versions := map[uuid.UUID]int64{a: 1, b: 1, c: 1}
	seqBefore := f.boardSeq(t)

	newCol := uuid.New()
	out := f.apply(t, protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: newCol, Title: "wedge", AfterID: &b, BeforeID: &c})
	require.Nil(t, out.Nack)
	require.Len(t, out.Extra, 1)
	extra := out.Extra[0]
	assert.Equal(t, protocol.EventColumnsRebalanced, extra.Type)
	assert.Nil(t, extra.OpID)
	assert.Equal(t, seqBefore+1, extra.Seq)
	assert.Equal(t, seqBefore+2, out.Event.Seq)

	rebalanced := decode[protocol.ColumnsRebalanced](t, extra)
	require.Len(t, rebalanced.Columns, 3)
	got := make([]uuid.UUID, len(rebalanced.Columns))
	ranks := make([]string, len(rebalanced.Columns))
	for i, r := range rebalanced.Columns {
		got[i], ranks[i] = r.ColumnID, r.Rank
		assert.Equal(t, r.Rank, f.columnRow(t, r.ColumnID).Rank)
	}
	assert.Equal(t, []uuid.UUID{a, b, c}, got)
	assertStrictlyIncreasing(t, ranks, "rebalanced column ranks")
	for id, v := range versions {
		assert.Equal(t, v, f.columnRow(t, id).Version, "rebalancing must not bump column versions")
	}
	assert.Equal(t, []uuid.UUID{a, b, newCol, c}, f.columnOrder(t))
}

func TestColumnMoveRebalance(t *testing.T) {
	f := newFixture(t)
	a := f.column(t, "a", nil)
	b := f.column(t, "b", nil)
	c := f.column(t, "c", nil)
	f.setColumnRank(t, a, "a0")
	f.setColumnRank(t, b, "a0"+strings.Repeat("0", 62)+"1")
	f.setColumnRank(t, c, "a0"+strings.Repeat("0", 61)+"1")

	out := f.apply(t, protocol.OpColumnMove, protocol.ColumnMove{ColumnID: c, AfterID: &a, BeforeID: &b, BaseVersion: ptr(int64(1))})
	require.Nil(t, out.Nack)
	require.Len(t, out.Extra, 1)
	assert.Equal(t, int64(2), f.columnRow(t, c).Version)
	assert.Equal(t, int64(1), f.columnRow(t, b).Version)
	assert.Equal(t, []uuid.UUID{a, c, b}, f.columnOrder(t))
}

func TestRebalanceIsNotTriggeredWhenRankFits(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	a := f.card(t, col, "a")
	b := f.card(t, col, "b")
	for range 20 {
		out := f.apply(t, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "n", AfterID: &a, BeforeID: &b})
		require.Nil(t, out.Nack)
		assert.Empty(t, out.Extra)
	}
}

func TestAnchorFallbackRulesForCardCreate(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	other := f.column(t, "Other", nil)
	a := f.card(t, col, "A")
	b := f.card(t, col, "B")
	c := f.card(t, col, "C")
	foreign := f.card(t, other, "Foreign")
	archived := f.card(t, col, "Archived")
	f.mustEvent(t, protocol.OpCardArchive, protocol.CardArchive{CardID: archived})
	ghost := uuid.New()

	titles := func() []string { return cardTitles(f.snapshot(t), col) }
	create := func(title string, after, before *uuid.UUID) {
		f.mustEvent(t, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: title, AfterID: after, BeforeID: before})
	}

	create("after-a", &a, nil)
	assert.Equal(t, []string{"A", "after-a", "B", "C"}, titles())

	create("before-c", nil, &c)
	assert.Equal(t, []string{"A", "after-a", "B", "before-c", "C"}, titles())

	create("both-valid-after-wins", &b, &a)
	assert.Equal(t, []string{"A", "after-a", "B", "both-valid-after-wins", "before-c", "C"}, titles())

	create("ghost-after-uses-before", &ghost, &a)
	assert.Equal(t, []string{"ghost-after-uses-before", "A", "after-a", "B", "both-valid-after-wins", "before-c", "C"}, titles())

	create("ghost-before-uses-after", &c, &ghost)
	assert.Equal(t, "ghost-before-uses-after", titles()[len(titles())-1])

	create("ghost-both-goes-last", &ghost, &ghost)
	assert.Equal(t, "ghost-both-goes-last", titles()[len(titles())-1])

	create("no-anchors-goes-last", nil, nil)
	assert.Equal(t, "no-anchors-goes-last", titles()[len(titles())-1])

	create("anchor-in-other-column", &foreign, nil)
	assert.Equal(t, "anchor-in-other-column", titles()[len(titles())-1], "an anchor from another column is treated as missing")

	create("archived-anchor", &archived, nil)
	assert.Equal(t, "archived-anchor", titles()[len(titles())-1], "an archived anchor is treated as missing")

	create("nil-uuid-anchor", &uuid.Nil, &uuid.Nil)
	assert.Equal(t, "nil-uuid-anchor", titles()[len(titles())-1])

	assertColumnsHaveUniqueSortedRanks(t, f)
}

func TestAnchorFallbackRulesForCardMove(t *testing.T) {
	f := newFixture(t)
	src := f.column(t, "src", nil)
	dst := f.column(t, "dst", nil)
	x := f.card(t, dst, "x")
	y := f.card(t, dst, "y")
	mover := f.card(t, src, "mover")
	version := int64(1)
	ghost := uuid.New()

	move := func(after, before *uuid.UUID, to uuid.UUID) {
		f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: mover, ToColumnID: to, AfterID: after, BeforeID: before, BaseVersion: &version})
		version++
	}

	move(&ghost, &y, dst)
	assert.Equal(t, []string{"x", "mover", "y"}, cardTitles(f.snapshot(t), dst))

	move(&x, &ghost, dst)
	assert.Equal(t, []string{"x", "mover", "y"}, cardTitles(f.snapshot(t), dst))

	move(&ghost, &ghost, dst)
	assert.Equal(t, []string{"x", "y", "mover"}, cardTitles(f.snapshot(t), dst))

	move(nil, &x, dst)
	assert.Equal(t, []string{"mover", "x", "y"}, cardTitles(f.snapshot(t), dst))

	n := f.mustNack(t, protocol.ReasonValidation, protocol.OpCardMove, protocol.CardMove{CardID: mover, ToColumnID: dst, AfterID: &mover, BaseVersion: &version})
	assert.Contains(t, n.Message, "after_id")
	n = f.mustNack(t, protocol.ReasonValidation, protocol.OpCardMove, protocol.CardMove{CardID: mover, ToColumnID: dst, BeforeID: &mover, BaseVersion: &version})
	assert.Contains(t, n.Message, "before_id")
	assert.Equal(t, []string{"mover", "x", "y"}, cardTitles(f.snapshot(t), dst), "a rejected self anchor leaves the order untouched")

	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardMove, protocol.CardMove{CardID: mover, ToColumnID: dst, AfterID: &mover, BeforeID: &y, BaseVersion: &version})
	move(&x, &y, dst)
	assert.Equal(t, []string{"x", "mover", "y"}, cardTitles(f.snapshot(t), dst))

	move(&y, nil, src)
	assert.Equal(t, []string{"mover"}, cardTitles(f.snapshot(t), src))
	assert.Equal(t, []string{"x", "y"}, cardTitles(f.snapshot(t), dst))
}

func TestAnchorFallbackRulesForColumns(t *testing.T) {
	f := newFixture(t)
	a := f.column(t, "A", nil)
	b := f.column(t, "B", nil)
	c := f.column(t, "C", nil)
	archived := f.column(t, "Archived", nil)
	f.mustEvent(t, protocol.OpColumnArchive, protocol.ColumnArchive{ColumnID: archived})
	ghost := uuid.New()
	titles := func() []string {
		var out []string
		for _, col := range f.snapshot(t).Columns {
			out = append(out, col.Title)
		}
		return out
	}
	create := func(title string, after, before *uuid.UUID) uuid.UUID {
		id := uuid.New()
		f.mustEvent(t, protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: id, Title: title, AfterID: after, BeforeID: before})
		return id
	}

	create("after-a", &a, nil)
	assert.Equal(t, []string{"A", "after-a", "B", "C"}, titles())
	create("before-c", nil, &c)
	assert.Equal(t, []string{"A", "after-a", "B", "before-c", "C"}, titles())
	create("ghost-after", &ghost, &a)
	assert.Equal(t, "ghost-after", titles()[0])
	create("ghost-both", &ghost, &ghost)
	assert.Equal(t, "ghost-both", titles()[len(titles())-1])
	create("archived-anchor", &archived, nil)
	assert.Equal(t, "archived-anchor", titles()[len(titles())-1])

	version := f.columnRow(t, b).Version
	before := titles()
	n := f.mustNack(t, protocol.ReasonValidation, protocol.OpColumnMove, protocol.ColumnMove{ColumnID: b, AfterID: &b, BaseVersion: &version})
	assert.Contains(t, n.Message, "after_id")
	assert.Equal(t, before, titles(), "a self anchor is rejected without moving the column")
	assertColumnsHaveUniqueSortedRanks(t, f)
}

func TestPublisherRunsAfterCommitAndSeesDurableEvents(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var observed []struct {
		seqs    []int64
		visible int
	}
	pub := pubsub.PublisherFunc(func(ctx context.Context, events []domain.Event) error {
		var visible int
		err := f.pool.QueryRow(ctx, `SELECT count(*) FROM board_events WHERE board_id = $1 AND seq = ANY($2)`, f.board.ID, seqsOf(events)).Scan(&visible)
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, struct {
			seqs    []int64
			visible int
		}{seqsOf(events), visible})
		return nil
	})
	proc := ops.NewProcessor(f.store, ops.WithPublisher(pub))
	f.proc = proc

	col := f.column(t, "Todo", nil)
	f.card(t, col, "A")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, observed, 2)
	for _, o := range observed {
		assert.Equal(t, len(o.seqs), o.visible, "every published event is already committed and visible to other connections")
	}
}

func seqsOf(events []domain.Event) []int64 {
	out := make([]int64, len(events))
	for i, ev := range events {
		out[i] = ev.Seq
	}
	return out
}

func TestPublisherIsSkippedForNacksReplaysAndErrors(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")
	viewer := f.member(t, domain.RoleViewer)
	published := len(f.pub.batches)

	f.mustNack(t, protocol.ReasonVersionConflict, protocol.OpCardMove, protocol.CardMove{CardID: card, ToColumnID: col, BaseVersion: ptr(int64(9))})
	f.applyAs(t, viewer.ID, uuid.New(), protocol.OpCardArchive, protocol.CardArchive{CardID: card})
	f.applyRaw(t, f.owner.ID, protocol.OpCardArchive, `{"bogus":1}`)
	assert.Len(t, f.pub.batches, published, "nacks are never published")

	opID := uuid.New()
	f.applyAs(t, f.owner.ID, opID, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr("once")})
	require.Len(t, f.pub.batches, published+1)
	for range 3 {
		f.applyAs(t, f.owner.ID, opID, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr("once")})
	}
	assert.Len(t, f.pub.batches, published+1, "replays are never republished")

	_, err := f.proc.Apply(context.Background(), f.owner.ID, uuid.New(), mustOp(t, protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{ColumnID: uuid.New(), Title: "x"}))
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Len(t, f.pub.batches, published+1, "failures are never published")
}

func TestPublisherIsSkippedWhenTheBoardLockTimesOut(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	store := storage.New(f.pool, storage.WithLockTimeout(100*time.Millisecond))
	pub := &recorder{}
	proc := ops.NewProcessor(store, ops.WithPublisher(pub))

	ctx := context.Background()
	holder, err := f.pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback(context.Background()) })
	_, err = holder.Exec(ctx, `SELECT id FROM boards WHERE id = $1 FOR UPDATE`, f.board.ID)
	require.NoError(t, err)

	op := mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "blocked"})
	_, err = proc.Apply(ctx, f.owner.ID, f.board.ID, op)
	require.ErrorIs(t, err, domain.ErrBusy)
	assert.Empty(t, pub.batches)

	require.NoError(t, holder.Rollback(ctx))
	out, err := proc.Apply(ctx, f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	require.NotNil(t, out.Event)
	assert.False(t, out.Replayed, "a timed out attempt leaves no processed op behind")
	assert.Len(t, pub.batches, 1)
}

func TestPublisherFailureDoesNotFailTheOperation(t *testing.T) {
	f := newFixture(t)
	var calls int
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	pub := pubsub.PublisherFunc(func(ctx context.Context, _ []domain.Event) error {
		calls++
		assert.NoError(t, ctx.Err())
		return errors.New("redis down")
	})
	proc := ops.NewProcessor(f.store, ops.WithPublisher(pub), ops.WithLogger(logger))

	colID := uuid.New()
	op := mustOp(t, protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{ColumnID: colID, Title: "Todo"})
	out, err := proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	require.NotNil(t, out.Event)
	assert.Equal(t, 1, calls)
	assert.Contains(t, logs.String(), "publish board events failed")
	assert.Contains(t, logs.String(), "redis down")
	assert.Equal(t, []uuid.UUID{colID}, f.columnOrder(t))

	again, err := proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, 1, calls)
}

func TestPublisherGetsAContextThatSurvivesCancellation(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	var ctxErr error
	pub := pubsub.PublisherFunc(func(pctx context.Context, _ []domain.Event) error {
		cancel()
		ctxErr = pctx.Err()
		return nil
	})
	proc := ops.NewProcessor(f.store, ops.WithPublisher(pub))
	op := mustOp(t, protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{ColumnID: uuid.New(), Title: "x"})
	_, err := proc.Apply(ctx, f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	assert.NoError(t, ctxErr)
}

func TestCanceledContextAppliesNothing(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	seq := f.boardSeq(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	op := mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	_, err := f.proc.Apply(ctx, f.owner.ID, f.board.ID, op)
	require.Error(t, err)
	f.requireUnchanged(t, seq)
	events, nacks := f.processedOps(t)
	assert.Equal(t, int(seq), events)
	assert.Zero(t, nacks)
}

func TestEventsCarryOpIDActorAndAreStoredInTheLog(t *testing.T) {
	f := newFixture(t)
	editor := f.member(t, domain.RoleEditor)
	colID := uuid.New()
	opID := uuid.New()
	out := f.applyAs(t, editor.ID, opID, protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: colID, Title: "Todo"})
	require.NotNil(t, out.Event)

	logged := f.eventsAfter(t, 0)
	require.Len(t, logged, 1)
	assert.Equal(t, out.Event.Seq, logged[0].Seq)
	require.NotNil(t, logged[0].OpID)
	assert.Equal(t, opID, *logged[0].OpID)
	require.NotNil(t, logged[0].ActorID)
	assert.Equal(t, editor.ID, *logged[0].ActorID)
	assert.JSONEq(t, string(out.Event.Payload), string(logged[0].Payload))
	assert.WithinDuration(t, time.Now(), logged[0].CreatedAt, time.Minute)
}
