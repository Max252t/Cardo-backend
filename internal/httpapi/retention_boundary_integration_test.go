package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/retention"
)

func (b *boardFlow) seedAndPrune(t *testing.T, events, keepMin int) {
	t.Helper()
	for i := range events {
		b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: fmt.Sprintf("c%d", i)})
	}
	_, err := b.pool.Exec(context.Background(), `UPDATE board_events SET created_at = now() - interval '10 days' WHERE board_id = $1`, b.boardID)
	require.NoError(t, err)
	svc, err := retention.New(b.store, retention.Config{
		Interval:              time.Hour,
		EventsRetention:       7 * 24 * time.Hour,
		EventsKeepMin:         keepMin,
		ProcessedOpsRetention: 24 * time.Hour,
		BatchSize:             2,
	})
	require.NoError(t, err)
	svc.RunOnce(context.Background())
}

func TestFlowEventLogBoundaryAtTheOldestRetainedSeq(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	b.seedAndPrune(t, 10, 4)

	for after := 0; after <= 5; after++ {
		b.expectError(t, http.MethodGet, b.path(fmt.Sprintf("/events?after_seq=%d", after)), "", b.owner.access, http.StatusGone, httpapi.CodeGone)
	}
	for after, want := range map[int]int{6: 4, 7: 3, 8: 2, 9: 1, 10: 0} {
		page := b.mustJSON(t, http.MethodGet, b.path(fmt.Sprintf("/events?after_seq=%d", after)), "", b.owner.access, http.StatusOK)
		assert.Len(t, page["events"], want, "after_seq=%d", after)
		assert.EqualValues(t, 10, page["seq"])
	}
	first := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=6"), "", b.owner.access, http.StatusOK)["events"].([]any)[0].(map[string]any)
	assert.EqualValues(t, 7, first["seq"])
}

func TestFlowEventLogGoneBodyMentionsSnapshot(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	b.seedAndPrune(t, 6, 2)

	rec := b.expectError(t, http.MethodGet, b.path("/events?after_seq=0"), "", b.owner.access, http.StatusGone, httpapi.CodeGone)
	_, message := rec.errorBody(t)
	assert.Contains(t, message, "snapshot")
}

func TestFlowEventLogAfterEverythingWasPruned(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	b.seedAndPrune(t, 5, 0)

	b.expectError(t, http.MethodGet, b.path("/events?after_seq=0"), "", b.owner.access, http.StatusGone, httpapi.CodeGone)
	b.expectError(t, http.MethodGet, b.path("/events?after_seq=4"), "", b.owner.access, http.StatusGone, httpapi.CodeGone)
	page := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=5"), "", b.owner.access, http.StatusOK)
	assert.Empty(t, page["events"])

	b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "again"})
	page = b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=5"), "", b.owner.access, http.StatusOK)
	assert.Len(t, page["events"], 1)
	b.expectError(t, http.MethodGet, b.path("/events?after_seq=4"), "", b.owner.access, http.StatusGone, httpapi.CodeGone)
}

func TestFlowSnapshotStaysCompleteAfterPruning(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	b.seedAndPrune(t, 9, 0)

	snap := b.mustJSON(t, http.MethodGet, b.path(""), "", b.owner.access, http.StatusOK)
	assert.EqualValues(t, 9, snap["seq"])
	assert.Len(t, snap["columns"], 9)
}

func TestFlowEventLogGoneForOtherMembersToo(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	viewer := b.register(t, "viewer")
	b.addMember(t, viewer, "viewer")
	b.seedAndPrune(t, 6, 3)

	b.expectError(t, http.MethodGet, b.path("/events?after_seq=0"), "", viewer.access, http.StatusGone, httpapi.CodeGone)
	page := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=3"), "", viewer.access, http.StatusOK)
	assert.Len(t, page["events"], 3)
	stranger := b.register(t, "stranger")
	b.expectError(t, http.MethodGet, b.path("/events?after_seq=0"), "", stranger.access, http.StatusNotFound, httpapi.CodeNotFound)
}
