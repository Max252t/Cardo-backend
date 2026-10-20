package realtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

func TestRealtimeFlowAgainstPostgresAcrossTwoInstances(t *testing.T) {
	store := testutil.NewStore(t)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	boardID, err := uuid.NewV7()
	require.NoError(t, err)
	_, err = store.CreateBoardWithOwner(ctx, domain.Board{ID: boardID, Title: "Roadmap", OwnerID: owner.ID})
	require.NoError(t, err)

	env := rttest.NewEnv(t)
	publisher := env.NewPublisher(t)
	boardService := boards.NewService(store, boards.WithPublisher(publisher))
	env.Ops = ops.NewProcessor(store, ops.WithPublisher(publisher))
	env.Sync = boardService
	env.Members = store
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")

	first := env.DialBoard(t, a, boardID, owner.ID)
	snap := first.HandshakeSnapshot()
	assert.Equal(t, boardID, snap.Board.ID)
	assert.Equal(t, domain.RoleOwner, snap.Board.Role)
	second := env.DialBoard(t, b, boardID, owner.ID)
	second.HandshakeSnapshot()

	columnID := uuid.New()
	columnOp := uuid.New()
	first.SendJSON(protocol.OpColumnCreate, &columnOp, protocol.ColumnCreate{ColumnID: columnID, Title: "Todo"})
	for _, c := range []*rttest.Conn{first, second} {
		ev := drainUntil(c, protocol.EventColumnCreated)
		assert.Equal(t, int64(1), ev.Seq)
		require.NotNil(t, ev.OpID)
		assert.Equal(t, columnOp, *ev.OpID)
	}

	cardOp := uuid.New()
	first.SendJSON(protocol.OpCardCreate, &cardOp, protocol.CardCreate{CardID: uuid.New(), ColumnID: columnID, Title: "Write tests"})
	for _, c := range []*rttest.Conn{first, second} {
		assert.Equal(t, int64(2), drainUntil(c, protocol.EventCardCreated).Seq)
	}

	missing := uuid.New()
	badOp := uuid.New()
	first.SendJSON(protocol.OpCardArchive, &badOp, protocol.CardArchive{CardID: missing})
	var nack protocol.Nack
	require.NoError(t, json.Unmarshal(drainUntil(first, protocol.TypeNack).Payload, &nack))
	assert.Equal(t, protocol.ReasonNotFound, nack.Reason)

	late := env.DialBoard(t, b, boardID, owner.ID)
	late.Hello(int64Ptr(1))
	var delta protocol.SyncDelta
	require.NoError(t, json.Unmarshal(late.Expect(protocol.TypeSyncDelta).Payload, &delta))
	require.Len(t, delta.Events, 1)
	assert.Equal(t, protocol.EventCardCreated, delta.Events[0].Type)
	late.Expect(protocol.TypePresenceList)
	late.Expect(protocol.TypeSyncReady)

	_, err = boardService.Delete(ctx, owner.ID, boardID)
	require.NoError(t, err)
	for _, c := range []*rttest.Conn{first, second, late} {
		assert.Equal(t, protocol.EventBoardDeleted, drainUntil(c, protocol.EventBoardDeleted).Type)
		assert.Equal(t, protocol.CloseBoardDeleted, c.ExpectClose().Code)
	}
}
