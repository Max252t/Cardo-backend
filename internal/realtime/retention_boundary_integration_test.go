package realtime_test

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
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type opResults struct {
	mu   sync.Mutex
	seen []string
}

func (o *opResults) ObserveOp(_, result string, _ time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, result)
}

func (o *opResults) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.seen...)
}

type retentionWorld struct {
	store   *storage.Store
	owner   domain.User
	boardID uuid.UUID
	env     *rttest.Env
	inst    *rttest.Instance
	proc    *ops.Processor
	results *opResults
}

func newRetentionWorld(t *testing.T) *retentionWorld {
	t.Helper()
	store := testutil.NewStore(t)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	boardID := uuid.New()
	_, err := store.CreateBoardWithOwner(ctx, domain.Board{ID: boardID, Title: "Roadmap", OwnerID: owner.ID})
	require.NoError(t, err)

	env := rttest.NewEnv(t)
	publisher := env.NewPublisher(t)
	results := &opResults{}
	proc := ops.NewProcessor(store, ops.WithPublisher(publisher), ops.WithMetrics(results))
	env.Ops = proc
	env.Sync = boards.NewService(store, boards.WithPublisher(publisher))
	env.Members = store
	return &retentionWorld{store: store, owner: owner, boardID: boardID, env: env, inst: env.StartInstance(t, "a"), proc: proc, results: results}
}

func (w *retentionWorld) addColumns(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		op, err := boardcontent.NewOp(boardcontent.OpColumnCreate, uuid.New(), boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: string(rune('a' + i))})
		require.NoError(t, err)
		_, err = w.proc.Apply(context.Background(), w.owner.ID, w.boardID, op)
		require.NoError(t, err)
	}
}

func (w *retentionWorld) prune(t *testing.T, keepMin int) int64 {
	t.Helper()
	ctx := context.Background()
	_, err := w.store.Pool().Exec(ctx, `UPDATE board_events SET created_at = now() - interval '10 days' WHERE board_id = $1`, w.boardID)
	require.NoError(t, err)
	res, err := w.store.PruneEvents(ctx, time.Now().Add(-7*24*time.Hour), keepMin, 3)
	require.NoError(t, err)
	return res.Deleted
}

func TestPrunedReplayKeepsTheSessionOpenAndLaterOpsWork(t *testing.T) {
	w := newRetentionWorld(t)
	conn := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	conn.HandshakeSnapshot()

	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	drainUntil(conn, boardcontent.EventColumnCreated)
	require.EqualValues(t, 1, w.prune(t, 0))

	for range 3 {
		conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
		var nack boardcontent.Nack
		env := drainUntil(conn, protocol.TypeNack)
		require.NoError(t, json.Unmarshal(env.Payload, &nack))
		assert.Equal(t, boardcontent.ReasonResyncRequired, nack.Reason)
		require.NotNil(t, env.OpID)
		assert.Equal(t, opID, *env.OpID)
		assert.NotEmpty(t, nack.Message)
	}

	fresh := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &fresh, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Next"})
	ev := drainUntil(conn, boardcontent.EventColumnCreated)
	assert.EqualValues(t, 2, ev.Seq)

	assert.Equal(t, []string{ops.ResultApplied, "resync_required", "resync_required", "resync_required", ops.ResultApplied}, w.results.snapshot())
}

func TestPrunedReplayDoesNotCloseOtherSessionsOnTheBoard(t *testing.T) {
	w := newRetentionWorld(t)
	sender := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	sender.HandshakeSnapshot()
	watcher := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	watcher.HandshakeSnapshot()

	opID := uuid.New()
	sender.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	drainUntil(sender, boardcontent.EventColumnCreated)
	drainUntil(watcher, boardcontent.EventColumnCreated)
	w.prune(t, 0)

	sender.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	drainUntil(sender, protocol.TypeNack)

	next := uuid.New()
	watcher.SendJSON(boardcontent.OpColumnCreate, &next, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Later"})
	drainUntil(watcher, boardcontent.EventColumnCreated)
	drainUntil(sender, boardcontent.EventColumnCreated)
}

func TestHelloLastSeqAtTheRetentionBoundary(t *testing.T) {
	w := newRetentionWorld(t)
	w.addColumns(t, 8)
	require.EqualValues(t, 5, w.prune(t, 3))

	cases := []struct {
		name      string
		lastSeq   int64
		snapshot  bool
		wantDelta int
	}{
		{"seq zero", 0, true, 0},
		{"two below boundary", 3, true, 0},
		{"oldest minus one", 5, false, 3},
		{"oldest", 6, false, 2},
		{"head", 8, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
			conn.Hello(int64Ptr(tc.lastSeq))
			if tc.snapshot {
				var snap protocol.Snapshot
				require.NoError(t, json.Unmarshal(conn.Expect(protocol.TypeSyncSnapshot).Payload, &snap))
				assert.EqualValues(t, 8, snap.Seq)
				assert.Len(t, snap.Columns, 8)
				return
			}
			var d protocol.SyncDelta
			require.NoError(t, json.Unmarshal(conn.Expect(protocol.TypeSyncDelta).Payload, &d))
			assert.Len(t, d.Events, tc.wantDelta)
		})
	}
}

func TestHelloAheadOfTheBoardFallsBackToSnapshotAfterPruning(t *testing.T) {
	w := newRetentionWorld(t)
	w.addColumns(t, 6)
	w.prune(t, 2)

	conn := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	conn.Hello(int64Ptr(500))
	var snap protocol.Snapshot
	require.NoError(t, json.Unmarshal(conn.Expect(protocol.TypeSyncSnapshot).Payload, &snap))
	assert.EqualValues(t, 6, snap.Seq)
}

func TestHelloWhenEveryEventIsPrunedUsesSnapshotUnlessAtHead(t *testing.T) {
	w := newRetentionWorld(t)
	w.addColumns(t, 4)
	require.EqualValues(t, 4, w.prune(t, 0))

	stale := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	stale.Hello(int64Ptr(2))
	var snap protocol.Snapshot
	require.NoError(t, json.Unmarshal(stale.Expect(protocol.TypeSyncSnapshot).Payload, &snap))
	assert.EqualValues(t, 4, snap.Seq)
	assert.Len(t, snap.Columns, 4)

	head := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	head.Hello(int64Ptr(4))
	var d protocol.SyncDelta
	require.NoError(t, json.Unmarshal(head.Expect(protocol.TypeSyncDelta).Payload, &d))
	assert.Empty(t, d.Events)
}

func TestNewEventsAfterPruningStillFormAContinuousDelta(t *testing.T) {
	w := newRetentionWorld(t)
	w.addColumns(t, 6)
	w.prune(t, 2)
	w.addColumns(t, 2)

	conn := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	conn.Hello(int64Ptr(5))
	var d protocol.SyncDelta
	require.NoError(t, json.Unmarshal(conn.Expect(protocol.TypeSyncDelta).Payload, &d))
	require.Len(t, d.Events, 3)
	assert.EqualValues(t, 6, d.Events[0].Seq)
	assert.EqualValues(t, 8, d.Events[2].Seq)

	old := w.env.DialBoard(t, w.inst, w.boardID, w.owner.ID)
	old.Hello(int64Ptr(3))
	old.Expect(protocol.TypeSyncSnapshot)
}
