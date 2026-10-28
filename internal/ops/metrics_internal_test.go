package ops

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

type observedOp struct {
	opType string
	result string
}

type opRecorder struct {
	mu   sync.Mutex
	seen []observedOp
}

func (r *opRecorder) ObserveOp(opType, result string, _ time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, observedOp{opType, result})
}

type failingStore struct{ err error }

func (s failingStore) WithBoardLock(context.Context, uuid.UUID, boardtx.Func) error {
	return s.err
}

func TestProcessorReportsResultAndBoundedType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		opType string
		err    error
		want   observedOp
	}{
		{"known type forbidden", boardcontent.OpCardCreate, domain.ErrForbidden, observedOp{boardcontent.OpCardCreate, boardcontent.ReasonForbidden}},
		{"unknown type not found", "attacker.supplied", domain.ErrNotFound, observedOp{TypeUnknown, boardcontent.ReasonNotFound}},
		{"validation", boardcontent.OpCardEdit, domain.NewValidationError("title", "bad"), observedOp{boardcontent.OpCardEdit, boardcontent.ReasonValidation}},
		{"other error", boardcontent.OpCardMove, assert.AnError, observedOp{boardcontent.OpCardMove, boardcontent.ReasonInternal}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &opRecorder{}
			p := NewProcessor(failingStore{err: tt.err}, WithMetrics(rec))
			_, err := p.Apply(context.Background(), uuid.New(), uuid.New(), boardcontent.Op{Type: tt.opType, OpID: uuid.New(), Payload: json.RawMessage(`{}`)})
			require.Error(t, err)
			assert.Equal(t, []observedOp{tt.want}, rec.seen)
		})
	}
}

func TestMetricResultClassifiesOutcomes(t *testing.T) {
	t.Parallel()
	assert.Equal(t, ResultApplied, metricResult(boardcontent.OpOutcome{Event: &domain.Event{}}, nil))
	assert.Equal(t, ResultReplayed, metricResult(boardcontent.OpOutcome{Event: &domain.Event{}, Replayed: true}, nil))
	assert.Equal(t, boardcontent.ReasonVersionConflict, metricResult(boardcontent.OpOutcome{Nack: &boardcontent.Nack{Reason: boardcontent.ReasonVersionConflict}}, nil))
}
