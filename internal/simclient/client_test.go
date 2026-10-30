package simclient

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func pendingCreate(id uuid.UUID, columnID uuid.UUID, title string) PendingOp {
	return PendingOp{ID: id, Type: boardcontent.OpColumnCreate, Payload: &boardcontent.ColumnCreate{ColumnID: columnID, Title: title}}
}

func pendingCard(id, cardID, columnID uuid.UUID) PendingOp {
	return PendingOp{ID: id, Type: boardcontent.OpCardCreate, Payload: &boardcontent.CardCreate{CardID: cardID, ColumnID: columnID, Title: "card"}}
}

func TestEventsApplyStrictlyInSequenceOrder(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	for i := int64(1); i <= 3; i++ {
		c.acceptEvent(columnCreatedEvent(t, i, nil, uuid.New(), "col", string(rune('a'+i))))
	}
	assert.Equal(t, int64(3), c.state.Seq)
	assert.Len(t, c.state.Columns, 3)
	assert.Equal(t, 3, c.stats.EventsApplied)
	assert.Empty(t, c.buf)
	assert.True(t, c.gapDeadline.IsZero())
}

func TestOutOfOrderEventsAreBufferedUntilTheGapCloses(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 3, nil, uuid.New(), "c", "c"))
	c.acceptEvent(columnCreatedEvent(t, 2, nil, uuid.New(), "b", "b"))
	assert.Equal(t, int64(0), c.state.Seq)
	assert.Len(t, c.buf, 2)
	assert.False(t, c.gapDeadline.IsZero())
	assert.Zero(t, c.stats.EventsApplied)

	c.acceptEvent(columnCreatedEvent(t, 1, nil, uuid.New(), "a", "a"))
	assert.Equal(t, int64(3), c.state.Seq)
	assert.Empty(t, c.buf)
	assert.True(t, c.gapDeadline.IsZero())
	assert.Equal(t, 3, c.stats.EventsApplied)
}

func TestGapDeadlineIsNotExtendedByMoreBufferedEvents(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 3, nil, uuid.New(), "c", "c"))
	first := c.gapDeadline
	time.Sleep(5 * time.Millisecond)
	c.acceptEvent(columnCreatedEvent(t, 4, nil, uuid.New(), "d", "d"))
	assert.Equal(t, first, c.gapDeadline)
}

func TestGapDeadlineRestartsWhenTheBufferDrains(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 2, nil, uuid.New(), "b", "b"))
	c.acceptEvent(columnCreatedEvent(t, 4, nil, uuid.New(), "d", "d"))
	first := c.gapDeadline
	time.Sleep(5 * time.Millisecond)
	c.acceptEvent(columnCreatedEvent(t, 1, nil, uuid.New(), "a", "a"))
	assert.Equal(t, int64(2), c.state.Seq)
	assert.Len(t, c.buf, 1)
	assert.True(t, c.gapDeadline.After(first))
}

func TestNothingIsFetchedBeforeTheGapDeadline(t *testing.T) {
	c, api := newClientWithAPI(t)
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 2, nil, uuid.New(), "b", "b"))
	c.onTick(context.Background(), c.gapDeadline.Add(-10*time.Millisecond))
	assert.Empty(t, api.requests())
	assert.Zero(t, c.stats.GapFetches)
}

func TestGapTimerFetchesMissingEventsOverREST(t *testing.T) {
	c, api := newClientWithAPI(t)
	first, second := uuid.New(), uuid.New()
	api.events = func(after int64) (int, protocol.EventsPage) {
		return http.StatusOK, protocol.EventsPage{Events: []protocol.Envelope{
			columnCreatedEvent(t, 1, nil, first, "a", "a"),
			columnCreatedEvent(t, 2, nil, second, "b", "b"),
		}, Seq: 2}
	}
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 2, nil, second, "b", "b"))
	require.False(t, c.gapDeadline.IsZero())

	c.onTick(context.Background(), c.gapDeadline.Add(time.Millisecond))

	assert.Equal(t, []string{"after_seq=0"}, api.requests())
	api.mu.Lock()
	assert.Equal(t, []string{"Bearer token-1"}, api.auths)
	api.mu.Unlock()
	assert.Equal(t, int64(2), c.state.Seq)
	assert.Contains(t, c.state.Columns, first)
	assert.Contains(t, c.state.Columns, second)
	assert.Equal(t, 1, c.stats.GapFetches)
	assert.Equal(t, 2, c.stats.GapFetchEvents)
	assert.Empty(t, c.buf)
	assert.True(t, c.gapDeadline.IsZero())
}

