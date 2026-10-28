package ops_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

func TestReplayOfPrunedEventReportsEventPruned(t *testing.T) {
	f := newFixture(t)
	opID := uuid.New()
	payload := boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"}
	require.NotNil(t, f.applyAs(t, f.owner.ID, opID, boardcontent.OpColumnCreate, payload).Event)

	_, err := f.pool.Exec(context.Background(), `UPDATE board_events SET created_at = now() - interval '10 days' WHERE board_id = $1`, f.board.ID)
	require.NoError(t, err)
	res, err := f.store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 0, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Deleted)

	op, err := boardcontent.NewOp(boardcontent.OpColumnCreate, opID, payload)
	require.NoError(t, err)
	_, err = f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.ErrorIs(t, err, domain.ErrEventPruned)
	assert.NotErrorIs(t, err, domain.ErrNotFound)
}
