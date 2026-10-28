package ops_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
)

func TestCardPlacementComparesRanksBytewiseAcrossCases(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	upper := f.card(t, col, "upper")
	lower := f.card(t, col, "lower")
	f.setCardRank(t, upper, "Zz")
	f.setCardRank(t, lower, "a0")

	between := uuid.New()
	f.mustEvent(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: between, ColumnID: col, Title: "between", AfterID: &upper})
	r := f.cardRow(t, between).Rank
	assert.Greater(t, r, "Zz")
	assert.Less(t, r, "a0")

	front := uuid.New()
	f.mustEvent(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: front, ColumnID: col, Title: "front", BeforeID: &upper})
	assert.Less(t, f.cardRow(t, front).Rank, "Zz")

	tail := uuid.New()
	f.mustEvent(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: tail, ColumnID: col, Title: "tail"})
	assert.Greater(t, f.cardRow(t, tail).Rank, "a0")

	assert.Equal(t, []uuid.UUID{front, upper, between, lower, tail}, f.cardsInColumn(t, col))
}

func TestColumnPlacementComparesRanksBytewiseAcrossCases(t *testing.T) {
	f := newFixture(t)
	a := f.column(t, "A", nil)
	b := f.column(t, "B", nil)
	f.setColumnRank(t, a, "Zz")
	f.setColumnRank(t, b, "a0")

	between := uuid.New()
	f.mustEvent(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: between, Title: "between", AfterID: &a})
	r := f.columnRow(t, between).Rank
	assert.Greater(t, r, "Zz")
	assert.Less(t, r, "a0")
	assert.Equal(t, []uuid.UUID{a, between, b}, f.columnOrder(t))
}

func TestCardMoveWithinColumnIgnoresItsOwnRankAsNeighbour(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "1")
	second := f.card(t, col, "2")
	third := f.card(t, col, "3")

	f.mustEvent(t, boardcontent.OpCardMove, boardcontent.CardMove{CardID: first, ToColumnID: col, AfterID: &second, BaseVersion: ptr(int64(1))})
	assert.Equal(t, []uuid.UUID{second, first, third}, f.cardsInColumn(t, col))

	f.mustEvent(t, boardcontent.OpCardMove, boardcontent.CardMove{CardID: first, ToColumnID: col, BeforeID: &second, BaseVersion: ptr(int64(2))})
	assert.Equal(t, []uuid.UUID{first, second, third}, f.cardsInColumn(t, col))

	f.mustEvent(t, boardcontent.OpCardMove, boardcontent.CardMove{CardID: first, ToColumnID: col, BaseVersion: ptr(int64(3))})
	assert.Equal(t, []uuid.UUID{second, third, first}, f.cardsInColumn(t, col))
	assert.EqualValues(t, 4, f.cardRow(t, first).Version)
}