func TestGapFetchFollowsPaging(t *testing.T) {
	c, api := newClientWithAPI(t)
	api.events = func(after int64) (int, protocol.EventsPage) {
		if after == 0 {
			return http.StatusOK, protocol.EventsPage{Events: []protocol.Envelope{
				columnCreatedEvent(t, 1, nil, uuid.New(), "a", "a"),
				columnCreatedEvent(t, 2, nil, uuid.New(), "b", "b"),
			}, HasMore: true}
		}
		return http.StatusOK, protocol.EventsPage{Events: []protocol.Envelope{
			columnCreatedEvent(t, 3, nil, uuid.New(), "c", "c"),
			columnCreatedEvent(t, 4, nil, uuid.New(), "d", "d"),
		}}
	}
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 5, nil, uuid.New(), "e", "e"))
	c.onTick(context.Background(), c.gapDeadline.Add(time.Millisecond))

	assert.Equal(t, []string{"after_seq=0", "after_seq=2"}, api.requests())
	assert.Equal(t, int64(5), c.state.Seq)
	assert.Empty(t, c.buf)
}

func TestGapFetchThatDoesNotCloseTheGapRearmsTheTimer(t *testing.T) {
	c, api := newClientWithAPI(t)
	api.events = func(int64) (int, protocol.EventsPage) {
		return http.StatusOK, protocol.EventsPage{Events: []protocol.Envelope{columnCreatedEvent(t, 1, nil, uuid.New(), "a", "a")}}
	}
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 4, nil, uuid.New(), "d", "d"))
	c.onTick(context.Background(), c.gapDeadline.Add(time.Millisecond))
	assert.Equal(t, int64(1), c.state.Seq)
	assert.Len(t, c.buf, 1)
	assert.False(t, c.gapDeadline.IsZero())
}

func TestGoneResponseReloadsTheSnapshot(t *testing.T) {
	c, api := newClientWithAPI(t)
	col := uuid.New()
	stale := uuid.New()
	api.events = func(int64) (int, protocol.EventsPage) { return http.StatusGone, protocol.EventsPage{} }
	api.snapshot = func() (int, protocol.Snapshot) {
		return http.StatusOK, protocol.Snapshot{Seq: 10, Columns: []boardcontent.Column{{ID: col, Title: "T", Rank: "m", Version: 4}}}
	}
	loadEmpty(c, 0)
	c.pending = []PendingOp{pendingCreate(stale, uuid.New(), "x")}
	c.acceptEvent(columnCreatedEvent(t, 3, &stale, uuid.New(), "late", "z"))
	c.acceptEvent(columnCreatedEvent(t, 12, nil, uuid.New(), "future", "y"))

	c.onTick(context.Background(), c.gapDeadline.Add(time.Millisecond))

	assert.Equal(t, 1, c.stats.Gone)
	assert.Equal(t, 1, c.stats.SnapshotReloads)
	assert.Equal(t, 1, api.snapshotRequests())
	assert.Equal(t, int64(10), c.state.Seq)
	require.Contains(t, c.state.Columns, col)
	assert.Equal(t, int64(4), c.state.Columns[col].Version)
	assert.Len(t, c.buf, 1)
	assert.Contains(t, c.buf, int64(12))
	assert.Empty(t, c.pending)
	assert.False(t, c.gapDeadline.IsZero())
}

func TestUnexpectedStatusIsAFailureAndRetries(t *testing.T) {
	c, api := newClientWithAPI(t)
	api.events = func(int64) (int, protocol.EventsPage) { return http.StatusInternalServerError, protocol.EventsPage{} }
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 2, nil, uuid.New(), "b", "b"))
	c.onTick(context.Background(), c.gapDeadline.Add(time.Millisecond))
	require.Len(t, c.stats.Failures, 1)
	assert.Contains(t, c.stats.Failures[0], "500")
	assert.False(t, c.gapDeadline.IsZero())
	assert.Equal(t, int64(0), c.state.Seq)
}

func TestUnreachableServerRearmsTheTimerWithoutFailure(t *testing.T) {
	c := newTestClient(t, "http://127.0.0.1:1")
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 2, nil, uuid.New(), "b", "b"))
	c.onTick(context.Background(), c.gapDeadline.Add(time.Millisecond))
	assert.Empty(t, c.stats.Failures)
	assert.False(t, c.gapDeadline.IsZero())
	assert.Len(t, c.buf, 1)
}

