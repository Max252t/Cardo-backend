package ops_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type observation struct {
	opType   string
	result   string
	duration time.Duration
}

type opSpy struct {
	mu   sync.Mutex
	seen []observation
}

func (s *opSpy) ObserveOp(opType, result string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, observation{opType, result, d})
}

func (s *opSpy) all() []observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observation(nil), s.seen...)
}

func (s *opSpy) last(t *testing.T) observation {
	t.Helper()
	all := s.all()
	require.NotEmpty(t, all)
	return all[len(all)-1]
}

func (f *fixture) withSpy() *opSpy {
	spy := &opSpy{}
	f.proc = ops.NewProcessor(f.store, ops.WithPublisher(f.pub), ops.WithMetrics(spy))
	return spy
}

type failingPublisher struct{}

func (failingPublisher) Publish(context.Context, []domain.Event) error {
	return errors.New("publish failed")
}

func TestMetricsReportAppliedOperation(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()

	f.mustEvent(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})

	got := spy.last(t)
	assert.Equal(t, boardcontent.OpColumnCreate, got.opType)
	assert.Equal(t, ops.ResultApplied, got.result)
	assert.GreaterOrEqual(t, got.duration, time.Duration(0))
	assert.Len(t, spy.all(), 1)
}

func TestMetricsReportEveryOperationTypeAsApplied(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	other := f.column(t, "Done", nil)
	card := f.card(t, col, "A")
	spy := f.withSpy()

	steps := []struct {
		opType  string
		payload any
	}{
		{boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "New"}},
		{boardcontent.OpColumnRename, boardcontent.ColumnRename{ColumnID: col, Title: "Renamed"}},
		{boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "B"}},
		{boardcontent.OpCardEdit, boardcontent.CardEdit{CardID: card, Title: ptr("A2")}},
		{boardcontent.OpCardMove, boardcontent.CardMove{CardID: card, ToColumnID: other, BaseVersion: ptr(int64(2))}},
		{boardcontent.OpCardArchive, boardcontent.CardArchive{CardID: card}},
		{boardcontent.OpColumnArchive, boardcontent.ColumnArchive{ColumnID: other}},
	}
	for _, s := range steps {
		out := f.apply(t, s.opType, s.payload)
		require.Nil(t, out.Nack, s.opType)
	}

	seen := spy.all()
	require.Len(t, seen, len(steps))
	for i, s := range steps {
		assert.Equal(t, s.opType, seen[i].opType)
		assert.Equal(t, ops.ResultApplied, seen[i].result, s.opType)
	}
}

func TestMetricsReportReplayedOperation(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()
	opID := uuid.New()
	payload := boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Once"}

	first := f.applyAs(t, f.owner.ID, opID, boardcontent.OpColumnCreate, payload)
	again := f.applyAs(t, f.owner.ID, opID, boardcontent.OpColumnCreate, payload)

	require.Nil(t, first.Nack)
	require.True(t, again.Replayed)
	seen := spy.all()
	require.Len(t, seen, 2)
	assert.Equal(t, ops.ResultApplied, seen[0].result)
	assert.Equal(t, ops.ResultReplayed, seen[1].result)
	assert.Equal(t, boardcontent.OpColumnCreate, seen[1].opType)
}

func TestMetricsReportEveryNackReason(t *testing.T) {
	f := newFixture(t)
	w := buildWorld(t, f)
	spy := f.withSpy()

	reasons := map[string]int{}
	for _, tc := range nackCases() {
		before := len(spy.all())
		out := f.applyAs(t, f.owner.ID, uuid.New(), tc.opType, tc.payload(w))
		require.NotNil(t, out.Nack, tc.name)

		seen := spy.all()
		require.Len(t, seen, before+1, tc.name)
		assert.Equal(t, tc.opType, seen[before].opType, tc.name)
		assert.Equal(t, tc.reason, seen[before].result, tc.name)
		reasons[tc.reason]++
	}
	assert.Contains(t, reasons, boardcontent.ReasonValidation)
	assert.Contains(t, reasons, boardcontent.ReasonNotFound)
	assert.Contains(t, reasons, boardcontent.ReasonVersionConflict)
}

func TestMetricsReportReplayedNackWithItsOriginalReason(t *testing.T) {
	f := newFixture(t)
	w := buildWorld(t, f)
	spy := f.withSpy()
	opID := uuid.New()
	payload := boardcontent.CardMove{CardID: w.card, ToColumnID: w.done, BaseVersion: ptr(int64(42))}

	f.applyAs(t, f.owner.ID, opID, boardcontent.OpCardMove, payload)
	again := f.applyAs(t, f.owner.ID, opID, boardcontent.OpCardMove, payload)

	require.True(t, again.Replayed)
	for _, o := range spy.all() {
		assert.Equal(t, boardcontent.OpCardMove, o.opType)
		assert.Equal(t, boardcontent.ReasonVersionConflict, o.result)
	}
	assert.Len(t, spy.all(), 2)
}

