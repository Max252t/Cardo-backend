package users_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/users"
)

type fakeStore struct {
	user       domain.User
	getErr     error
	updateErr  error
	getCalls   int
	updateArgs []domain.ProfileUpdate
}

func (s *fakeStore) GetUserByID(_ context.Context, id uuid.UUID) (domain.User, error) {
	s.getCalls++
	if s.getErr != nil {
		return domain.User{}, s.getErr
	}
	if id != s.user.ID {
		return domain.User{}, domain.ErrNotFound
	}
	return s.user, nil
}

func (s *fakeStore) UpdateUserProfile(_ context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error) {
	s.updateArgs = append(s.updateArgs, upd)
	if s.updateErr != nil {
		return domain.User{}, s.updateErr
	}
	if id != s.user.ID {
		return domain.User{}, domain.ErrNotFound
	}
	if upd.DisplayName != nil {
		s.user.DisplayName = *upd.DisplayName
	}
	if upd.AvatarColor != nil {
		s.user.AvatarColor = *upd.AvatarColor
	}
	return s.user, nil
}

func newStore() *fakeStore {
	return &fakeStore{user: domain.User{
		ID:          uuid.New(),
		Email:       "alice@example.com",
		DisplayName: "Alice",
		AvatarColor: "#111111",
	}}
}

func ptr(s string) *string { return &s }

func TestGet(t *testing.T) {
	t.Parallel()
	st := newStore()
	svc := users.NewService(st)

	got, err := svc.Get(context.Background(), st.user.ID)
	require.NoError(t, err)
	assert.Equal(t, st.user, got)

	_, err = svc.Get(context.Background(), uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	boom := errors.New("boom")
	st.getErr = boom
	_, err = svc.Get(context.Background(), st.user.ID)
	assert.ErrorIs(t, err, boom)
}

func TestUpdateProfileNormalizesInput(t *testing.T) {
	t.Parallel()
	st := newStore()
	svc := users.NewService(st)

	got, err := svc.UpdateProfile(context.Background(), st.user.ID, domain.ProfileUpdate{
		DisplayName: ptr("  Alicia  "),
		AvatarColor: ptr("#abcdef"),
	})
	require.NoError(t, err)
	assert.Equal(t, "Alicia", got.DisplayName)
	assert.Equal(t, "#ABCDEF", got.AvatarColor)

	require.Len(t, st.updateArgs, 1)
	assert.Equal(t, "Alicia", *st.updateArgs[0].DisplayName)
	assert.Equal(t, "#ABCDEF", *st.updateArgs[0].AvatarColor)
}

func TestUpdateProfilePartial(t *testing.T) {
	t.Parallel()
	t.Run("name only", func(t *testing.T) {
		t.Parallel()
		st := newStore()
		got, err := users.NewService(st).UpdateProfile(context.Background(), st.user.ID, domain.ProfileUpdate{DisplayName: ptr("Bob")})
		require.NoError(t, err)
		assert.Equal(t, "Bob", got.DisplayName)
		assert.Equal(t, "#111111", got.AvatarColor)
		assert.Nil(t, st.updateArgs[0].AvatarColor)
	})
	t.Run("color only", func(t *testing.T) {
		t.Parallel()
		st := newStore()
		got, err := users.NewService(st).UpdateProfile(context.Background(), st.user.ID, domain.ProfileUpdate{AvatarColor: ptr("#222222")})
		require.NoError(t, err)
		assert.Equal(t, "Alice", got.DisplayName)
		assert.Equal(t, "#222222", got.AvatarColor)
		assert.Nil(t, st.updateArgs[0].DisplayName)
	})
}

func TestUpdateProfileEmptyUpdateReadsUser(t *testing.T) {
	t.Parallel()
	st := newStore()
	got, err := users.NewService(st).UpdateProfile(context.Background(), st.user.ID, domain.ProfileUpdate{})
	require.NoError(t, err)
	assert.Equal(t, st.user, got)
	assert.Empty(t, st.updateArgs)
	assert.Equal(t, 1, st.getCalls)
}

func TestUpdateProfileValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		upd   domain.ProfileUpdate
		field string
	}{
		{"blank name", domain.ProfileUpdate{DisplayName: ptr("   ")}, "display_name"},
		{"empty name", domain.ProfileUpdate{DisplayName: ptr("")}, "display_name"},
		{"bad color", domain.ProfileUpdate{AvatarColor: ptr("blue")}, "avatar_color"},
		{"valid name but bad color", domain.ProfileUpdate{DisplayName: ptr("Ok"), AvatarColor: ptr("#zzz")}, "avatar_color"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			st := newStore()
			_, err := users.NewService(st).UpdateProfile(context.Background(), st.user.ID, tt.upd)
			var vErr *domain.ValidationError
			require.True(t, errors.As(err, &vErr))
			assert.Equal(t, tt.field, vErr.Field)
			assert.Empty(t, st.updateArgs, "invalid input must not reach the store")
		})
	}
}

func TestUpdateProfileStoreErrors(t *testing.T) {
	t.Parallel()
	st := newStore()
	svc := users.NewService(st)

	_, err := svc.UpdateProfile(context.Background(), uuid.New(), domain.ProfileUpdate{DisplayName: ptr("X")})
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = svc.UpdateProfile(context.Background(), uuid.New(), domain.ProfileUpdate{})
	assert.ErrorIs(t, err, domain.ErrNotFound)

	boom := errors.New("boom")
	st.updateErr = boom
	_, err = svc.UpdateProfile(context.Background(), st.user.ID, domain.ProfileUpdate{DisplayName: ptr("X")})
	assert.ErrorIs(t, err, boom)
}
