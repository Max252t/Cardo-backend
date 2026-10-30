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

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func TestConcurrentJoinsAndLeavesLeaveNoResidue(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	users := make([]uuid.UUID, 4)
	for i := range users {
		users[i] = rttest.AddUser(env, "User"+strconv.Itoa(i), domain.RoleEditor)
	}
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	instances := []*rttest.Instance{a, b}
	probe := presenceProbe(t, env)

	stop := make(chan struct{})
	var publisher sync.WaitGroup
	publisher.Add(1)
	go func() {
		defer publisher.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = env.Board.Append(context.Background(), boardcontent.EventCardCreated, map[string]string{}, users[0])
		}
	}()

	const workers = 8
	const rounds = 6
	var wg sync.WaitGroup
	var failures atomic.Int64
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				inst := instances[(w+r)%len(instances)]
				user := users[(w+r)%len(users)]
				conn, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, user), nil)
				if err != nil {
					failures.Add(1)
					continue
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"v":1,"type":"hello","payload":{"last_seq":null}}`))
				for range 4 {
					if _, _, err := conn.ReadMessage(); err != nil {
						break
					}
				}
				_ = conn.Close()
			}
		}()
	}
	wg.Wait()
	close(stop)
	publisher.Wait()

	assert.Zero(t, failures.Load())
	waitConnections(t, a, env.Board.ID, 0)
	waitConnections(t, b, env.Board.ID, 0)
	waitSubscribed(t, a, env.Board.ID, false)
	waitSubscribed(t, b, env.Board.ID, false)
	require.Eventually(t, func() bool { return len(onlineNames(t, probe, env.Board.ID)) == 0 }, eventually, 5*time.Millisecond)
}

func TestReconnectRaceNeverLeavesAConnectedClientUnsubscribed(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	board := env.Board.ID

	for round := range 20 {
		old := inst.Dial(t, ann)
		old.HandshakeSnapshot()

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = old.Close()
		}()
		fresh := inst.Dial(t, ann)
		wg.Wait()
		fresh.Hello(nil)
		fresh.Expect(protocol.TypeSyncSnapshot)
		fresh.Expect(protocol.TypePresenceList)
		fresh.Expect(protocol.TypeSyncReady)

		waitConnections(t, inst, board, 1)
		waitSubscribed(t, inst, board, true)
		ev, err := env.Board.Append(context.Background(), boardcontent.EventCardCreated, map[string]string{"round": strconv.Itoa(round)}, ann)
		require.NoError(t, err)
		assert.Equal(t, []int64{ev.Seq}, readEventSeqs(t, fresh, boardcontent.EventCardCreated, ev.Seq))

		require.NoError(t, fresh.Close())
		waitConnections(t, inst, board, 0)
		waitSubscribed(t, inst, board, false)
	}
}

func TestDispatchRacingWithJoinsLeavesAndForcedCloses(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	board := env.Board.ID
	stop := make(chan struct{})
	var background sync.WaitGroup
	spawn := func(fn func(i int64)) {
		background.Add(1)
		go func() {
			defer background.Done()
			for i := int64(1); ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				fn(i)
			}
		}()
	}
	spawn(func(i int64) {
		inst.Hub.Dispatch(protocol.BoardChannel(board), []byte(`{"v":1,"type":"card.created","seq":`+strconv.FormatInt(i, 10)+`,"payload":{}}`))
	})
	spawn(func(int64) {
		inst.Hub.Dispatch(protocol.PresenceChannel(board), []byte(`{"v":1,"type":"presence.join","payload":{"user":{"id":"`+uuid.NewString()+`","name":"X","avatar_color":"#000000"}}}`))
	})
	spawn(func(int64) { inst.Hub.CloseUser(board, ann, protocol.CloseAccessRevoked, "revoked") })
	spawn(func(int64) { inst.Hub.SetRole(board, ann, domain.RoleViewer) })
	spawn(func(int64) { inst.Hub.SetRole(board, ann, domain.RoleEditor) })
	spawn(func(int64) { _ = inst.Hub.ConnectionCount(board) })

	for range 25 {
		conn, _, err := inst.TryDial(board, "Bearer "+env.IssueToken(t, ann), nil)
		if err != nil {
			continue
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"v":1,"type":"hello"}`))
		_ = conn.Close()
	}
	close(stop)
	background.Wait()
	waitConnections(t, inst, board, 0)
	waitSubscribed(t, inst, board, false)
}

