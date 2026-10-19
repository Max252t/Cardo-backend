package storage_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestUserCreateAndGet(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()

	id, err := uuid.NewV7()
	require.NoError(t, err)
	created, err := store.CreateUser(ctx, domain.User{
		ID: id, Email: "alice@example.com", PasswordHash: "hash", DisplayName: "Alice", AvatarColor: "#ABCDEF",
	})
	require.NoError(t, err)
	assert.Equal(t, id, created.ID)
	assert.Equal(t, "alice@example.com", created.Email)
	assert.Equal(t, "hash", created.PasswordHash)
	assert.False(t, created.CreatedAt.IsZero())

	byID, err := store.GetUserByID(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, created, byID)

	byEmail, err := store.GetUserByEmail(ctx, "alice@example.com")
	require.NoError(t, err)
	assert.Equal(t, created, byEmail)

	byEmailMixedCase, err := store.GetUserByEmail(ctx, "ALICE@Example.com")
	require.NoError(t, err)
	assert.Equal(t, id, byEmailMixedCase.ID, "citext lookup is case-insensitive")
}

func TestUserNotFound(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()

	_, err := store.GetUserByID(ctx, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = store.GetUserByEmail(ctx, "ghost@example.com")
	assert.ErrorIs(t, err, domain.ErrNotFound)

	name := "x"
	_, err = store.UpdateUserProfile(ctx, uuid.New(), domain.ProfileUpdate{DisplayName: &name})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestUserDuplicateEmail(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()
	testutil.CreateUser(t, store, "dup@example.com")

	id, err := uuid.NewV7()
	require.NoError(t, err)
	for _, email := range []string{"dup@example.com", "DUP@example.com"} {
		_, err = store.CreateUser(ctx, domain.User{ID: id, Email: email, PasswordHash: "h", DisplayName: "B", AvatarColor: "#000000"})
		assert.ErrorIs(t, err, domain.ErrEmailTaken, email)
	}
}

func TestUserConcurrentDuplicateRegistration(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()

	const workers = 10
	results := make(chan error, workers)
	for range workers {
		go func() {
			id, err := uuid.NewV7()
			if err != nil {
				results <- err
				return
			}
			_, err = store.CreateUser(ctx, domain.User{ID: id, Email: "race@example.com", PasswordHash: "h", DisplayName: "R", AvatarColor: "#000000"})
			results <- err
		}()
	}
	var ok, taken int
	for range workers {
		err := <-results
		switch {
		case err == nil:
			ok++
		case assert.ErrorIs(t, err, domain.ErrEmailTaken):
			taken++
		}
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, workers-1, taken)
}

func TestUserUpdateProfile(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()
	u := testutil.CreateUser(t, store, "profile@example.com")

	name, color := "Renamed", "#FFFFFF"

	got, err := store.UpdateUserProfile(ctx, u.ID, domain.ProfileUpdate{DisplayName: &name})
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.DisplayName)
	assert.Equal(t, u.AvatarColor, got.AvatarColor)

	got, err = store.UpdateUserProfile(ctx, u.ID, domain.ProfileUpdate{AvatarColor: &color})
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.DisplayName)
	assert.Equal(t, "#FFFFFF", got.AvatarColor)

	got, err = store.UpdateUserProfile(ctx, u.ID, domain.ProfileUpdate{})
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.DisplayName)
	assert.Equal(t, "#FFFFFF", got.AvatarColor)

	persisted, err := store.GetUserByID(ctx, u.ID)
	require.NoError(t, err)
	assert.Equal(t, got, persisted)
}
