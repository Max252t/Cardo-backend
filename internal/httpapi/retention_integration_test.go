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

func TestFlowEventLogGoneAfterRetentionRun(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	for i := range 8 {
		b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: fmt.Sprintf("c%d", i)})
	}
	_, err := b.pool.Exec(context.Background(), `UPDATE board_events SET created_at = now() - interval '10 days' WHERE board_id = $1`, b.boardID)
	require.NoError(t, err)

	svc, err := retention.New(b.store, retention.Config{
		Interval:              time.Hour,
		EventsRetention:       7 * 24 * time.Hour,
		EventsKeepMin:         3,
		ProcessedOpsRetention: 24 * time.Hour,
		BatchSize:             2,
	})
	require.NoError(t, err)
	svc.RunOnce(context.Background())

	for _, after := range []int{0, 1, 4} {
		b.expectError(t, http.MethodGet, b.path(fmt.Sprintf("/events?after_seq=%d", after)), "", b.owner.access, http.StatusGone, httpapi.CodeGone)
	}
	page := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=5"), "", b.owner.access, http.StatusOK)
	assert.Len(t, page["events"], 3)

	snap := b.mustJSON(t, http.MethodGet, b.path(""), "", b.owner.access, http.StatusOK)
	assert.EqualValues(t, 8, snap["seq"])
	assert.Len(t, snap["columns"], 8)
}
