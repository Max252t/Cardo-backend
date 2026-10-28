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

type window struct {
	anchor, neighbour string
	found             bool
}

func cardWindow(t *testing.T, btx boardtx.Tx, col, anchor uuid.UUID, dir boardtx.RankDirection, exclude uuid.UUID) window {
	t.Helper()
	a, n, ok, err := btx.CardRankWindow(context.Background(), col, anchor, dir, exclude)
	require.NoError(t, err)
	return window{a, n, ok}
}

func columnWindow(t *testing.T, btx boardtx.Tx, anchor uuid.UUID, dir boardtx.RankDirection, exclude uuid.UUID) window {
	t.Helper()
	a, n, ok, err := btx.ColumnRankWindow(context.Background(), anchor, dir, exclude)
	require.NoError(t, err)
	return window{a, n, ok}
}

func lastRank(t *testing.T) func(string, bool, error) (string, bool) {
	return func(r string, ok bool, err error) (string, bool) {
		t.Helper()
		require.NoError(t, err)
		return r, ok
	}
}

func TestCardRankWindowUsesBytewiseOrder(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		assert.Equal(t, window{"Zy", "a0", true}, cardWindow(t, btx, w.column, w.cards["Zy"], boardtx.RankAfter, uuid.Nil), "uppercase sorts before lowercase")
		assert.Equal(t, window{"a1", "a3", true}, cardWindow(t, btx, w.column, w.cards["a1"], boardtx.RankAfter, uuid.Nil))
		assert.Equal(t, window{"a0", "Zy", true}, cardWindow(t, btx, w.column, w.cards["a0"], boardtx.RankBefore, uuid.Nil))
		assert.Equal(t, window{"a5", "", true}, cardWindow(t, btx, w.column, w.cards["a5"], boardtx.RankAfter, uuid.Nil))
		assert.Equal(t, window{"Zy", "", true}, cardWindow(t, btx, w.column, w.cards["Zy"], boardtx.RankBefore, uuid.Nil))

		r, ok := lastRank(t)(btx.CardLastRank(ctx, w.column, uuid.Nil))
		assert.True(t, ok)
		assert.Equal(t, "a5", r)
	})
}

func TestCardRankWindowExcludesSelfAndScopesToColumn(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		assert.Equal(t, window{"a1", "a5", true}, cardWindow(t, btx, w.column, w.cards["a1"], boardtx.RankAfter, w.cards["a3"]), "the excluded card is skipped")
		assert.Equal(t, window{"a5", "a1", true}, cardWindow(t, btx, w.column, w.cards["a5"], boardtx.RankBefore, w.cards["a3"]))

		r, _ := lastRank(t)(btx.CardLastRank(ctx, w.column, w.cards["a5"]))
		assert.Equal(t, "a3", r)

		assert.False(t, cardWindow(t, btx, w.other, w.cards["a3"], boardtx.RankAfter, uuid.Nil).found, "a card of another column is not an anchor")
		assert.False(t, cardWindow(t, btx, w.column, uuid.New(), boardtx.RankAfter, uuid.Nil).found)

		r, ok := lastRank(t)(btx.CardLastRank(ctx, w.other, uuid.Nil))
		assert.True(t, ok)
		assert.Equal(t, "zz", r)
		_, ok = lastRank(t)(btx.CardLastRank(ctx, uuid.New(), uuid.Nil))
		assert.False(t, ok)
	})
}

func TestCardRankWindowIgnoresArchivedCards(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		_, err := btx.ArchiveCard(ctx, w.cards["a5"], w.owner.ID)
		require.NoError(t, err)

		r, _ := lastRank(t)(btx.CardLastRank(ctx, w.column, uuid.Nil))
		assert.Equal(t, "a3", r)
		assert.False(t, cardWindow(t, btx, w.column, w.cards["a5"], boardtx.RankAfter, uuid.Nil).found)
		assert.Equal(t, window{"a3", "", true}, cardWindow(t, btx, w.column, w.cards["a3"], boardtx.RankAfter, uuid.Nil))
	})
}

func TestColumnRankWindowUsesBytewiseOrder(t *testing.T) {
	w := newRankWorld(t)
	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		assert.Equal(t, window{"Zz", "a0", true}, columnWindow(t, btx, w.cols["Zz"], boardtx.RankAfter, uuid.Nil))
		assert.Equal(t, window{"a0", "a4", true}, columnWindow(t, btx, w.cols["a0"], boardtx.RankAfter, w.cols["a2"]))
		assert.Equal(t, window{"a0", "Zz", true}, columnWindow(t, btx, w.cols["a0"], boardtx.RankBefore, uuid.Nil))
		assert.Equal(t, window{"a4", "a0", true}, columnWindow(t, btx, w.cols["a4"], boardtx.RankBefore, w.cols["a2"]))
		assert.Equal(t, window{"a4", "", true}, columnWindow(t, btx, w.cols["a4"], boardtx.RankAfter, uuid.Nil))
		assert.False(t, columnWindow(t, btx, uuid.New(), boardtx.RankAfter, uuid.Nil).found)

		r, _ := lastRank(t)(btx.ColumnLastRank(ctx, uuid.Nil))
		assert.Equal(t, "a4", r)
		r, _ = lastRank(t)(btx.ColumnLastRank(ctx, w.cols["a4"]))
		assert.Equal(t, "a2", r)
	})
}

func TestColumnRankWindowIsScopedToTheBoard(t *testing.T) {
	w := newRankWorld(t)
	foreign := createBoard(t, w.store, w.owner.ID, "foreign")
	var foreignCol uuid.UUID
	require.NoError(t, w.store.WithBoardLock(context.Background(), foreign.ID, func(ctx context.Context, btx boardtx.Tx) error {
		foreignCol = uuid.New()
		_, err := btx.InsertColumn(ctx, domain.Column{ID: foreignCol, Title: "z", Rank: "z9"})
		return err
	}))

	w.inLock(t, func(ctx context.Context, btx boardtx.Tx) {
		r, _ := lastRank(t)(btx.ColumnLastRank(ctx, uuid.Nil))
		assert.Equal(t, "a4", r)
		assert.Equal(t, window{"a4", "", true}, columnWindow(t, btx, w.cols["a4"], boardtx.RankAfter, uuid.Nil))
		assert.False(t, columnWindow(t, btx, foreignCol, boardtx.RankAfter, uuid.Nil).found)
	})
}
