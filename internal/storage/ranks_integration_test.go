package storage_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type rankWorld struct {
	store  *storage.Store
	owner  domain.User
	board  domain.BoardWithRole
	column uuid.UUID
	other  uuid.UUID
	cards  map[string]uuid.UUID
	cols   map[string]uuid.UUID
}

func newRankWorld(t *testing.T) *rankWorld {
	t.Helper()
	store := storage.New(testutil.NewMigratedPool(t))
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("ranks"))
	w := &rankWorld{
		store: store,
		owner: owner,
		board: createBoard(t, store, owner.ID, "ranks"),
		cards: map[string]uuid.UUID{},
		cols:  map[string]uuid.UUID{},
	}
	err := store.WithBoardLock(context.Background(), w.board.ID, func(ctx context.Context, btx boardtx.Tx) error {
		for _, r := range []string{"a0", "a2", "a4", "Zz"} {
			c, err := btx.InsertColumn(ctx, domain.Column{ID: uuid.New(), Title: r, Rank: r})
			if err != nil {
				return err
			}
			w.cols[r] = c.ID
		}
		w.column = w.cols["a0"]
		w.other = w.cols["a2"]
		for _, r := range []string{"a1", "a3", "a5", "Zy", "a0"} {
			c, err := btx.InsertCard(ctx, domain.Card{ID: uuid.New(), ColumnID: w.column, Title: r, Rank: r, CreatedBy: owner.ID})
			if err != nil {
				return err
			}
			w.cards[r] = c.ID
		}
		_, err := btx.InsertCard(ctx, domain.Card{ID: uuid.New(), ColumnID: w.other, Title: "zz", Rank: "zz", CreatedBy: owner.ID})
		return err
	})
	require.NoError(t, err)
	return w
}

func (w *rankWorld) inLock(t *testing.T, fn func(ctx context.Context, btx boardtx.Tx)) {
	t.Helper()
	require.NoError(t, w.store.WithBoardLock(context.Background(), w.board.ID, func(ctx context.Context, btx boardtx.Tx) error {
		fn(ctx, btx)
		return nil
	}))
}

func rankOK(t *testing.T) func(string, bool, error) (string, bool) {
	return func(rank string, ok bool, err error) (string, bool) {
		t.Helper()
		require.NoError(t, err)
		return rank, ok
	}
}

func TestCardRankNeighboursUseBytewiseOrder(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		r, ok := rankOK(t)(btx.CardRankOf(ctx, w.column, w.cards["a3"]))
		assert.True(t, ok)
		assert.Equal(t, "a3", r)

		r, ok = rankOK(t)(btx.CardRankAfter(ctx, w.column, "Zy", uuid.Nil))
		assert.True(t, ok)
		assert.Equal(t, "a0", r, "uppercase sorts before lowercase")

		r, ok = rankOK(t)(btx.CardRankAfter(ctx, w.column, "a1", uuid.Nil))
		assert.True(t, ok)
		assert.Equal(t, "a3", r)

		r, ok = rankOK(t)(btx.CardRankBefore(ctx, w.column, "a0", uuid.Nil))
		assert.True(t, ok)
		assert.Equal(t, "Zy", r)

		r, ok = rankOK(t)(btx.CardLastRank(ctx, w.column, uuid.Nil))
		assert.True(t, ok)
		assert.Equal(t, "a5", r)

		_, ok = rankOK(t)(btx.CardRankAfter(ctx, w.column, "a5", uuid.Nil))
		assert.False(t, ok)
		_, ok = rankOK(t)(btx.CardRankBefore(ctx, w.column, "Zy", uuid.Nil))
		assert.False(t, ok)
	})
}

func TestCardRankNeighboursExcludeSelfAndScopeToColumn(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		r, _ := rankOK(t)(btx.CardRankAfter(ctx, w.column, "a1", w.cards["a3"]))
		assert.Equal(t, "a5", r, "the excluded card is skipped")

		r, _ = rankOK(t)(btx.CardRankBefore(ctx, w.column, "a5", w.cards["a3"]))
		assert.Equal(t, "a1", r)

		r, _ = rankOK(t)(btx.CardLastRank(ctx, w.column, w.cards["a5"]))
		assert.Equal(t, "a3", r)

		_, ok := rankOK(t)(btx.CardRankOf(ctx, w.other, w.cards["a3"]))
		assert.False(t, ok, "a card of another column is not an anchor")

		r, ok = rankOK(t)(btx.CardLastRank(ctx, w.other, uuid.Nil))
		assert.True(t, ok)
		assert.Equal(t, "zz", r)

		_, ok = rankOK(t)(btx.CardLastRank(ctx, uuid.New(), uuid.Nil))
		assert.False(t, ok)
	})
}

func TestCardRankNeighboursIgnoreArchivedCards(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		_, err := btx.ArchiveCard(ctx, w.cards["a5"], w.owner.ID)
		require.NoError(t, err)

		r, _ := rankOK(t)(btx.CardLastRank(ctx, w.column, uuid.Nil))
		assert.Equal(t, "a3", r)
		_, ok := rankOK(t)(btx.CardRankOf(ctx, w.column, w.cards["a5"]))
		assert.False(t, ok)
		_, ok = rankOK(t)(btx.CardRankAfter(ctx, w.column, "a3", uuid.Nil))
		assert.False(t, ok)
	})
}

func TestColumnRankNeighboursUseBytewiseOrder(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		r, ok := rankOK(t)(btx.ColumnRankOf(ctx, w.cols["a2"]))
		assert.True(t, ok)
		assert.Equal(t, "a2", r)

		r, _ = rankOK(t)(btx.ColumnRankAfter(ctx, "Zz", uuid.Nil))
		assert.Equal(t, "a0", r)
		r, _ = rankOK(t)(btx.ColumnRankAfter(ctx, "a0", w.cols["a2"]))
		assert.Equal(t, "a4", r)
		r, _ = rankOK(t)(btx.ColumnRankBefore(ctx, "a0", uuid.Nil))
		assert.Equal(t, "Zz", r)
		r, _ = rankOK(t)(btx.ColumnRankBefore(ctx, "a4", w.cols["a2"]))
		assert.Equal(t, "a0", r)
		r, _ = rankOK(t)(btx.ColumnLastRank(ctx, uuid.Nil))
		assert.Equal(t, "a4", r)
		r, _ = rankOK(t)(btx.ColumnLastRank(ctx, w.cols["a4"]))
		assert.Equal(t, "a2", r)

		_, ok = rankOK(t)(btx.ColumnRankAfter(ctx, "a4", uuid.Nil))
		assert.False(t, ok)
	})
}

func TestColumnRankNeighboursAreScopedToTheBoard(t *testing.T) {
	w := newRankWorld(t)
	foreign := createBoard(t, w.store, w.owner.ID, "foreign")
	require.NoError(t, w.store.WithBoardLock(context.Background(), foreign.ID, func(ctx context.Context, btx boardtx.Tx) error {
		_, err := btx.InsertColumn(ctx, domain.Column{ID: uuid.New(), Title: "z", Rank: "z9"})
		return err
	}))

	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		r, _ := rankOK(t)(btx.ColumnLastRank(ctx, uuid.Nil))
		assert.Equal(t, "a4", r)
		_, ok := rankOK(t)(btx.ColumnRankAfter(ctx, "a4", uuid.Nil))
		assert.False(t, ok)
	})
}
