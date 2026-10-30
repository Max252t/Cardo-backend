package ops

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/rank"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

const placementBenchCards = 2000

func BenchmarkPlacementIntoLargeColumn(b *testing.B) {
	if testing.Short() {
		b.Skip("integration benchmark skipped in short mode")
	}
	pool := testutil.NewMigratedPool(b)
	store := storage.New(pool)
	owner := testutil.CreateUser(b, store, testutil.UniqueEmail("bench"))
	board, err := store.CreateBoardWithOwner(context.Background(), domain.Board{ID: uuid.New(), Title: "Bench", OwnerID: owner.ID})
	require.NoError(b, err)
	column := uuid.New()
	_, err = pool.Exec(context.Background(),
		`INSERT INTO columns (id, board_id, title, rank) VALUES ($1, $2, 'Todo', 'a0')`, column, board.ID)
	require.NoError(b, err)
	keys, err := rank.Evenly(placementBenchCards)
	require.NoError(b, err)
	ids := make([]uuid.UUID, placementBenchCards)
	for i := range ids {
		ids[i] = uuid.New()
	}
	_, err = pool.Exec(context.Background(),
		`INSERT INTO cards (id, board_id, column_id, title, rank, created_by, updated_by)
		 SELECT v.id, $1, $2, 'card', v.rank, $3, $3 FROM unnest($4::uuid[], $5::text[]) AS v(id, rank)`,
		board.ID, column, owner.ID, ids, keys)
	require.NoError(b, err)

	middle := ids[placementBenchCards/2]
	head := ids[0]
	positions := []struct {
		name          string
		after, before *uuid.UUID
	}{
		{"tail", nil, nil},
		{"middle", &middle, nil},
		{"head", nil, &head},
	}

	b.Run("lock-only", func(b *testing.B) {
		ctx := context.Background()
		b.ResetTimer()
		for range b.N {
			require.NoError(b, store.WithBoardLock(ctx, board.ID, func(context.Context, boardtx.Tx) error { return nil }))
		}
	})

	for _, mode := range []string{"window", "full"} {
		for _, pos := range positions {
			b.Run(mode+"/"+pos.name, func(b *testing.B) {
				ctx := context.Background()
				b.ResetTimer()
				for range b.N {
					err := store.WithBoardLock(ctx, board.ID, func(ctx context.Context, btx boardtx.Tx) error {
						a := &applier{btx: btx, actor: owner.ID}
						scope := a.cardRanks(column)
						if mode == "full" {
							scope.probe = nil
						}
						pl, err := placeIn(ctx, scope, uuid.New(), pos.after, pos.before)
						if err != nil {
							return err
						}
						if pl.rank == "" || pl.rebalance != nil {
							b.Fatalf("unexpected placement %+v", pl)
						}
						return nil
					})
					require.NoError(b, err)
				}
			})
		}
	}
}
