package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

type timedRecorder struct {
	mu        sync.Mutex
	seen      []observedOp
	durations []time.Duration
}

func (r *timedRecorder) ObserveOp(opType, result string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, observedOp{opType, result})
	r.durations = append(r.durations, d)
}

func TestMetricTypeKeepsEveryKnownOperation(t *testing.T) {
	t.Parallel()
	known := []string{
		boardcontent.OpCardCreate, boardcontent.OpCardEdit, boardcontent.OpCardMove, boardcontent.OpCardArchive,
		boardcontent.OpColumnCreate, boardcontent.OpColumnRename, boardcontent.OpColumnMove, boardcontent.OpColumnArchive,
	}
	for _, opType := range known {
		assert.Equal(t, opType, metricType(opType))
	}
}

func TestMetricTypeBoundsUnknownOperations(t *testing.T) {
	t.Parallel()
	for _, opType := range []string{"", "op.card.explode", "card.create", boardcontent.EventCardCreated, "OP.CARD.CREATE", "op.card.create ", "x\x00y"} {
		assert.Equal(t, TypeUnknown, metricType(opType), fmt.Sprintf("%q", opType))
	}
}

func TestMetricResultFromErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"not found", domain.ErrNotFound, boardcontent.ReasonNotFound},
		{"wrapped not found", fmt.Errorf("lock: %w", domain.ErrNotFound), boardcontent.ReasonNotFound},
		{"forbidden", domain.ErrForbidden, boardcontent.ReasonForbidden},
		{"wrapped forbidden", fmt.Errorf("check: %w", domain.ErrForbidden), boardcontent.ReasonForbidden},
		{"validation", domain.NewValidationError("title", "bad"), boardcontent.ReasonValidation},
		{"wrapped validation", fmt.Errorf("payload: %w", domain.NewValidationError("title", "bad")), boardcontent.ReasonValidation},
		{"context canceled", context.Canceled, ResultInternal},
		{"deadline", context.DeadlineExceeded, ResultInternal},
		{"arbitrary", assert.AnError, ResultInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, metricResult(boardcontent.OpOutcome{}, tt.err))
		})
	}
}

func TestMetricResultErrorWinsOverOutcome(t *testing.T) {
	t.Parallel()
	out := boardcontent.OpOutcome{Event: &domain.Event{}, Replayed: true, Nack: &boardcontent.Nack{Reason: boardcontent.ReasonVersionConflict}}
	assert.Equal(t, ResultInternal, metricResult(out, assert.AnError))
}

func TestMetricResultFromNackReasons(t *testing.T) {
	t.Parallel()
	reasons := []string{
		boardcontent.ReasonVersionConflict, boardcontent.ReasonNotFound, boardcontent.ReasonForbidden,
		boardcontent.ReasonValidation, boardcontent.ReasonRateLimited, boardcontent.ReasonInternal,
	}
	for _, reason := range reasons {
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, reason, metricResult(boardcontent.OpOutcome{Nack: &boardcontent.Nack{Reason: reason}}, nil))
			assert.Equal(t, reason, metricResult(boardcontent.OpOutcome{Nack: &boardcontent.Nack{Reason: reason}, Replayed: true}, nil))
		})
	}
}

func TestMetricResultAppliedAndReplayed(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ResultApplied, metricResult(boardcontent.OpOutcome{}, nil))
	assert.Equal(t, ResultApplied, metricResult(boardcontent.OpOutcome{Event: &domain.Event{}}, nil))
	assert.Equal(t, ResultReplayed, metricResult(boardcontent.OpOutcome{Replayed: true}, nil))
}

func TestResultConstantsDoNotCollideWithNackReasons(t *testing.T) {
	t.Parallel()
	reasons := map[string]struct{}{
		boardcontent.ReasonVersionConflict: {}, boardcontent.ReasonNotFound: {}, boardcontent.ReasonForbidden: {},
		boardcontent.ReasonValidation: {}, boardcontent.ReasonRateLimited: {},
	}
	assert.NotContains(t, reasons, ResultApplied)
	assert.NotContains(t, reasons, ResultReplayed)
	assert.Equal(t, boardcontent.ReasonInternal, ResultInternal)
}

func TestApplyWithoutMetricsOptionUsesNoopRecorder(t *testing.T) {
	t.Parallel()
	p := NewProcessor(failingStore{err: domain.ErrNotFound})
	op := boardcontent.Op{Type: boardcontent.OpCardCreate, OpID: uuid.New(), Payload: json.RawMessage(`{}`)}
	assert.NotPanics(t, func() {
		_, err := p.Apply(context.Background(), uuid.New(), uuid.New(), op)
		require.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestWithMetricsNilKeepsTheNoopRecorder(t *testing.T) {
	t.Parallel()
	p := NewProcessor(failingStore{err: domain.ErrNotFound}, WithMetrics(nil))
	op := boardcontent.Op{Type: boardcontent.OpCardCreate, OpID: uuid.New(), Payload: json.RawMessage(`{}`)}
	assert.NotPanics(t, func() {
		_, _ = p.Apply(context.Background(), uuid.New(), uuid.New(), op)
	})
}

func TestApplyObservesExactlyOncePerCallWithNonNegativeDuration(t *testing.T) {
	t.Parallel()
	rec := &timedRecorder{}
	p := NewProcessor(failingStore{err: domain.ErrForbidden}, WithMetrics(rec))
	for range 5 {
		op := boardcontent.Op{Type: boardcontent.OpColumnCreate, OpID: uuid.New(), Payload: json.RawMessage(`{}`)}
		_, err := p.Apply(context.Background(), uuid.New(), uuid.New(), op)
		require.ErrorIs(t, err, domain.ErrForbidden)
	}

	require.Len(t, rec.seen, 5)
	for i, d := range rec.durations {
		assert.GreaterOrEqual(t, d, time.Duration(0), i)
		assert.Equal(t, observedOp{boardcontent.OpColumnCreate, boardcontent.ReasonForbidden}, rec.seen[i])
	}
}

func TestApplyWithNilOpIDIsNotObserved(t *testing.T) {
	t.Parallel()
	rec := &timedRecorder{}
	p := NewProcessor(failingStore{}, WithMetrics(rec))

	_, err := p.Apply(context.Background(), uuid.New(), uuid.New(), boardcontent.Op{Type: boardcontent.OpCardCreate})

	var vErr *domain.ValidationError
	require.ErrorAs(t, err, &vErr)
	assert.Empty(t, rec.seen)
}

func TestApplyObservesCanceledContextAsInternal(t *testing.T) {
	t.Parallel()
	rec := &timedRecorder{}
	p := NewProcessor(failingStore{err: context.Canceled}, WithMetrics(rec))
	op := boardcontent.Op{Type: boardcontent.OpCardMove, OpID: uuid.New(), Payload: json.RawMessage(`{}`)}

	_, err := p.Apply(context.Background(), uuid.New(), uuid.New(), op)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []observedOp{{boardcontent.OpCardMove, ResultInternal}}, rec.seen)
}
