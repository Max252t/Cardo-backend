package realtime_test

import (
	"context"
	"errors"
	"sync"
	"testing"

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

func TestHandshakeMessageOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		lastSeq  *int64
		wantSync string
	}{
		{"snapshot for a fresh client", nil, protocol.TypeSyncSnapshot},
		{"delta for a returning client", int64Ptr(1), protocol.TypeSyncDelta},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, ann, inst := newEditorEnv(t, nil)
			appendEvents(t, env, ann, 3)
			other := rttest.AddUser(env, "Zed", domain.RoleViewer)
			zed := inst.Dial(t, other)
			zed.HandshakeSnapshot()

			c := inst.Dial(t, ann)
			c.Hello(tt.lastSeq)
			var order []string
			for {
				msg := c.MustNext()
				order = append(order, msg.Type)
				if msg.Type == protocol.TypePresenceList {
					names := make([]string, 0)
					for _, u := range presenceList(t, msg) {
						names = append(names, u.Name)
					}
					assert.Equal(t, []string{"Ann", "Zed"}, names, "the list includes the connecting user and is sorted by name")
				}
				if msg.Type == protocol.TypeSyncReady {
					break
				}
			}
			assert.Equal(t, []string{tt.wantSync, protocol.TypePresenceList, protocol.TypeSyncReady}, order)
		})
	}
}

func TestHandshakeDeltaIsSentInChunks(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{DeltaChunk: 2} })
	appendEvents(t, env, ann, 5)

	c := inst.Dial(t, ann)
	c.Hello(int64Ptr(0))
	var sizes []int
	var seqs []int64
	for range 3 {
		delta := decodeAs[protocol.SyncDelta](t, c.Expect(protocol.TypeSyncDelta))
		sizes = append(sizes, len(delta.Events))
		for _, ev := range delta.Events {
			seqs = append(seqs, ev.Seq)
		}
	}
	assert.Equal(t, []int{2, 2, 1}, sizes)
	requireContiguous(t, seqs, 1, 5)
	c.Expect(protocol.TypePresenceList)
	c.Expect(protocol.TypeSyncReady)
}

func TestHandshakeDeltaSpansSeveralStorePages(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	const total = 1200
	appendEvents(t, env, ann, total)

	c := inst.Dial(t, ann)
	c.Hello(int64Ptr(0))
	var seqs []int64
	var messages int
	for int64(len(seqs)) < total {
		delta := decodeAs[protocol.SyncDelta](t, c.Expect(protocol.TypeSyncDelta))
		messages++
		for _, ev := range delta.Events {
			seqs = append(seqs, ev.Seq)
		}
	}
	requireContiguous(t, seqs, 1, total)
	assert.Equal(t, 3, messages, "1200 events in chunks of 500")
	c.Expect(protocol.TypePresenceList)
	c.Expect(protocol.TypeSyncReady)
}

func TestHandshakeDeltaLimitBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		missed   int
		wantSync string
	}{
		{"exactly the limit stays a delta", 4, protocol.TypeSyncDelta},
		{"one above the limit becomes a snapshot", 5, protocol.TypeSyncSnapshot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{MaxDeltaEvents: 4} })
			appendEvents(t, env, ann, tt.missed)
			c := inst.Dial(t, ann)
			c.Hello(int64Ptr(0))
			c.Expect(tt.wantSync)
		})
	}
}

func TestHandshakeFallbackReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t testing.TB, env *rttest.Env, user uuid.UUID)
		lastSeq int64
	}{
		{"events were pruned", func(t testing.TB, env *rttest.Env, user uuid.UUID) {
			appendEvents(t, env, user, 5)
			env.Board.Truncate(3)
		}, 1},
		{"client is ahead of the board", func(t testing.TB, env *rttest.Env, user uuid.UUID) {
			appendEvents(t, env, user, 2)
		}, 50},
		{"client claims a seq one above the head", func(t testing.TB, env *rttest.Env, user uuid.UUID) {
			appendEvents(t, env, user, 2)
		}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, ann, inst := newEditorEnv(t, nil)
			tt.prepare(t, env, ann)
			c := inst.Dial(t, ann)
			c.Hello(int64Ptr(tt.lastSeq))
			snap := decodeAs[protocol.Snapshot](t, c.Expect(protocol.TypeSyncSnapshot))
			assert.Equal(t, env.Board.Seq(), snap.Seq)
			c.Expect(protocol.TypePresenceList)
			c.Expect(protocol.TypeSyncReady)
		})
	}
}