func TestIdlePollFetchesEventsOnlyOnceInitialized(t *testing.T) {
	c, api := newClientWithAPI(t)
	api.events = func(int64) (int, protocol.EventsPage) {
		return http.StatusOK, protocol.EventsPage{Events: []protocol.Envelope{columnCreatedEvent(t, 1, nil, uuid.New(), "a", "a")}}
	}
	c.cfg.IdlePoll = time.Minute
	c.nextPoll = time.Now().Add(-time.Second)
	c.onTick(context.Background(), time.Now())
	assert.Empty(t, api.requests())

	loadEmpty(c, 0)
	c.nextPoll = time.Now().Add(-time.Second)
	c.onTick(context.Background(), time.Now())
	assert.Equal(t, []string{"after_seq=0"}, api.requests())
	assert.Equal(t, 1, c.stats.PollEvents)
	assert.Zero(t, c.stats.GapFetchEvents)
	assert.True(t, c.nextPoll.After(time.Now()))
}

func TestDuplicateEventResolvesPendingExactlyOnce(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	op, other := uuid.New(), uuid.New()
	colID := uuid.New()
	c.pending = []PendingOp{pendingCreate(op, colID, "a"), pendingCreate(other, uuid.New(), "b")}
	ev := columnCreatedEvent(t, 1, &op, colID, "a", "a")

	c.acceptEvent(ev)
	assert.Equal(t, []uuid.UUID{other}, pendingOps(c))
	c.acceptEvent(ev)
	assert.Equal(t, 1, c.stats.EventsApplied)
	assert.Equal(t, []uuid.UUID{other}, pendingOps(c))
	assert.Empty(t, c.stats.Failures)
}

func TestReplayedEventBehindTheStateStillResolvesItsOp(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 5)
	op := uuid.New()
	c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a")}
	c.acceptEvent(columnCreatedEvent(t, 3, &op, uuid.New(), "a", "a"))
	assert.Empty(t, c.pending)
	assert.Zero(t, c.stats.EventsApplied)
	assert.Equal(t, int64(5), c.state.Seq)
}

func TestEventForAnotherOpLeavesPendingUntouched(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	mine, theirs := uuid.New(), uuid.New()
	c.pending = []PendingOp{pendingCreate(mine, uuid.New(), "a")}
	c.acceptEvent(columnCreatedEvent(t, 1, &theirs, uuid.New(), "x", "x"))
	assert.Equal(t, []uuid.UUID{mine}, pendingOps(c))
}

func TestEventWithoutSequenceIsIgnored(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	op := uuid.New()
	c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a")}
	c.acceptEvent(columnCreatedEvent(t, 0, &op, uuid.New(), "x", "x"))
	assert.Len(t, c.pending, 1)
	assert.Zero(t, c.stats.EventsApplied)
}

func TestApplyFailureKeepsTheSequenceAndIsRecorded(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	op := uuid.New()
	c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a")}
	c.acceptEvent(event(t, 1, boardcontent.EventCardEdited, &op, boardcontent.CardEdited{CardID: uuid.New(), Version: 2}))
	assert.Equal(t, int64(0), c.state.Seq)
	require.Len(t, c.stats.Failures, 1)
	assert.Zero(t, c.stats.EventsApplied)
	assert.Len(t, c.pending, 1)
}

func TestSnapshotDiscardsBufferedEventsItCoversAndKeepsLaterOnes(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	op := uuid.New()
	c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a")}
	c.acceptEvent(columnCreatedEvent(t, 3, &op, uuid.New(), "a", "a"))
	c.acceptEvent(columnCreatedEvent(t, 4, nil, uuid.New(), "b", "b"))
	c.acceptEvent(columnCreatedEvent(t, 8, nil, uuid.New(), "c", "c"))
	require.Len(t, c.buf, 3)

	c.loadSnapshot(protocol.Snapshot{Seq: 5})

	assert.Equal(t, int64(5), c.state.Seq)
	assert.Len(t, c.buf, 1)
	assert.Contains(t, c.buf, int64(8))
	assert.Empty(t, c.pending)
	assert.False(t, c.gapDeadline.IsZero())
}

func TestSnapshotFollowedByContiguousBufferedEventsAppliesThem(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.acceptEvent(columnCreatedEvent(t, 6, nil, uuid.New(), "f", "f"))
	c.loadSnapshot(protocol.Snapshot{Seq: 5})
	assert.Equal(t, int64(6), c.state.Seq)
	assert.Empty(t, c.buf)
	assert.True(t, c.gapDeadline.IsZero())
}

