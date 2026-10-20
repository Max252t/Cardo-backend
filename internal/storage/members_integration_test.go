package storage_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestBoardMember(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, "owner@example.com")
	stranger := testutil.CreateUser(t, store, "stranger@example.com")
	board := createBoard(t, store, owner.ID, "Roadmap")
	ctx := context.Background()

	m, err := store.BoardMember(ctx, board.ID, owner.ID)
	require.NoError(t, err)
	assert.Equal(t, owner.ID, m.UserID)
	assert.Equal(t, owner.DisplayName, m.DisplayName)
	assert.Equal(t, owner.AvatarColor, m.AvatarColor)
	assert.Equal(t, domain.RoleOwner, m.Role)

	_, err = store.BoardMember(ctx, board.ID, stranger.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = store.BoardMember(ctx, uuid.New(), owner.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)

	_, err = pool.Exec(ctx, `UPDATE boards SET deleted_at = now() WHERE id = $1`, board.ID)
	require.NoError(t, err)
	_, err = store.BoardMember(ctx, board.ID, owner.ID)
	require.ErrorIs(t, err, domain.ErrNotFound)
}
