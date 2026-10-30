package simclient

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
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const wsWait = 5 * time.Second

func startWSClient(t *testing.T, f *fakeWS, c *Client) {
	t.Helper()
	c.cfg.Endpoints = []string{f.srv.URL}
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	t.Cleanup(func() {
		cancel()
		c.Stop()
	})
}

func newWSPair(t *testing.T) (*Client, *fakeWS) {
	t.Helper()
	c := newTestClient(t)
	f := newFakeWS(t, c.cfg.BoardID)
	startWSClient(t, f, c)
	return c, f
}

func waitLive(t *testing.T, c *Client) {
	t.Helper()
	require.Eventually(t, func() bool {
		st := c.Status()
		return st.Live && st.Initialized
	}, wsWait, 10*time.Millisecond)
}

func TestHandshakeSendsLastSeqOnlyAfterTheFirstSnapshot(t *testing.T) {
	c, f := newWSPair(t)
	f.mu.Lock()
	f.snapshot = protocol.Snapshot{Seq: 7}
	f.mu.Unlock()
	waitLive(t, c)
	require.Equal(t, int64(7), c.Status().State.Seq)

	c.Drop(10 * time.Millisecond)
	require.Eventually(t, func() bool { return len(f.helloList()) == 2 }, wsWait, 10*time.Millisecond)
	hellos := f.helloList()
	assert.Nil(t, hellos[0].LastSeq)
	require.NotNil(t, hellos[1].LastSeq)
	assert.Equal(t, int64(7), *hellos[1].LastSeq)
	assert.Equal(t, "simclient", hellos[1].Client)
	waitLive(t, c)
	assert.Equal(t, 1, c.Status().Stats.SelfDrops)
	assert.Equal(t, 2, c.Status().Stats.Reconnects)
}

func TestPendingOpsAreResentWithTheSameOpIDAfterReconnect(t *testing.T) {
	c, f := newWSPair(t)
	waitLive(t, c)
	require.True(t, c.RandomOp())
	require.Eventually(t, func() bool { return len(f.opList()) == 1 }, wsWait, 10*time.Millisecond)
	first := f.opList()[0]
	require.NotNil(t, first.OpID)

	c.Drop(10 * time.Millisecond)
	require.Eventually(t, func() bool { return len(f.opList()) == 2 }, wsWait, 10*time.Millisecond)
	second := f.opList()[1]
	require.NotNil(t, second.OpID)
	assert.Equal(t, *first.OpID, *second.OpID)
	assert.Equal(t, first.Type, second.Type)
	assert.JSONEq(t, string(first.Payload), string(second.Payload))
	assert.Equal(t, 1, c.Status().Pending)
}

func TestOwnEventResolvesThePendingOp(t *testing.T) {
	c, f := newWSPair(t)
	f.mu.Lock()
	f.onOp = func(s *wsSession, env protocol.Envelope) {
		var p boardcontent.ColumnCreate
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		s.send(columnCreatedEvent(t, 1, env.OpID, p.ColumnID, p.Title, "m"))
	}
	f.mu.Unlock()
	waitLive(t, c)
	require.True(t, c.RandomOp())
	require.Eventually(t, func() bool {
		st := c.Status()
		return st.Pending == 0 && st.State.Seq == 1
	}, wsWait, 10*time.Millisecond)
	st := c.Status()
	assert.Len(t, st.State.Columns, 1)
	assert.Equal(t, 1, st.Stats.EventsApplied)
	assert.Empty(t, st.Stats.Failures)
}

func TestNackFromTheServerDropsTheOp(t *testing.T) {
	c, f := newWSPair(t)
	f.mu.Lock()
	f.onOp = func(s *wsSession, env protocol.Envelope) {
		s.send(nackEvent(t, env.OpID, boardcontent.ReasonNotFound))
	}
	f.mu.Unlock()
	waitLive(t, c)
	require.True(t, c.RandomOp())
	require.Eventually(t, func() bool { return c.Status().Pending == 0 }, wsWait, 10*time.Millisecond)
	st := c.Status()
	assert.Equal(t, 1, st.Stats.Nacks[boardcontent.ReasonNotFound])
	assert.Empty(t, st.Stats.Failures)
}

func TestServerClosingTheConnectionTriggersAReconnect(t *testing.T) {
	c, f := newWSPair(t)
	waitLive(t, c)
	f.mu.Lock()
	sess := f.sessions[0]
	f.mu.Unlock()
	_ = sess.conn.Close()
	require.Eventually(t, func() bool { return len(f.helloList()) == 2 }, wsWait, 10*time.Millisecond)
	waitLive(t, c)
	assert.Equal(t, 2, c.Status().Stats.Reconnects)
}

func TestEventsBufferedBeforeADisconnectAreDiscarded(t *testing.T) {
	c, f := newWSPair(t)
	waitLive(t, c)
	f.mu.Lock()
	sess := f.sessions[0]
	f.mu.Unlock()
	sess.send(columnCreatedEvent(t, 5, nil, uuid.New(), "gap", "m"))
	require.Eventually(t, func() bool { return c.Status().Buffered == 1 }, wsWait, 5*time.Millisecond)
	c.Drop(200 * time.Millisecond)
	assert.Zero(t, c.Status().Buffered)
}

func TestUnreachableServerCountsDialFailuresAndStopReturns(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	c.Start(ctx)
	require.Eventually(t, func() bool { return c.Status().Stats.DialFailures >= 2 }, wsWait, 10*time.Millisecond)
	assert.False(t, c.Status().Live)
	cancel()
	done := make(chan struct{})
	go func() {
		c.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wsWait):
		t.Fatal("Stop did not return")
	}
	st := c.Status()
	assert.GreaterOrEqual(t, st.Stats.DialFailures, 2)
}

func TestStatusAndCommandsAfterStopDoNotBlock(t *testing.T) {
	c, _ := newWSPair(t)
	waitLive(t, c)
	c.Stop()
	st := c.Status()
	assert.True(t, st.Initialized)
	assert.False(t, c.RandomOp())
	c.Drop(time.Millisecond)
}

func TestDropWithoutAConnectionIsANoOp(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Start(ctx)
	defer c.Stop()
	c.Drop(time.Millisecond)
	assert.Zero(t, c.Status().Stats.SelfDrops)
}

func TestConcurrentUseOfTheClientIsSafe(t *testing.T) {
	c, f := newWSPair(t)
	f.mu.Lock()
	f.onOp = func(s *wsSession, env protocol.Envelope) {
		s.send(nackEvent(t, env.OpID, boardcontent.ReasonNotFound))
	}
	f.mu.Unlock()
	waitLive(t, c)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					c.RandomOp()
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				st := c.Status()
				_ = st.State.OrderedColumns()
				_ = st.Stats.Nacks[boardcontent.ReasonNotFound]
				time.Sleep(time.Millisecond)
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 5 {
			c.Drop(5 * time.Millisecond)
			time.Sleep(30 * time.Millisecond)
		}
	}()
	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()
	assert.Positive(t, c.Status().Stats.OpsCreated)
}