func TestMetricsReportForbiddenForViewerAndOutsider(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	viewer := f.member(t, domain.RoleViewer)
	outsider := testutil.CreateUser(t, f.store, testutil.UniqueEmail("outsider"))
	spy := f.withSpy()

	for _, user := range []uuid.UUID{viewer.ID, outsider.ID} {
		out := f.applyAs(t, user, uuid.New(), boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
		require.NotNil(t, out.Nack)
	}

	seen := spy.all()
	require.Len(t, seen, 2)
	for _, o := range seen {
		assert.Equal(t, observation{boardcontent.OpCardCreate, boardcontent.ReasonForbidden, o.duration}, o)
	}
}

func TestMetricsReportValidationNackForMalformedPayload(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()

	out := f.applyRaw(t, f.owner.ID, boardcontent.OpColumnArchive, `{"column_id":"abc"}`)

	require.NotNil(t, out.Nack)
	got := spy.last(t)
	assert.Equal(t, boardcontent.OpColumnArchive, got.opType)
	assert.Equal(t, boardcontent.ReasonValidation, got.result)
}

func TestMetricsBoundUnknownOperationTypes(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()

	for _, opType := range []string{"op.column.explode", boardcontent.EventCardCreated, "attacker-" + uuid.NewString()} {
		out := f.applyRaw(t, f.owner.ID, opType, `{}`)
		require.NotNil(t, out.Nack, opType)
	}

	seen := spy.all()
	require.Len(t, seen, 3)
	for _, o := range seen {
		assert.Equal(t, ops.TypeUnknown, o.opType)
		assert.Equal(t, boardcontent.ReasonValidation, o.result)
	}
}

func TestMetricsReportNotFoundErrorForDeletedBoard(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	_, err := f.pool.Exec(context.Background(), `UPDATE boards SET deleted_at = now() WHERE id = $1`, f.board.ID)
	require.NoError(t, err)
	spy := f.withSpy()

	op := mustOp(t, boardcontent.OpCardCreate, uuid.New(), boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	_, err = f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)

	require.ErrorIs(t, err, domain.ErrNotFound)
	got := spy.last(t)
	assert.Equal(t, boardcontent.OpCardCreate, got.opType)
	assert.Equal(t, boardcontent.ReasonNotFound, got.result)
}

func TestMetricsReportNotFoundErrorForUnknownBoard(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()

	op := mustOp(t, boardcontent.OpColumnCreate, uuid.New(), boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "x"})
	_, err := f.proc.Apply(context.Background(), f.owner.ID, uuid.New(), op)

	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Equal(t, boardcontent.ReasonNotFound, spy.last(t).result)
}

func TestMetricsAreNotObservedForNilOpID(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()

	_, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, boardcontent.Op{Type: boardcontent.OpColumnCreate})

	require.Error(t, err)
	assert.Empty(t, spy.all())
}

func TestMetricsReportInternalOnCanceledContext(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	op := mustOp(t, boardcontent.OpColumnCreate, uuid.New(), boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "x"})
	_, err := f.proc.Apply(ctx, f.owner.ID, f.board.ID, op)

	require.Error(t, err)
	got := spy.last(t)
	assert.Equal(t, ops.ResultInternal, got.result)
	assert.Equal(t, boardcontent.OpColumnCreate, got.opType)
}

func TestMetricsStayAppliedWhenPublishingFails(t *testing.T) {
	f := newFixture(t)
	spy := &opSpy{}
	f.proc = ops.NewProcessor(f.store, ops.WithPublisher(failingPublisher{}), ops.WithMetrics(spy))

	out := f.applyAs(t, f.owner.ID, uuid.New(), boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "x"})

	require.Nil(t, out.Nack)
	require.NotNil(t, out.Event)
	assert.Equal(t, ops.ResultApplied, spy.last(t).result)
	assert.Len(t, spy.all(), 1)
}

func TestMetricsCountEveryConcurrentOperation(t *testing.T) {
	f := newFixture(t)
	spy := f.withSpy()
	const n = 12

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := boardcontent.NewOp(boardcontent.OpColumnCreate, uuid.New(), boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: string(rune('A' + i))})
			assert.NoError(t, err)
			_, err = f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	seen := spy.all()
	require.Len(t, seen, n)
	for _, o := range seen {
		assert.Equal(t, observation{boardcontent.OpColumnCreate, ops.ResultApplied, o.duration}, o)
		assert.GreaterOrEqual(t, o.duration, time.Duration(0))
	}
}

func TestProcessorWithoutMetricsOptionStillApplies(t *testing.T) {
	f := newFixture(t)

	out := f.applyAs(t, f.owner.ID, uuid.New(), boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "x"})

	require.Nil(t, out.Nack)
	require.NotNil(t, out.Event)
}
