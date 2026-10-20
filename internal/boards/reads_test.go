package boards_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type readStore struct {
	snapshot    func(ctx context.Context, boardID, userID uuid.UUID) (domain.BoardSnapshot, error)
	events      func(ctx context.Context, boardID, userID uuid.UUID, afterSeq int64, limit int) (domain.EventPage, error)
	eventsCalls int
}

func (s *readStore) ListBoardsForUser(context.Context, uuid.UUID) ([]domain.BoardWithRole, error) {
	return nil, nil
}

func (s *readStore) CreateBoardWithOwner(context.Context, domain.Board) (domain.BoardWithRole, error) {
	return domain.BoardWithRole{}, nil
}

func (s *readStore) WithBoardLock(context.Context, uuid.UUID, storage.BoardTxFunc) error {
	return nil
}

func (s *readStore) Snapshot(ctx context.Context, boardID, userID uuid.UUID) (domain.BoardSnapshot, error) {
	return s.snapshot(ctx, boardID, userID)
}

func (s *readStore) EventsAfter(ctx context.Context, boardID, userID uuid.UUID, afterSeq int64, limit int) (domain.EventPage, error) {
	s.eventsCalls++
	return s.events(ctx, boardID, userID, afterSeq, limit)
}

func TestEventsRejectsNegativeAfterSeqWithoutHittingTheStore(t *testing.T) {
	t.Parallel()
	store := &readStore{}
	svc := boards.NewService(store)
	for _, after := range []int64{-1, -100, -1 << 62} {
		_, err := svc.Events(context.Background(), uuid.New(), uuid.New(), after)
		var vErr *domain.ValidationError
		require.ErrorAs(t, err, &vErr)
		assert.Equal(t, "after_seq", vErr.Field)
	}
	assert.Zero(t, store.eventsCalls)
}

func TestEventsPassesArgumentsAndPageLimit(t *testing.T) {
	t.Parallel()
	userID, boardID := uuid.New(), uuid.New()
	want := domain.EventPage{Seq: 9, HasMore: true, Events: []domain.Event{{Seq: 8}}}
	store := &readStore{events: func(_ context.Context, b, u uuid.UUID, after int64, limit int) (domain.EventPage, error) {
		assert.Equal(t, boardID, b)
		assert.Equal(t, userID, u)
		assert.Equal(t, int64(7), after)
		assert.Equal(t, boards.MaxEventsPerPage, limit)
		return want, nil
	}}
	page, err := boards.NewService(store).Events(context.Background(), userID, boardID, 7)
	require.NoError(t, err)
	assert.Equal(t, want, page)
	assert.Equal(t, 1000, boards.MaxEventsPerPage)
}

func TestEventsPropagatesStoreErrors(t *testing.T) {
	t.Parallel()
	for _, want := range []error{domain.ErrGone, domain.ErrNotFound, domain.ErrBusy, errors.New("db down")} {
		store := &readStore{events: func(context.Context, uuid.UUID, uuid.UUID, int64, int) (domain.EventPage, error) {
			return domain.EventPage{}, want
		}}
		_, err := boards.NewService(store).Events(context.Background(), uuid.New(), uuid.New(), 0)
		assert.ErrorIs(t, err, want)
	}
}

func TestSnapshotPassesArgumentsInTheServiceOrder(t *testing.T) {
	t.Parallel()
	userID, boardID := uuid.New(), uuid.New()
	want := domain.BoardSnapshot{Seq: 3}
	store := &readStore{snapshot: func(_ context.Context, b, u uuid.UUID) (domain.BoardSnapshot, error) {
		assert.Equal(t, boardID, b)
		assert.Equal(t, userID, u)
		return want, nil
	}}
	snap, err := boards.NewService(store).Snapshot(context.Background(), userID, boardID)
	require.NoError(t, err)
	assert.Equal(t, want, snap)

	boom := errors.New("boom")
	store.snapshot = func(context.Context, uuid.UUID, uuid.UUID) (domain.BoardSnapshot, error) {
		return domain.BoardSnapshot{}, boom
	}
	_, err = boards.NewService(store).Snapshot(context.Background(), userID, boardID)
	assert.ErrorIs(t, err, boom)
}
