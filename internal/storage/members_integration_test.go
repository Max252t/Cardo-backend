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

func TestDatabaseRejectsASecondOwnerRow(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, "owner-index@example.com")
	other := testutil.CreateUser(t, store, "other-index@example.com")
	board := createBoard(t, store, owner.ID, "Single owner")
	ctx := context.Background()

	_, err := pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'owner')`, board.ID, other.ID)
	require.Error(t, err)

	_, err = pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'editor')`, board.ID, other.ID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE board_members SET role = 'owner' WHERE board_id = $1 AND user_id = $2`, board.ID, other.ID)
	require.Error(t, err)
}
