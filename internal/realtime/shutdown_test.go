package realtime_test

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func TestShutdownClosesEveryConnectionOnEveryBoardWith1001(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	mb := multiBoard{boards: map[uuid.UUID]*rttest.Board{env.Board.ID: env.Board}}
	second := addBoard(t, env, mb, ann)
	env.Ops, env.Sync, env.Members = mb, mb, mb
	inst := env.StartInstance(t, "a")

	var conns []*rttest.Conn
	for _, board := range []uuid.UUID{env.Board.ID, env.Board.ID, second.ID, second.ID, second.ID} {
		c := env.DialBoard(t, inst, board, ann)
		c.HandshakeSnapshot()
		conns = append(conns, c)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- inst.Hub.Shutdown(ctx) }()
	for i, c := range conns {
		assert.Equal(t, protocol.CloseGoingAway, c.ExpectClose().Code, i)
	}
	require.NoError(t, <-done)
	assert.Zero(t, inst.Hub.ConnectionCount(env.Board.ID))
	assert.Zero(t, inst.Hub.ConnectionCount(second.ID))
	assert.False(t, inst.Broker.Subscribed(protocol.BoardChannel(env.Board.ID)))
	assert.False(t, inst.Broker.Subscribed(protocol.BoardChannel(second.ID)))
}

func TestShutdownIsIdempotentAndWorksWithoutConnections(t *testing.T) {
	t.Parallel()
	_, _, inst := newEditorEnv(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, inst.Hub.Shutdown(ctx))
	require.NoError(t, inst.Hub.Shutdown(ctx))
}

func TestNewConnectionsAfterShutdownAreToldToGoAway(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, inst.Hub.Shutdown(ctx))

	for range 3 {
		conn, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
		if err != nil {
			continue
		}
		_, _, readErr := conn.ReadMessage()
		var closeErr *websocket.CloseError
		require.ErrorAs(t, readErr, &closeErr)
		assert.Equal(t, protocol.CloseGoingAway, closeErr.Code)
		_ = conn.Close()
	}
	assert.Zero(t, inst.Hub.ConnectionCount(env.Board.ID))
}

func TestShutdownGivesUpWhenAClientNeverAcknowledgesTheClose(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := inst.Hub.Shutdown(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestShutdownWaitsForTheCloseGraceThenFinishes(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{CloseGrace: time.Second, PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- inst.Hub.Shutdown(ctx) }()

	require.Eventually(t, func() bool {
		env.Clock.Advance(time.Second)
		select {
		case err := <-done:
			require.NoError(t, err)
			return true
		default:
			return false
		}
	}, eventually, 10*time.Millisecond)
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestShutdownRacingWithNewConnectionsNeverHangsOrLeaksConnections(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	stop := make(chan struct{})
	var dialers sync.WaitGroup
	var connected atomic.Int64
	for range 6 {
		dialers.Add(1)
		go func() {
			defer dialers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				conn, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
				if err != nil {
					continue
				}
				connected.Add(1)
				_ = conn.Close()
			}
		}()
	}
	require.Eventually(t, func() bool { return connected.Load() >= 5 }, eventually, 5*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, inst.Hub.Shutdown(ctx))
	close(stop)
	dialers.Wait()
	waitConnections(t, inst, env.Board.ID, 0)
	waitSubscribed(t, inst, env.Board.ID, false)
}

func TestEventsDispatchedWhileShuttingDownDoNotPanic(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	var clients []*rttest.Conn
	for range 4 {
		c := inst.Dial(t, ann)
		c.HandshakeSnapshot()
		clients = append(clients, c)
	}
	closed := make(chan int, len(clients))
	for _, c := range clients {
		go func() { closed <- c.ExpectClose().Code }()
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var seq int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			seq++
			payload := []byte(`{"v":1,"type":"card.created","seq":` + strconv.FormatInt(seq, 10) + `,"payload":{}}`)
			inst.Hub.Dispatch(protocol.BoardChannel(env.Board.ID), payload)
			inst.Hub.Dispatch(protocol.PresenceChannel(env.Board.ID), []byte(`{"v":1,"type":"presence.leave","payload":{"user_id":"`+uuid.NewString()+`"}}`))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, inst.Hub.Shutdown(ctx))
	close(stop)
	wg.Wait()
	for range clients {
		assert.Contains(t, []int{protocol.CloseGoingAway, protocol.CloseSlowClient}, <-closed)
	}
}