func TestConcurrentOperationsWithRoleChangesAndCloses(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{OpsPerSecond: 100000, OpsBurst: 100000, RateAbuseLimit: 1 << 30}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	inst := env.StartInstance(t, "a")

	var clients []*rttest.Conn
	for range 6 {
		c := inst.Dial(t, ann)
		c.HandshakeSnapshot()
		clients = append(clients, c)
	}
	var drain sync.WaitGroup
	for _, c := range clients {
		drain.Add(1)
		go func() {
			defer drain.Done()
			for {
				if _, err := c.Next(); err != nil {
					return
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				opID := uuid.New()
				c.SendJSON(boardcontent.OpCardCreate, &opID, map[string]any{"card_id": uuid.NewString()})
				if i%2 == 0 {
					inst.Hub.SetRole(env.Board.ID, ann, domain.RoleViewer)
					inst.Hub.SetRole(env.Board.ID, ann, domain.RoleEditor)
				}
			}
		}()
	}
	wg.Wait()
	inst.Hub.CloseBoard(env.Board.ID, protocol.CloseGoingAway, "done")
	drain.Wait()
	waitConnections(t, inst, env.Board.ID, 0)
}

type orderingPresence struct {
	realtime.Presence
	mu          sync.Mutex
	armed       bool
	leaveEnter  chan struct{}
	releaseOnce chan struct{}
	leaveDone   chan struct{}
}

func (p *orderingPresence) Leave(ctx context.Context, boardID, userID uuid.UUID) error {
	p.mu.Lock()
	armed := p.armed
	p.armed = false
	p.mu.Unlock()
	if armed {
		close(p.leaveEnter)
		<-p.releaseOnce
	}
	err := p.Presence.Leave(ctx, boardID, userID)
	if armed {
		close(p.leaveDone)
	}
	return err
}

func TestReconnectWhileTheOldLeaveIsStillInFlightKeepsTheUserOnline(t *testing.T) {
	t.Parallel()

	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	spy := &orderingPresence{leaveEnter: make(chan struct{}), releaseOnce: make(chan struct{}), leaveDone: make(chan struct{})}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) {
		spy.Presence = d.Presence
		d.Presence = spy
	})
	probe := presenceProbe(t, env)

	first := inst.Dial(t, ann)
	first.HandshakeSnapshot()
	spy.mu.Lock()
	spy.armed = true
	spy.mu.Unlock()
	require.NoError(t, first.Close())
	<-spy.leaveEnter

	second := inst.Dial(t, ann)
	close(spy.releaseOnce)
	<-spy.leaveDone
	second.HandshakeSnapshot()
	waitConnections(t, inst, env.Board.ID, 1)

	assert.Equal(t, []string{"Ann"}, onlineNames(t, probe, env.Board.ID), "a user with a live connection must stay in the presence store")
}

type blockingTouch struct {
	realtime.Presence
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (p *blockingTouch) Touch(ctx context.Context, boardID uuid.UUID, users []domain.PresenceUser) error {
	p.mu.Lock()
	armed := p.armed
	p.armed = false
	p.mu.Unlock()
	if armed {
		close(p.entered)
		<-p.release
	}
	return p.Presence.Touch(ctx, boardID, users)
}

func TestHeartbeatInFlightDoesNotResurrectAUserWhoLeftMeanwhile(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PresenceInterval: 15 * time.Second, PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	spy := &blockingTouch{entered: make(chan struct{}), release: make(chan struct{})}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) {
		spy.Presence = d.Presence
		d.Presence = spy
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go inst.Hub.Run(ctx)
	probe := presenceProbe(t, env)

	conn := inst.Dial(t, ann)
	conn.HandshakeSnapshot()
	waitTimers(t, env, 3)
	spy.mu.Lock()
	spy.armed = true
	spy.mu.Unlock()
	env.Clock.Advance(15 * time.Second)
	<-spy.entered

	require.NoError(t, conn.Close())
	assert.Never(t, func() bool { return inst.Hub.ConnectionCount(env.Board.ID) == 0 }, 150*time.Millisecond, 5*time.Millisecond)
	close(spy.release)
	waitConnections(t, inst, env.Board.ID, 0)

	require.Eventually(t, func() bool { return len(onlineNames(t, probe, env.Board.ID)) == 0 }, eventually, 5*time.Millisecond)
	assert.Never(t, func() bool { return len(onlineNames(t, probe, env.Board.ID)) != 0 }, 150*time.Millisecond, 5*time.Millisecond)
}

type gatedBus struct {
	realtime.Bus
	entered     chan struct{}
	release     chan struct{}
	mu          sync.Mutex
	subscribes  int
	unsubscribe int
}

func (b *gatedBus) Subscribe(ctx context.Context, channels ...string) error {
	close(b.entered)
	<-b.release
	b.mu.Lock()
	b.subscribes++
	b.mu.Unlock()
	return b.Bus.Subscribe(ctx, channels...)
}

func (b *gatedBus) Unsubscribe(ctx context.Context, channels ...string) error {
	b.mu.Lock()
	b.unsubscribe++
	b.mu.Unlock()
	return b.Bus.Unsubscribe(ctx, channels...)
}

func TestJoinRacingShutdownReleasesItsSubscription(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bus := &gatedBus{entered: make(chan struct{}), release: make(chan struct{})}
	inst := env.StartInstanceWith(t, "a", func(d *realtime.Deps) {
		bus.Bus = d.Bus
		d.Bus = bus
	})

	conn, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	<-bus.entered

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, inst.Hub.Shutdown(ctx))
	close(bus.release)

	require.Eventually(t, func() bool {
		bus.mu.Lock()
		defer bus.mu.Unlock()
		return bus.unsubscribe == 1
	}, eventually, 5*time.Millisecond)
	waitSubscribed(t, inst, env.Board.ID, false)
	assert.Zero(t, inst.Hub.ConnectionCount(env.Board.ID))
}
