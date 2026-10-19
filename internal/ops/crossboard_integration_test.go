package ops_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func TestSameEntityIDCreatedConcurrentlyOnTwoBoards(t *testing.T) {
	t.Skip("BUG: concurrent card.create with one card_id on two boards loses the race on cards_pkey and Apply returns a raw unique violation instead of a validation_error nack")
	f := newConcurrentFixture(t)
	boardA := f.board.ID
	boardB := f.newBoard(t, f.owner.ID)
	colA, colB := uuid.New(), uuid.New()
	for board, col := range map[uuid.UUID]uuid.UUID{boardA: colA, boardB: colB} {
		out := f.applyOnBoard(t, board, f.owner.ID, uuid.New(), protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: col, Title: "Todo"})
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
				op := mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{CardID: cardID, ColumnID: target.col, Title: "same id"})
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
			assert.Equal(t, protocol.ReasonValidation, r.out.Nack.Reason)
		}
		assert.Equal(t, 1, created, "exactly one board may own the card id")
	}
}
