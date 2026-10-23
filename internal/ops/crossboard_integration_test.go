package ops_test

import (
	"context"
	"sync"
	"testing"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSameEntityIDCreatedConcurrentlyOnTwoBoards(t *testing.T) {
	f := newConcurrentFixture(t)
	boardA := f.board.ID
	boardB := f.newBoard(t, f.owner.ID)
	colA, colB := uuid.New(), uuid.New()
	for board, col := range map[uuid.UUID]uuid.UUID{boardA: colA, boardB: colB} {
		out := f.applyOnBoard(t, board, f.owner.ID, uuid.New(), boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: col, Title: "Todo"})
		require.NotNil(t, out.Event)
	}

	const rounds = 15
	for round := range rounds {
		cardID := uuid.New()
		var wg sync.WaitGroup
		var outcomes [2]result
		start := make(chan struct{})
		for i, target := range []struct{ board, col uuid.UUID }{{boardA, colA}, {boardB, colB}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				op := mustOp(t, boardcontent.OpCardCreate, uuid.New(), boardcontent.CardCreate{CardID: cardID, ColumnID: target.col, Title: "same id"})
				out, err := f.proc.Apply(context.Background(), f.owner.ID, target.board, op)
				outcomes[i] = result{out: out, err: err}
			}()
		}
		close(start)
		wg.Wait()

		created := 0
		for _, r := range outcomes {
			if r.err == nil && r.out.Event != nil {
				created++
				continue
			}
			require.NoError(t, r.err, "round %d: the loser must receive a nack, not an error", round)
			require.NotNil(t, r.out.Nack)
			assert.Equal(t, boardcontent.ReasonValidation, r.out.Nack.Reason)
		}
		assert.Equal(t, 1, created, "exactly one board may own the card id")
	}
}

func TestSameColumnIDCreatedConcurrentlyOnTwoBoards(t *testing.T) {
	f := newConcurrentFixture(t)
	boards := []uuid.UUID{f.board.ID, f.newBoard(t, f.owner.ID)}
	for round := range 10 {
		colID := uuid.New()
		results := applyAcrossBoards(f, boards, func(int) (uuid.UUID, boardcontent.Op) {
			return f.owner.ID, mustOp(t, boardcontent.OpColumnCreate, uuid.New(), boardcontent.ColumnCreate{ColumnID: colID, Title: "same"})
		})
		created := 0
		for _, r := range results {
			require.NoError(t, r.err, "round %d", round)
			if r.out.Event != nil {
				created++
				continue
			}
			require.NotNil(t, r.out.Nack)
			assert.Equal(t, boardcontent.ReasonValidation, r.out.Nack.Reason)
			assert.Equal(t, "column_id: is not available", r.out.Nack.Message)
		}
		assert.Equal(t, 1, created, "round %d", round)
	}
}

func TestSameOpIDSentConcurrentlyToTwoBoards(t *testing.T) {
	f := newConcurrentFixture(t)
	boards := []uuid.UUID{f.board.ID, f.newBoard(t, f.owner.ID)}
	for round := range 15 {
		opID := uuid.New()
		results := applyAcrossBoards(f, boards, func(int) (uuid.UUID, boardcontent.Op) {
			return f.owner.ID, mustOp(t, boardcontent.OpColumnCreate, opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "race"})
		})
		applied := 0
		for _, r := range results {
			require.NoError(t, r.err, "round %d: the loser must receive a nack, not an error", round)
			if r.out.Event != nil {
				applied++
				continue
			}
			require.NotNil(t, r.out.Nack)
			assert.Equal(t, boardcontent.ReasonValidation, r.out.Nack.Reason)
			assert.Contains(t, r.out.Nack.Message, "op_id")
		}
		assert.Equal(t, 1, applied, "round %d: exactly one board applies the op", round)

		var stored int
		require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_ops WHERE op_id = $1`, opID).Scan(&stored))
		assert.Equal(t, 1, stored)
	}
	var columns int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM columns`).Scan(&columns))
	assert.Equal(t, 15, columns, "the losing board must not keep the op's changes")
}

func TestDuplicateIDNackDoesNotRevealOtherBoards(t *testing.T) {
	f := newFixture(t)
	other := f.newBoard(t, f.owner.ID)
	colHere := f.column(t, "Here", nil)
	colThere := uuid.New()
	require.NotNil(t, f.applyOnBoard(t, other, f.owner.ID, uuid.New(), boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: colThere, Title: "There"}).Event)
	cardThere := uuid.New()
	require.NotNil(t, f.applyOnBoard(t, other, f.owner.ID, uuid.New(), boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: cardThere, ColumnID: colThere, Title: "x"}).Event)
	cardHere := f.card(t, colHere, "here")

	sameBoard := f.mustNack(t, boardcontent.ReasonValidation, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: cardHere, ColumnID: colHere, Title: "dup"})
	otherBoard := f.mustNack(t, boardcontent.ReasonValidation, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: cardThere, ColumnID: colHere, Title: "dup"})
	assert.Equal(t, sameBoard.Message, otherBoard.Message)
	assert.Equal(t, "card_id: is not available", otherBoard.Message)

	colNack := f.mustNack(t, boardcontent.ReasonValidation, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: colThere, Title: "dup"})
	assert.Equal(t, "column_id: is not available", colNack.Message)
}

func applyAcrossBoards(f *fixture, boards []uuid.UUID, build func(i int) (uuid.UUID, boardcontent.Op)) []result {
	results := make([]result, len(boards))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, board := range boards {
		user, op := build(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := f.proc.Apply(context.Background(), user, board, op)
			results[i] = result{out: out, err: err}
		}()
	}
	close(start)
	wg.Wait()
	return results
}
