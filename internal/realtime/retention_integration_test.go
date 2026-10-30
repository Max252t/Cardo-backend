package realtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
	"github.com/Max252t/Cardo-backend/internal/retention"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestPrunedEventsForceSnapshotAndResyncNack(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	boardID := uuid.New()
	_, err := store.CreateBoardWithOwner(ctx, domain.Board{ID: boardID, Title: "Roadmap", OwnerID: owner.ID})
	require.NoError(t, err)

	env := rttest.NewEnv(t)
	publisher := env.NewPublisher(t)
	env.Ops = ops.NewProcessor(store, ops.WithPublisher(publisher))
	env.Sync = boards.NewService(store, boards.WithPublisher(publisher))
	env.Members = store
	inst := env.StartInstance(t, "a")

	conn := env.DialBoard(t, inst, boardID, owner.ID)
	conn.HandshakeSnapshot()
	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	assert.Equal(t, int64(1), drainUntil(conn, boardcontent.EventColumnCreated).Seq)

	pool := store.Pool()
	_, err = pool.Exec(ctx, `UPDATE board_events SET created_at = now() - interval '10 days' WHERE board_id = $1`, boardID)
	require.NoError(t, err)
	res, err := store.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), 0, 100)
	require.NoError(t, err)
	require.EqualValues(t, 1, res.Deleted)

	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	var nack boardcontent.Nack
	require.NoError(t, json.Unmarshal(drainUntil(conn, protocol.TypeNack).Payload, &nack))
	assert.Equal(t, boardcontent.ReasonResyncRequired, nack.Reason)

	stale := env.DialBoard(t, inst, boardID, owner.ID)
	stale.Hello(int64Ptr(0))
	var snap protocol.Snapshot
	require.NoError(t, json.Unmarshal(stale.Expect(protocol.TypeSyncSnapshot).Payload, &snap))
	assert.Equal(t, int64(1), snap.Seq)
	assert.Len(t, snap.Columns, 1)
	stale.Expect(protocol.TypePresenceList)
	stale.Expect(protocol.TypeSyncReady)
}

func TestRetentionServiceKeepsRecentEventsForDeltaCatchUp(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	boardID := uuid.New()
	_, err := store.CreateBoardWithOwner(ctx, domain.Board{ID: boardID, Title: "Roadmap", OwnerID: owner.ID})
	require.NoError(t, err)

	env := rttest.NewEnv(t)
	publisher := env.NewPublisher(t)
	processor := ops.NewProcessor(store, ops.WithPublisher(publisher))
	env.Ops = processor
	env.Sync = boards.NewService(store, boards.WithPublisher(publisher))
	env.Members = store
	inst := env.StartInstance(t, "a")

	for i := range 6 {
		op, err := boardcontent.NewOp(boardcontent.OpColumnCreate, uuid.New(), boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: string(rune('a' + i))})
		require.NoError(t, err)
		_, err = processor.Apply(ctx, owner.ID, boardID, op)
		require.NoError(t, err)
	}
	_, err = store.Pool().Exec(ctx, `UPDATE board_events SET created_at = now() - interval '10 days' WHERE board_id = $1`, boardID)
	require.NoError(t, err)

	svc, err := retention.New(store, retention.Config{
		Interval:              time.Hour,
		EventsRetention:       7 * 24 * time.Hour,
		EventsKeepMin:         3,
		ProcessedOpsRetention: 24 * time.Hour,
		BatchSize:             2,
	})
	require.NoError(t, err)
	svc.RunOnce(ctx)

	delta := env.DialBoard(t, inst, boardID, owner.ID)
	delta.Hello(int64Ptr(3))
	var d protocol.SyncDelta
	require.NoError(t, json.Unmarshal(delta.Expect(protocol.TypeSyncDelta).Payload, &d))
	assert.Len(t, d.Events, 3)

	stale := env.DialBoard(t, inst, boardID, owner.ID)
	stale.Hello(int64Ptr(2))
	var snap protocol.Snapshot
	require.NoError(t, json.Unmarshal(stale.Expect(protocol.TypeSyncSnapshot).Payload, &snap))
	assert.Equal(t, int64(6), snap.Seq)
}