func TestHandshakeAtPruneEdgeStillGetsADelta(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	appendEvents(t, env, ann, 6)
	env.Board.Truncate(3)

	c := inst.Dial(t, ann)
	c.Hello(int64Ptr(3))
	delta := decodeAs[protocol.SyncDelta](t, c.Expect(protocol.TypeSyncDelta))
	require.Len(t, delta.Events, 3)
	assert.Equal(t, int64(4), delta.Events[0].Seq)
}

func TestHandshakeFreshBoardWithoutEvents(t *testing.T) {
	t.Parallel()
	_, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	snap := c.HandshakeSnapshot()
	assert.Zero(t, snap.Seq)

	again := inst.Dial(t, ann)
	again.Hello(int64Ptr(0))
	delta := decodeAs[protocol.SyncDelta](t, again.Expect(protocol.TypeSyncDelta))
	assert.Empty(t, delta.Events)
}

func TestHelloValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		raw  string
	}{
		{"negative last_seq", `{"v":1,"type":"hello","payload":{"last_seq":-1}}`},
		{"last_seq with a string", `{"v":1,"type":"hello","payload":{"last_seq":"5"}}`},
		{"payload is an array", `{"v":1,"type":"hello","payload":[1]}`},
		{"payload is a string", `{"v":1,"type":"hello","payload":"x"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, ann, inst := newEditorEnv(t, nil)
			c := inst.Dial(t, ann)
			c.SendRaw([]byte(tt.raw))
			assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))

			c.Hello(nil)
			c.Expect(protocol.TypeSyncSnapshot)
			c.Expect(protocol.TypePresenceList)
			c.Expect(protocol.TypeSyncReady)
		})
	}
}

func TestHelloWithoutPayloadIsAFreshSync(t *testing.T) {
	t.Parallel()
	_, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.SendRaw([]byte(`{"v":1,"type":"hello"}`))
	c.Expect(protocol.TypeSyncSnapshot)
	c.Expect(protocol.TypePresenceList)
	c.Expect(protocol.TypeSyncReady)
}

func TestSecondHelloIsAProtocolViolationAndDoesNotRestartSync(t *testing.T) {
	t.Parallel()
	_, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{MaxProtocolViolations: 10} })
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	c.Hello(nil)
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	barrier(t, c)
}

func TestEventsPublishedDuringSnapshotHandshakeAreDeliveredExactlyOnceInOrder(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, nil)
	appendEvents(t, env, ann, 2)

	var once sync.Once
	var background sync.WaitGroup
	env.Sync = syncHooks{
		inner: env.Board,
		beforeSnapshot: func() error {
			once.Do(func() {
				appendEvents(t, env, ann, 3)
				background.Add(1)
				go func() {
					defer background.Done()
					appendEvents(t, env, ann, 20)
				}()
			})
			return nil
		},
		afterSnapshot: func() { appendEvents(t, env, ann, 3) },
	}
	inst := env.StartInstance(t, "late")

	c := inst.Dial(t, ann)
	c.Hello(nil)
	snap := decodeAs[protocol.Snapshot](t, c.Expect(protocol.TypeSyncSnapshot))
	c.Expect(protocol.TypePresenceList)

	var delivered []int64
	for {
		msg := c.MustNext()
		if msg.Type == protocol.TypeSyncReady {
			break
		}
		if msg.Type == boardcontent.EventCardCreated {
			delivered = append(delivered, msg.Seq)
		}
	}
	background.Wait()
	final := appendEvents(t, env, ann, 1)[0]
	delivered = append(delivered, readEventSeqs(t, c, boardcontent.EventCardCreated, final)...)

	requireContiguous(t, delivered, snap.Seq+1, final)
}

func TestEventsPublishedDuringDeltaHandshakeAreDeliveredExactlyOnceInOrder(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, nil)
	appendEvents(t, env, ann, 4)

	var once sync.Once
	env.Sync = syncHooks{
		inner: env.Board,
		beforeEvents: func(int64) error {
			once.Do(func() { appendEvents(t, env, ann, 3) })
			return nil
		},
		afterEvents: func() { appendEvents(t, env, ann, 3) },
	}
	inst := env.StartInstance(t, "late")

	c := inst.Dial(t, ann)
	c.Hello(int64Ptr(2))
	delta := decodeAs[protocol.SyncDelta](t, c.Expect(protocol.TypeSyncDelta))
	var seqs []int64
	for _, ev := range delta.Events {
		seqs = append(seqs, ev.Seq)
	}
	c.Expect(protocol.TypePresenceList)
	for {
		msg := c.MustNext()
		if msg.Type == protocol.TypeSyncReady {
			break
		}
		if msg.Type == boardcontent.EventCardCreated {
			seqs = append(seqs, msg.Seq)
		}
	}
	final := appendEvents(t, env, ann, 1)[0]
	seqs = append(seqs, readEventSeqs(t, c, boardcontent.EventCardCreated, final)...)
	requireContiguous(t, seqs, 3, final)
}

func TestLiveEventsAreHeldBackUntilThePresenceListAndPrecedeSyncReady(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, nil)
	observer := rttest.AddUser(env, "Obs", domain.RoleViewer)
	appendEvents(t, env, ann, 1)
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	env.Sync = syncHooks{
		inner: env.Board,
		afterSnapshot: func() {
			once.Do(func() { close(reached) })
			<-release
		},
	}
	inst := env.StartInstance(t, "late")
	obs := inst.Dial(t, observer)
	obs.Hello(int64Ptr(1))
	obs.Expect(protocol.TypeSyncDelta)
	obs.Expect(protocol.TypePresenceList)
	obs.Expect(protocol.TypeSyncReady)

	c := inst.Dial(t, ann)
	c.Hello(nil)
	<-reached
	appendEvents(t, env, ann, 4)
	readEventSeqs(t, obs, boardcontent.EventCardCreated, 5)
	close(release)

	var order []string
	var beforeReady []int64
	for {
		msg := c.MustNext()
		order = append(order, msg.Type)
		if msg.Type == boardcontent.EventCardCreated {
			beforeReady = append(beforeReady, msg.Seq)
		}
		if msg.Type == protocol.TypeSyncReady {
			break
		}
	}
	assert.Equal(t, protocol.TypeSyncSnapshot, order[0])
	assert.Equal(t, protocol.TypePresenceList, order[1])
	assert.Equal(t, protocol.TypeSyncReady, order[len(order)-1])
	require.GreaterOrEqual(t, len(beforeReady), 3)
	assert.Equal(t, []int64{2, 3, 4}, beforeReady[:3], "events buffered during the handshake are flushed before sync.ready")
	if len(beforeReady) == 3 {
		assert.Equal(t, []int64{5}, readEventSeqs(t, c, boardcontent.EventCardCreated, 5))
	} else {
		assert.Equal(t, []int64{2, 3, 4, 5}, beforeReady)
	}
}

func TestOperationDuringHandshakeIsAProtocolErrorAndNotApplied(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{MaxProtocolViolations: 10} })
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	env.Sync = syncHooks{
		inner: env.Board,
		beforeSnapshot: func() error {
			once.Do(func() { close(reached) })
			<-release
			return nil
		},
	}
	inst := env.StartInstance(t, "late")

	c := inst.Dial(t, ann)
	c.Hello(nil)
	<-reached
	sendOp(c, boardcontent.OpCardCreate)
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	close(release)
	c.Expect(protocol.TypeSyncSnapshot)
	c.Expect(protocol.TypePresenceList)
	c.Expect(protocol.TypeSyncReady)
	assert.Zero(t, env.Board.ApplyCount())

	sendOp(c, boardcontent.OpCardCreate)
	c.Expect("card.create")
}

func TestOperationBeforeHelloIsAProtocolErrorForEveryOpType(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{MaxProtocolViolations: 100} })
	c := inst.Dial(t, ann)
	for _, opType := range []string{
		boardcontent.OpCardCreate, boardcontent.OpCardEdit, boardcontent.OpCardMove, boardcontent.OpCardArchive,
		boardcontent.OpColumnCreate, boardcontent.OpColumnRename, boardcontent.OpColumnMove, boardcontent.OpColumnArchive,
	} {
		sendOp(c, opType)
		assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()), opType)
	}
	assert.Zero(t, env.Board.ApplyCount())
}

func TestHandshakeFailuresCloseWithTheMatchingCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		err       error
		wantCode  int
		wantError bool
	}{
		{"board disappeared", domain.ErrNotFound, protocol.CloseBoardDeleted, false},
		{"access revoked", domain.ErrForbidden, protocol.CloseAccessRevoked, false},
		{"infrastructure failure", errors.New("db down"), websocket.CloseInternalServerErr, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, ann, _ := newEditorEnv(t, nil)
			env.Sync = syncHooks{inner: env.Board, beforeSnapshot: func() error { return tt.err }}
			inst := env.StartInstance(t, "late")
			c := inst.Dial(t, ann)
			c.Hello(nil)
			if tt.wantError {
				assert.Equal(t, boardcontent.ReasonInternal, errorCode(t, c.MustNext()))
			}
			assert.Equal(t, tt.wantCode, c.ExpectClose().Code)
			waitConnections(t, inst, env.Board.ID, 0)
		})
	}
}

func TestHandshakeFailureDuringDeltaClosesToo(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, nil)
	appendEvents(t, env, ann, 2)
	env.Sync = syncHooks{inner: env.Board, beforeEvents: func(int64) error { return errors.New("db down") }}
	inst := env.StartInstance(t, "late")
	c := inst.Dial(t, ann)
	c.Hello(int64Ptr(1))
	assert.Equal(t, boardcontent.ReasonInternal, errorCode(t, c.MustNext()))
	assert.Equal(t, websocket.CloseInternalServerErr, c.ExpectClose().Code)
}

func TestPendingBufferOverflowDuringHandshakeClosesWithSlowClientCode(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{PendingLimit: 2} })
	observer := rttest.AddUser(env, "Obs", domain.RoleViewer)
	release := make(chan struct{})
	reached := make(chan struct{})
	var once sync.Once
	env.Sync = syncHooks{
		inner: env.Board,
		beforeSnapshot: func() error {
			once.Do(func() { close(reached) })
			<-release
			return nil
		},
	}
	inst := env.StartInstance(t, "late")
	obs := inst.Dial(t, observer)
	obs.Hello(int64Ptr(0))
	obs.Expect(protocol.TypeSyncDelta)
	obs.Expect(protocol.TypePresenceList)
	obs.Expect(protocol.TypeSyncReady)

	c := inst.Dial(t, ann)
	c.Hello(nil)
	<-reached
	appendEvents(t, env, ann, 4)
	readEventSeqs(t, obs, boardcontent.EventCardCreated, 4)
	close(release)
	assert.Equal(t, protocol.CloseSlowClient, c.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 1)
}

func TestHandshakeIsCancelledWhenTheClientDisconnects(t *testing.T) {
	t.Parallel()
	env, ann, _ := newEditorEnv(t, nil)
	reached := make(chan struct{})
	ctxCancelled := make(chan struct{})
	env.Sync = hookedContext{inner: env.Board, reached: reached, cancelled: ctxCancelled}
	inst := env.StartInstance(t, "late")
	c := inst.Dial(t, ann)
	c.Hello(nil)
	<-reached
	require.NoError(t, c.Close())
	<-ctxCancelled
	waitConnections(t, inst, env.Board.ID, 0)
}

type hookedContext struct {
	inner     syncInner
	reached   chan struct{}
	cancelled chan struct{}
}

func (h hookedContext) Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error) {
	close(h.reached)
	<-ctx.Done()
	close(h.cancelled)
	return domain.BoardSnapshot{}, ctx.Err()
}

func (h hookedContext) Events(ctx context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error) {
	return h.inner.Events(ctx, userID, boardID, afterSeq)
}