func TestNackOfCreateDropsItsDependantsOnRebase(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	createOp, cardOp, editOp, unrelatedOp := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	col, card, otherCol := uuid.New(), uuid.New(), uuid.New()
	title := "edited"
	c.pending = []PendingOp{
		pendingCreate(createOp, col, "col"),
		pendingCard(cardOp, card, col),
		{ID: editOp, Type: boardcontent.OpCardEdit, Payload: &boardcontent.CardEdit{CardID: card, Title: &title}},
		pendingCreate(unrelatedOp, otherCol, "other"),
	}
	c.rebase()
	require.Len(t, c.pending, 4)

	c.handle(nackEvent(t, &createOp, boardcontent.ReasonVersionConflict))
	assert.Equal(t, []uuid.UUID{cardOp, editOp, unrelatedOp}, pendingOps(c))
	assert.Equal(t, 1, c.stats.Nacks[boardcontent.ReasonVersionConflict])

	c.rebase()
	assert.Equal(t, []uuid.UUID{unrelatedOp}, pendingOps(c))
	assert.Equal(t, 2, c.stats.LocalDrops)
	assert.Empty(t, c.stats.Failures)
}

func TestVersionConflictAndNotFoundDropTheOpWithoutFailure(t *testing.T) {
	for _, reason := range []string{boardcontent.ReasonVersionConflict, boardcontent.ReasonNotFound} {
		t.Run(reason, func(t *testing.T) {
			c := newTestClient(t)
			loadEmpty(c, 0)
			op, keep := uuid.New(), uuid.New()
			c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a"), pendingCreate(keep, uuid.New(), "b")}
			c.handle(nackEvent(t, &op, reason))
			assert.Equal(t, []uuid.UUID{keep}, pendingOps(c))
			assert.Equal(t, 1, c.stats.Nacks[reason])
			assert.Empty(t, c.stats.Failures)
			assert.True(t, c.dirty)
		})
	}
}

func TestNackForAnUnknownOpIsCountedAndIgnored(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	keep, unknown := uuid.New(), uuid.New()
	c.pending = []PendingOp{pendingCreate(keep, uuid.New(), "a")}
	c.handle(nackEvent(t, &unknown, boardcontent.ReasonNotFound))
	assert.Equal(t, []uuid.UUID{keep}, pendingOps(c))
	assert.Equal(t, 1, c.stats.Nacks[boardcontent.ReasonNotFound])
	assert.Empty(t, c.stats.Failures)
}

func TestUnexpectedNackReasonIsRecordedAsAFailureAndDropsTheOp(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	op := uuid.New()
	c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a")}
	c.handle(nackEvent(t, &op, boardcontent.ReasonValidation))
	assert.Empty(t, c.pending)
	require.Len(t, c.stats.Failures, 1)
	assert.Contains(t, c.stats.Failures[0], boardcontent.ReasonValidation)
}

func TestNackWithoutOpIDIsAFailure(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.handle(nackEvent(t, nil, boardcontent.ReasonNotFound))
	require.Len(t, c.stats.Failures, 1)
}

func TestMalformedNackPayloadIsAFailure(t *testing.T) {
	c := newTestClient(t)
	op := uuid.New()
	c.handle(protocol.Envelope{V: protocol.Version, Type: protocol.TypeNack, OpID: &op, Payload: json.RawMessage(`"x"`)})
	require.Len(t, c.stats.Failures, 1)
}

func TestResyncRequiredNackDropsTheOpAndReloadsTheSnapshot(t *testing.T) {
	c, api := newClientWithAPI(t)
	col := uuid.New()
	api.snapshot = func() (int, protocol.Snapshot) {
		return http.StatusOK, protocol.Snapshot{Seq: 9, Columns: []boardcontent.Column{{ID: col, Title: "T", Rank: "m", Version: 1}}}
	}
	loadEmpty(c, 2)
	op, keep := uuid.New(), uuid.New()
	c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a"), pendingCreate(keep, uuid.New(), "b")}

	c.handle(nackEvent(t, &op, boardcontent.ReasonResyncRequired))

	assert.Equal(t, 1, c.stats.ResyncRequired)
	assert.Equal(t, 1, c.stats.SnapshotReloads)
	assert.Equal(t, int64(9), c.state.Seq)
	assert.Contains(t, c.state.Columns, col)
	assert.Equal(t, []uuid.UUID{keep}, pendingOps(c))
	assert.Empty(t, c.stats.Failures)
}

