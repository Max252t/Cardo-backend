package ops_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
)

type observed struct {
	opType string
	result string
}

type metricsSpy struct {
	mu   sync.Mutex
	seen []observed
}

func (m *metricsSpy) ObserveOp(opType, result string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, observed{opType, result})
}

func (m *metricsSpy) results() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.seen))
	for _, o := range m.seen {
		out = append(out, o.result)
	}
	return out
}

func (f *fixture) ageAll(t *testing.T) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), `UPDATE board_events SET created_at = now() - interval '10 days'`)
	require.NoError(t, err)
	_, err = f.pool.Exec(context.Background(), `UPDATE processed_ops SET created_at = now() - interval '3 days'`)
	require.NoError(t, err)
}

func (f *fixture) pruneEvents(t *testing.T, keepMin int) int64 {
	t.Helper()
	res, err := f.store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), keepMin, 100)
	require.NoError(t, err)
	return res.Deleted
}

func (f *fixture) pruneOps(t *testing.T) int64 {
	t.Helper()
	res, err := f.store.PruneProcessedOps(context.Background(), time.Now().Add(-24*time.Hour), 100)
	require.NoError(t, err)
	return res.Deleted
}

func TestPrunedReplayIsCountedAsResyncRequiredAndDoesNotChangeTheBoard(t *testing.T) {
	f := newFixture(t)
	spy := &metricsSpy{}
	proc := ops.NewProcessor(f.store, ops.WithMetrics(spy))
	opID := uuid.New()
	payload := boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"}
	op := mustOp(t, boardcontent.OpColumnCreate, opID, payload)
	_, err := proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	f.ageAll(t)
	require.EqualValues(t, 1, f.pruneEvents(t, 0))
	seq := f.boardSeq(t)

	for range 3 {
		_, err = proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
		require.ErrorIs(t, err, domain.ErrEventPruned)
	}

	assert.Equal(t, seq, f.boardSeq(t))
	assert.Equal(t, []string{ops.ResultApplied, "resync_required", "resync_required", "resync_required"}, spy.results())
	events, _ := f.processedOps(t)
	assert.Equal(t, 1, events, "the stored outcome stays until processed ops are pruned")
}

func TestReplayOfNackSurvivesEventPruning(t *testing.T) {
	f := newFixture(t)
	opID := uuid.New()
	op := mustOp(t, boardcontent.OpCardMove, opID, boardcontent.CardMove{CardID: uuid.New(), ToColumnID: uuid.New(), BaseVersion: ptr(int64(1))})
	first, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	require.NotNil(t, first.Nack)

	f.ageAll(t)
	f.pruneEvents(t, 0)

	again, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	require.NotNil(t, again.Nack)
	assert.Equal(t, first.Nack.Reason, again.Nack.Reason)
}

func TestReplayStillWorksWhenEventIsInsideTheKeepWindow(t *testing.T) {
	f := newFixture(t)
	opID := uuid.New()
	payload := boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"}
	first := f.applyAs(t, f.owner.ID, opID, boardcontent.OpColumnCreate, payload)
	f.ageAll(t)
	require.Zero(t, f.pruneEvents(t, 10))

	op := mustOp(t, boardcontent.OpColumnCreate, opID, payload)
	again, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Event.Seq, again.Event.Seq)
}

func TestResendAfterProcessedOpIsPrunedIsTreatedAsANewOperation(t *testing.T) {
	f := newFixture(t)
	opID := uuid.New()
	colID := uuid.New()
	payload := boardcontent.ColumnCreate{ColumnID: colID, Title: "Todo"}
	f.applyAs(t, f.owner.ID, opID, boardcontent.OpColumnCreate, payload)
	f.ageAll(t)
	require.EqualValues(t, 1, f.pruneEvents(t, 0))
	require.EqualValues(t, 1, f.pruneOps(t))

	op := mustOp(t, boardcontent.OpColumnCreate, opID, payload)
	out, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)

	assert.False(t, out.Replayed)
	require.NotNil(t, out.Nack, "the column id from the first attempt still exists, so the retry is rejected instead of duplicating it")
	assert.Len(t, f.columnOrder(t), 1)
	assert.EqualValues(t, 1, f.boardSeq(t))
}

func TestResendAfterProcessedOpIsPrunedIsRejectedByTheVersionCheck(t *testing.T) {
	f := newFixture(t)
	opID := uuid.New()
	first := f.column(t, "A", nil)
	second := f.column(t, "B", &first)
	payload := boardcontent.ColumnMove{ColumnID: first, AfterID: &second, BaseVersion: ptr(int64(1))}
	out := f.applyAs(t, f.owner.ID, opID, boardcontent.OpColumnMove, payload)
	require.NotNil(t, out.Event)
	f.ageAll(t)
	f.pruneEvents(t, 0)
	f.pruneOps(t)

	again, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, mustOp(t, boardcontent.OpColumnMove, opID, payload))
	require.NoError(t, err)
	assert.False(t, again.Replayed)
	require.NotNil(t, again.Nack)
	assert.Equal(t, boardcontent.ReasonVersionConflict, again.Nack.Reason)
	assert.EqualValues(t, 2, f.columnRow(t, first).Version)
}

func TestProcessedOpsOfOtherBoardsAreNotReplayedAcrossBoards(t *testing.T) {
	f := newFixture(t)
	other := f.newBoard(t, f.owner.ID)
	opID := uuid.New()
	payload := boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"}
	f.applyOnBoard(t, other, f.owner.ID, opID, boardcontent.OpColumnCreate, payload)
	f.ageAll(t)
	f.pruneEvents(t, 0)

	op := mustOp(t, boardcontent.OpColumnCreate, opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	_, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	assert.NotErrorIs(t, err, domain.ErrEventPruned)
}
