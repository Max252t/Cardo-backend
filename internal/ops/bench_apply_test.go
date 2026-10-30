package ops_test

import (
	"context"
	"math/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/rank"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

const benchColumnCards = 2000

type benchBoard struct {
	store  *storage.Store
	owner  domain.User
	board  uuid.UUID
	column uuid.UUID
	cards  []uuid.UUID
}

func seedBenchBoard(b *testing.B, cards int) *benchBoard {
	b.Helper()
	pool := testutil.NewMigratedPool(b)
	store := storage.New(pool)
	owner := testutil.CreateUser(b, store, testutil.UniqueEmail("bench"))
	board, err := store.CreateBoardWithOwner(context.Background(), domain.Board{ID: uuid.New(), Title: "Bench", OwnerID: owner.ID})
	require.NoError(b, err)

	column := uuid.New()
	_, err = pool.Exec(context.Background(),
		`INSERT INTO columns (id, board_id, title, rank) VALUES ($1, $2, 'Todo', 'a0')`, column, board.ID)
	require.NoError(b, err)

	keys, err := rank.Evenly(cards)
	require.NoError(b, err)
	ids := make([]uuid.UUID, cards)
	for i := range ids {
		ids[i] = uuid.New()
	}
	_, err = pool.Exec(context.Background(),
		`INSERT INTO cards (id, board_id, column_id, title, rank, created_by, updated_by)
		 SELECT v.id, $1, $2, 'card', v.rank, $3, $3 FROM unnest($4::uuid[], $5::text[]) AS v(id, rank)`,
		board.ID, column, owner.ID, ids, keys)
	require.NoError(b, err)
	return &benchBoard{store: store, owner: owner, board: board.ID, column: column, cards: ids}
}

func BenchmarkApplyCardCreateIntoLargeColumn(b *testing.B) {
	if testing.Short() {
		b.Skip("integration benchmark skipped in short mode")
	}
	w := seedBenchBoard(b, benchColumnCards)
	proc := ops.NewProcessor(w.store)
	rng := rand.New(rand.NewSource(1))
	ctx := context.Background()

	b.ResetTimer()
	for range b.N {
		anchor := w.cards[rng.Intn(len(w.cards))]
		op, err := boardcontent.NewOp(boardcontent.OpCardCreate, uuid.New(), boardcontent.CardCreate{
			CardID:   uuid.New(),
			ColumnID: w.column,
			Title:    "new",
			AfterID:  &anchor,
		})
		require.NoError(b, err)
		out, err := proc.Apply(ctx, w.owner.ID, w.board, op)
		require.NoError(b, err)
		require.Nil(b, out.Nack)
	}
}
