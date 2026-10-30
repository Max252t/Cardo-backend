package realtime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
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

type ctxRecorder struct {
	inner interface {
		Apply(ctx context.Context, userID, boardID uuid.UUID, op boardcontent.Op) (boardcontent.OpOutcome, error)
	}
	entered  chan struct{}
	deadline chan time.Time
	wait     func(ctx context.Context) error
	once     sync.Once
}

func (r *ctxRecorder) Apply(ctx context.Context, userID, boardID uuid.UUID, op boardcontent.Op) (boardcontent.OpOutcome, error) {
	if d, ok := ctx.Deadline(); ok {
		r.deadline <- d
	} else {
		r.deadline <- time.Time{}
	}
	r.once.Do(func() { close(r.entered) })
	if r.wait != nil {
		if err := r.wait(ctx); err != nil {
			return boardcontent.OpOutcome{}, err
		}
	}
	return r.inner.Apply(ctx, userID, boardID, op)
}

func newCtxRecorder() *ctxRecorder {
	return &ctxRecorder{entered: make(chan struct{}), deadline: make(chan time.Time, 8)}
}

func TestDetachedOperationIsStillPublishedToOtherClients(t *testing.T) {
	gate := &gatedApplier{entered: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1)}
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		gate.inner = e.Board
		e.Ops = gate
	})
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	sender := env.DialBoard(t, inst, env.Board.ID, ann)
	sender.HandshakeSnapshot()
	peer := env.DialBoard(t, inst, env.Board.ID, bob)
	peer.HandshakeSnapshot()

	opID := uuid.New()
	colID := uuid.New()
	sender.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: colID, Title: "Todo"})
	select {
	case <-gate.entered:
	case <-time.After(eventually):
		t.Fatal("the operation never reached the applier")
	}
	require.NoError(t, sender.Close())
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		appendEvents(t, env, ann, 1)
		time.Sleep(10 * time.Millisecond)
	}
	close(gate.release)

	var got protocol.Envelope
	for got.Type != "column.create" {
		got = peer.MustNext()
	}
	require.NotNil(t, got.OpID)
	assert.Equal(t, opID, *got.OpID)
	assert.NoError(t, <-gate.ctxErr)
	assert.Equal(t, 1, env.Board.ApplyCount())
}

func TestOperationContextCarriesTheOpTimeoutDeadline(t *testing.T) {
	rec := newCtxRecorder()
	const timeout = 3 * time.Second
	env, user, inst := newEditorEnv(t, func(e *rttest.Env) {
		rec.inner = e.Board
		e.Ops = rec
		e.Config = realtime.Config{OpTimeout: timeout}
	})
	conn := env.DialBoard(t, inst, env.Board.ID, user)
	conn.HandshakeSnapshot()

	before := time.Now()
	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	conn.Expect("column.create")
	deadline := <-rec.deadline
	require.False(t, deadline.IsZero(), "the operation context must have a deadline")
	assert.WithinDuration(t, before.Add(timeout), deadline, time.Second)
}

func TestStuckOperationIsBoundedByOpTimeoutEvenAfterTheClientLeaves(t *testing.T) {
	rec := newCtxRecorder()
	finished := make(chan error, 1)
	rec.wait = func(ctx context.Context) error {
		<-ctx.Done()
		finished <- ctx.Err()
		return ctx.Err()
	}
	env, user, inst := newEditorEnv(t, func(e *rttest.Env) {
		rec.inner = e.Board
		e.Ops = rec
		e.Config = realtime.Config{OpTimeout: 400 * time.Millisecond}
	})
	conn := env.DialBoard(t, inst, env.Board.ID, user)
	conn.HandshakeSnapshot()
	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	<-rec.entered
	started := time.Now()
	require.NoError(t, conn.Close())
	traffic := make(chan struct{})
	go func() {
		defer close(traffic)
		for time.Since(started) < 300*time.Millisecond {
			_, _ = env.Board.Append(context.Background(), boardcontent.EventCardCreated, map[string]string{}, user)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	defer func() { <-traffic }()

	select {
	case err := <-finished:
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.GreaterOrEqual(t, time.Since(started), 250*time.Millisecond, "a closed connection must not cancel the operation early")
	case <-time.After(eventually):
		t.Fatal("the operation outlived OpTimeout")
	}
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestTimedOutOperationIsNackedAsInternalWhileTheClientStaysConnected(t *testing.T) {
	rec := newCtxRecorder()
	rec.wait = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	env, user, inst := newEditorEnv(t, func(e *rttest.Env) {
		rec.inner = e.Board
		e.Ops = rec
		e.Config = realtime.Config{OpTimeout: 300 * time.Millisecond}
	})
	conn := env.DialBoard(t, inst, env.Board.ID, user)
	conn.HandshakeSnapshot()
	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	nack := conn.Expect(protocol.TypeNack)
	require.NotNil(t, nack.OpID)
	assert.Equal(t, opID, *nack.OpID)
	assert.Equal(t, boardcontent.ReasonInternal, decodeAs[boardcontent.Nack](t, nack).Reason)
}

func TestShutdownWaitsForTheInFlightOperationToFinish(t *testing.T) {
	gate := &gatedApplier{entered: make(chan struct{}), release: make(chan struct{}), ctxErr: make(chan error, 1)}
	env, user, inst := newEditorEnv(t, func(e *rttest.Env) {
		gate.inner = e.Board
		e.Ops = gate
	})
	conn := env.DialBoard(t, inst, env.Board.ID, user)
	conn.HandshakeSnapshot()
	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	<-gate.entered
	closed := make(chan *websocket.CloseError, 1)
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				var closeErr *websocket.CloseError
				if errors.As(err, &closeErr) {
					closed <- closeErr
				}
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- inst.Hub.Shutdown(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("shutdown returned while an operation was in flight: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(gate.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(eventually):
		t.Fatal("shutdown did not finish after the operation completed")
	}
	assert.NoError(t, <-gate.ctxErr, "shutdown must not cancel a committing operation")
	assert.Equal(t, int64(1), env.Board.Seq())
	select {
	case closeErr := <-closed:
		assert.Equal(t, protocol.CloseGoingAway, closeErr.Code)
	case <-time.After(eventually):
		t.Fatal("the client was never told to go away")
	}
}

func TestShutdownReturnsAfterOpTimeoutWhenAnOperationIsStuck(t *testing.T) {
	rec := newCtxRecorder()
	rec.wait = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	env, user, inst := newEditorEnv(t, func(e *rttest.Env) {
		rec.inner = e.Board
		e.Ops = rec
		e.Config = realtime.Config{OpTimeout: 600 * time.Millisecond}
	})
	conn := env.DialBoard(t, inst, env.Board.ID, user)
	conn.HandshakeSnapshot()
	opID := uuid.New()
	conn.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})
	<-rec.entered

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := inst.Hub.Shutdown(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), 5*time.Second, "a stuck operation must not hang shutdown beyond OpTimeout")
}