func TestFailedSnapshotReloadRearmsTheTimerAndKeepsTheState(t *testing.T) {
	c, api := newClientWithAPI(t)
	api.snapshot = func() (int, protocol.Snapshot) { return http.StatusInternalServerError, protocol.Snapshot{} }
	loadEmpty(c, 2)
	c.reloadSnapshot(context.Background())
	assert.Equal(t, int64(2), c.state.Seq)
	assert.Zero(t, c.stats.SnapshotReloads)
	assert.False(t, c.gapDeadline.IsZero())
}

func TestRebaseDropsPendingOpsInvalidatedByRemoteEvents(t *testing.T) {
	c := newTestClient(t)
	col, card := uuid.New(), uuid.New()
	c.loadSnapshot(protocol.Snapshot{
		Seq:     3,
		Columns: []boardcontent.Column{{ID: col, Title: "C", Rank: "m", Version: 1}},
		Cards:   []boardcontent.Card{{ID: card, ColumnID: col, Title: "K", Rank: "m", Version: 1, DescriptionVersion: 1}},
	})
	title := "mine"
	editOp, moveOp, archiveOp := uuid.New(), uuid.New(), uuid.New()
	base := int64(1)
	c.pending = []PendingOp{
		{ID: moveOp, Type: boardcontent.OpCardMove, Payload: &boardcontent.CardMove{CardID: card, ToColumnID: col, BaseVersion: &base}},
		{ID: editOp, Type: boardcontent.OpCardEdit, Payload: &boardcontent.CardEdit{CardID: card, Title: &title}},
	}
	c.rebase()
	require.Len(t, c.pending, 2)

	remote := "remote"
	c.acceptEvent(event(t, 4, boardcontent.EventCardEdited, nil, boardcontent.CardEdited{CardID: card, Title: &remote, Version: 2, DescriptionVersion: 1}))
	c.onReady()
	assert.Equal(t, []uuid.UUID{editOp}, pendingOps(c))
	assert.Equal(t, 1, c.stats.LocalDrops)

	c.pending = append(c.pending, PendingOp{ID: archiveOp, Type: boardcontent.OpCardArchive, Payload: &boardcontent.CardArchive{CardID: card}})
	c.acceptEvent(event(t, 5, boardcontent.EventCardArchived, nil, boardcontent.CardArchived{CardID: card, ColumnID: col, Version: 3}))
	c.onReady()
	assert.Empty(t, c.pending)
	assert.Equal(t, 3, c.stats.LocalDrops)
}

func TestRebaseWithoutChangesKeepsTheViewAndPending(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.rebase()
	first := c.view
	op := uuid.New()
	c.pending = []PendingOp{pendingCreate(op, uuid.New(), "a")}
	c.rebase()
	assert.Same(t, first, c.view)
	assert.Equal(t, []uuid.UUID{op}, pendingOps(c))
}

func TestStaleConnectionMessagesAreIgnored(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.connGen = 2
	stale, err := json.Marshal(columnCreatedEvent(t, 1, nil, uuid.New(), "stale", "a"))
	require.NoError(t, err)

	c.onInbound(inbound{gen: 1, data: stale})
	assert.Equal(t, int64(0), c.state.Seq)

	c.onInbound(inbound{gen: 1, err: assert.AnError})
	assert.Equal(t, 2, c.connGen)
	assert.True(t, c.reconnectAt.IsZero())

	c.onInbound(inbound{gen: 2, data: stale})
	assert.Equal(t, int64(1), c.state.Seq)
}

func TestReadErrorOnTheCurrentConnectionSchedulesAReconnect(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.connGen = 4
	c.buf[7] = columnCreatedEvent(t, 7, nil, uuid.New(), "x", "x")
	c.onInbound(inbound{gen: 4, err: assert.AnError})
	assert.Equal(t, 5, c.connGen)
	assert.False(t, c.reconnectAt.IsZero())
	assert.False(t, c.live)
	assert.Empty(t, c.buf)
	assert.True(t, c.gapDeadline.IsZero())
}

func TestMalformedServerMessageIsAFailure(t *testing.T) {
	c := newTestClient(t)
	c.onInbound(inbound{gen: 0, data: []byte("not json")})
	require.Len(t, c.stats.Failures, 1)
}

