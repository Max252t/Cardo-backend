package realtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

type gatedApplier struct {
	inner interface {
		Apply(ctx context.Context, userID, boardID uuid.UUID, op boardcontent.Op) (boardcontent.OpOutcome, error)
	}
	entered chan struct{}
	release chan struct{}
	ctxErr  chan error
	once    sync.Once
}

func (g *gatedApplier) Apply(ctx context.Context, userID, boardID uuid.UUID, op boardcontent.Op) (boardcontent.OpOutcome, error) {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	g.ctxErr <- ctx.Err()
	return g.inner.Apply(ctx, userID, boardID, op)
}

func TestInFlightOperationSurvivesTheClientDisconnecting(t *testing.T) {
	gate := &gatedApplier{entered: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1)}
	env, user, inst := newEditorEnv(t, func(e *rttest.Env) {
		gate.inner = e.Board
		e.Ops = gate
	})
	conn := env.DialBoard(t, inst, env.Board.ID, user)
	conn.HandshakeSnapshot()

	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	select {
	case <-gate.entered:
	case <-time.After(eventually):
		t.Fatal("the operation never reached the applier")
	}

	require.NoError(t, conn.Close())
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		appendEvents(t, env, user, 1)
		time.Sleep(10 * time.Millisecond)
	}
	close(gate.release)

	select {
	case err := <-gate.ctxErr:
		assert.NoError(t, err, "the commit must not be torn by the client going away")
	case <-time.After(eventually):
		t.Fatal("the applier never finished")
	}
}
