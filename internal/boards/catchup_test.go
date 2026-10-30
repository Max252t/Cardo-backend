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
)

type fakeHistory struct {
	head      int64
	oldest    int64
	pageSize  int
	snapshots int
	pages     int
	err       error
}

func (h *fakeHistory) Snapshot(context.Context, uuid.UUID, uuid.UUID) (domain.BoardSnapshot, error) {
	h.snapshots++
	if h.err != nil {
		return domain.BoardSnapshot{}, h.err
	}
	return domain.BoardSnapshot{Seq: h.head}, nil
}

func (h *fakeHistory) Events(_ context.Context, _, _ uuid.UUID, after int64) (domain.EventPage, error) {
	h.pages++
	if h.err != nil {
		return domain.EventPage{}, h.err
	}
	if after > h.head {
		return domain.EventPage{}, domain.NewValidationError("after_seq", "ahead")
	}
	if after < h.oldest {
		return domain.EventPage{}, domain.ErrGone
	}
	var events []domain.Event
	for seq := after + 1; seq <= h.head; seq++ {
		events = append(events, domain.Event{Seq: seq})
	}
	page := domain.EventPage{Events: events, Seq: h.head}
	if len(events) > h.pageSize {
		page.Events = events[:h.pageSize]
		page.HasMore = true
	}
	return page, nil
}

func seqPtr(v int64) *int64 {
	return &v
}

func TestCatchUpPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	limits := boards.CatchUpLimits{MaxEvents: 10}
	tests := []struct {
		name         string
		history      *fakeHistory
		last         *int64
		wantSnapshot bool
		wantEvents   int
		wantSeq      int64
	}{
		{"first connection gets a snapshot", &fakeHistory{head: 5, pageSize: 3}, nil, true, 0, 5},
		{"covered gap is a delta", &fakeHistory{head: 5, pageSize: 3}, seqPtr(2), false, 3, 5},
		{"current client gets an empty delta", &fakeHistory{head: 5, pageSize: 3}, seqPtr(5), false, 0, 5},
		{"delta pages are concatenated", &fakeHistory{head: 9, pageSize: 2}, seqPtr(0), false, 9, 9},
		{"gone log falls back to a snapshot", &fakeHistory{head: 9, oldest: 4, pageSize: 3}, seqPtr(1), true, 0, 9},
		{"client ahead of the board falls back to a snapshot", &fakeHistory{head: 5, pageSize: 3}, seqPtr(50), true, 0, 5},
		{"oversized delta falls back to a snapshot", &fakeHistory{head: 30, pageSize: 4}, seqPtr(0), true, 0, 30},
	}
	for _, tt := range tests {
		got, err := boards.CatchUp(ctx, tt.history, uuid.New(), uuid.New(), tt.last, limits)
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.wantSnapshot, got.IsSnapshot(), tt.name)
		assert.Len(t, got.Events, tt.wantEvents, tt.name)
		assert.Equal(t, tt.wantSeq, got.Seq, tt.name)
	}
}

func TestCatchUpPropagatesInfrastructureAndAccessErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	limits := boards.CatchUpLimits{MaxEvents: 10}
	boom := errors.New("db down")

	_, err := boards.CatchUp(ctx, &fakeHistory{err: boom}, uuid.New(), uuid.New(), seqPtr(1), limits)
	require.ErrorIs(t, err, boom)
	_, err = boards.CatchUp(ctx, &fakeHistory{err: boom}, uuid.New(), uuid.New(), nil, limits)
	require.ErrorIs(t, err, boom)
	_, err = boards.CatchUp(ctx, &fakeHistory{err: domain.ErrNotFound}, uuid.New(), uuid.New(), seqPtr(1), limits)
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestCatchUpDoesNotReadTheSnapshotWhenTheDeltaSuffices(t *testing.T) {
	t.Parallel()
	h := &fakeHistory{head: 4, pageSize: 10}
	_, err := boards.CatchUp(context.Background(), h, uuid.New(), uuid.New(), seqPtr(1), boards.CatchUpLimits{MaxEvents: 10})
	require.NoError(t, err)
	assert.Zero(t, h.snapshots)
	assert.Equal(t, 1, h.pages)
}