func TestUnknownMessageTypesAreFailuresUnlessTheyCarryASequence(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	c.handle(protocol.Envelope{V: protocol.Version, Type: "mystery"})
	assert.Len(t, c.stats.Failures, 1)
	c.handle(protocol.Envelope{V: protocol.Version, Type: protocol.TypePresenceJoin})
	c.handle(protocol.Envelope{V: protocol.Version, Type: protocol.TypeAuthRefreshed})
	assert.Len(t, c.stats.Failures, 1)
	c.handle(event(t, 1, "board.renamed", nil, map[string]string{"title": "x"}))
	assert.Equal(t, int64(1), c.state.Seq)
	assert.Len(t, c.stats.Failures, 1)
}

func TestServerErrorMessageIsAFailure(t *testing.T) {
	c := newTestClient(t)
	c.handle(protocol.Envelope{V: protocol.Version, Type: protocol.TypeError, Payload: json.RawMessage(`{"code":"x"}`)})
	assert.Len(t, c.stats.Failures, 1)
}

func TestSyncDeltaAppliesEventsInOrderAndReadyGoesLive(t *testing.T) {
	c := newTestClient(t)
	loadEmpty(c, 0)
	delta := protocol.SyncDelta{Events: []protocol.Envelope{
		columnCreatedEvent(t, 1, nil, uuid.New(), "a", "a"),
		columnCreatedEvent(t, 2, nil, uuid.New(), "b", "b"),
	}}
	env, err := protocol.NewEnvelope(protocol.TypeSyncDelta, delta)
	require.NoError(t, err)
	c.handle(env)
	assert.Equal(t, int64(2), c.state.Seq)
	assert.False(t, c.live)
	c.handle(protocol.Envelope{V: protocol.Version, Type: protocol.TypeSyncReady})
	assert.True(t, c.live)
	assert.NotNil(t, c.view)
}

func TestRandomOpRequiresInitializationAndRespectsMaxPending(t *testing.T) {
	c := newTestClient(t)
	assert.False(t, c.randomOp())
	loadEmpty(c, 0)
	c.cfg.MaxPending = 3
	for range 3 {
		assert.True(t, c.randomOp())
	}
	assert.False(t, c.randomOp())
	assert.Len(t, c.pending, 3)
	assert.Equal(t, 3, c.stats.OpsCreated)
	assert.Empty(t, c.stats.Failures)
}

func TestStatusReturnsAnIsolatedCopy(t *testing.T) {
	c := newTestClient(t)
	col := uuid.New()
	c.loadSnapshot(protocol.Snapshot{Seq: 1, Columns: []boardcontent.Column{{ID: col, Title: "T", Rank: "m", Version: 1}}})
	c.stats.Nacks["x"] = 1
	c.stats.Failures = append(c.stats.Failures, "f")
	st := c.status()
	st.State.Columns[col].Title = "mutated"
	st.Stats.Nacks["x"] = 99
	st.Stats.Failures[0] = "changed"
	assert.Equal(t, "T", c.state.Columns[col].Title)
	assert.Equal(t, 1, c.stats.Nacks["x"])
	assert.Equal(t, "f", c.stats.Failures[0])
}

func TestTraceIsBounded(t *testing.T) {
	c := newTestClient(t)
	for i := 0; i < traceLimit*3; i++ {
		c.note("entry %d", i)
	}
	assert.Len(t, c.trace, traceLimit)
	assert.Contains(t, c.trace[len(c.trace)-1], "entry 899")
}

func TestApplyFailureSchedulesASnapshotReloadThatDrainDoesNotCancel(t *testing.T) {
	c, api := newClientWithAPI(t)
	col := uuid.New()
	api.snapshot = func() (int, protocol.Snapshot) {
		return http.StatusOK, protocol.Snapshot{Seq: 5, Columns: []boardcontent.Column{{ID: col, Title: "T", Rank: "m", Version: 1}}}
	}
	loadEmpty(c, 0)
	c.acceptEvent(event(t, 1, boardcontent.EventCardEdited, nil, boardcontent.CardEdited{CardID: uuid.New(), Version: 2}))
	require.False(t, c.retryAt.IsZero())
	assert.Empty(t, c.buf)

	c.onTick(context.Background(), c.retryAt.Add(time.Millisecond))

	assert.True(t, c.retryAt.IsZero())
	assert.Equal(t, int64(5), c.state.Seq)
	assert.Contains(t, c.state.Columns, col)
}
