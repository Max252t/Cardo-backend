package ops_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type replayCase struct {
	name   string
	opType string
	build  func(w *replayWorld) any
}

type replayWorld struct {
	col, other uuid.UUID
	card       uuid.UUID
}

func replayCases() []replayCase {
	return []replayCase{
		{"card.create", protocol.OpCardCreate, func(w *replayWorld) any {
			return protocol.CardCreate{CardID: uuid.New(), ColumnID: w.col, Title: "once"}
		}},
		{"card.edit title", protocol.OpCardEdit, func(w *replayWorld) any {
			return protocol.CardEdit{CardID: w.card, Title: ptr("renamed once")}
		}},
		{"card.edit description", protocol.OpCardEdit, func(w *replayWorld) any {
			return protocol.CardEdit{CardID: w.card, Description: ptr("desc once"), BaseDescriptionVersion: ptr(int64(1))}
		}},
		{"card.move", protocol.OpCardMove, func(w *replayWorld) any {
			return protocol.CardMove{CardID: w.card, ToColumnID: w.other, BaseVersion: ptr(int64(1))}
		}},
		{"card.archive", protocol.OpCardArchive, func(w *replayWorld) any {
			return protocol.CardArchive{CardID: w.card}
		}},
		{"column.create", protocol.OpColumnCreate, func(w *replayWorld) any {
			return protocol.ColumnCreate{ColumnID: uuid.New(), Title: "new column"}
		}},
		{"column.rename", protocol.OpColumnRename, func(w *replayWorld) any {
			return protocol.ColumnRename{ColumnID: w.col, Title: "renamed once"}
		}},
		{"column.move", protocol.OpColumnMove, func(w *replayWorld) any {
			return protocol.ColumnMove{ColumnID: w.col, AfterID: &w.other, BaseVersion: ptr(int64(1))}
		}},
		{"column.archive", protocol.OpColumnArchive, func(w *replayWorld) any {
			return protocol.ColumnArchive{ColumnID: w.col}
		}},
	}
}

func newReplayWorld(t *testing.T, f *fixture) *replayWorld {
	t.Helper()
	w := &replayWorld{}
	w.col = f.column(t, "Todo", nil)
	w.other = f.column(t, "Done", nil)
	w.card = f.card(t, w.col, "card")
	return w
}

func TestSameOperationTenTimesSequentially(t *testing.T) {
	for _, tc := range replayCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			w := newReplayWorld(t, f)
			payload := tc.build(w)
			opID := uuid.New()
			seqBefore := f.boardSeq(t)
			publishedBefore := len(f.pub.types())

			first := f.applyAs(t, f.owner.ID, opID, tc.opType, payload)
			require.NotNil(t, first.Event)
			assert.False(t, first.Replayed)
			stateAfterFirst := f.snapshot(t)

			for i := range 9 {
				again := f.applyAs(t, f.owner.ID, opID, tc.opType, payload)
				require.NotNil(t, again.Event, "replay %d", i)
				assert.True(t, again.Replayed)
				assert.Nil(t, again.Nack)
				assert.Empty(t, again.Extra)
				assert.Equal(t, first.Event.Seq, again.Event.Seq)
				assert.Equal(t, first.Event.Type, again.Event.Type)
				assert.Equal(t, first.Event.OpID, again.Event.OpID)
				assert.Equal(t, first.Event.ActorID, again.Event.ActorID)
				assert.JSONEq(t, string(first.Event.Payload), string(again.Event.Payload))
				assert.True(t, first.Event.CreatedAt.Equal(again.Event.CreatedAt))
			}

			stateAfterReplays := f.snapshot(t)
			assert.Equal(t, stateAfterFirst.Cards, stateAfterReplays.Cards)
			assert.Equal(t, stateAfterFirst.Columns, stateAfterReplays.Columns)
			assert.Equal(t, seqBefore+1, f.requireGapFreeSeq(t), "seq is bumped exactly once")
			assert.Len(t, f.pub.types(), publishedBefore+1, "only the first execution is published")
			events, _ := f.processedOps(t)
			assert.Equal(t, int(seqBefore)+1, events)
		})
	}
}

func TestSameOperationTenTimesConcurrently(t *testing.T) {
	for _, tc := range replayCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newConcurrentFixture(t)
			w := newReplayWorld(t, f)
			payload := tc.build(w)
			opID := uuid.New()
			seqBefore := f.boardSeq(t)
			publishedBefore := len(f.pub.types())

			const copies = 10
			results := f.applyConcurrently(t, copies, func(int) (uuid.UUID, protocol.Op) {
				return f.owner.ID, mustOp(t, tc.opType, opID, payload)
			})

			fresh := 0
			var seq int64
			for _, r := range results {
				require.NotNil(t, r.out.Event)
				require.Nil(t, r.out.Nack)
				if !r.out.Replayed {
					fresh++
				}
				if seq == 0 {
					seq = r.out.Event.Seq
				}
				assert.Equal(t, seq, r.out.Event.Seq, "every copy reports the same event")
				assert.JSONEq(t, string(results[0].out.Event.Payload), string(r.out.Event.Payload))
			}
			assert.Equal(t, 1, fresh, "exactly one copy executes")
			assert.Equal(t, seqBefore+1, f.requireGapFreeSeq(t))
			assert.Len(t, f.pub.types(), publishedBefore+1)
		})
	}
}

func TestSameCreateTenTimesConcurrentlyCreatesOneCard(t *testing.T) {
	f := newConcurrentFixture(t)
	col := f.column(t, "Todo", nil)
	opID, cardID := uuid.New(), uuid.New()
	results := f.applyConcurrently(t, 10, func(int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardCreate, opID, protocol.CardCreate{CardID: cardID, ColumnID: col, Title: "dedupe"})
	})
	for _, r := range results {
		require.NotNil(t, r.out.Event)
	}
	assert.Equal(t, []uuid.UUID{cardID}, f.cardsInColumn(t, col))
	assert.Equal(t, int64(1), f.cardRow(t, cardID).Version)
}

func TestReplayedNackIsIdenticalSequentiallyAndConcurrently(t *testing.T) {
	f := newConcurrentFixture(t)
	w := newReplayWorld(t, f)
	opID := uuid.New()
	payload := protocol.CardMove{CardID: w.card, ToColumnID: w.other, BaseVersion: ptr(int64(5))}

	first := f.applyAs(t, f.owner.ID, opID, protocol.OpCardMove, payload)
	require.NotNil(t, first.Nack)
	assert.Equal(t, protocol.ReasonVersionConflict, first.Nack.Reason)
	assert.False(t, first.Replayed)

	f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: w.card, Title: ptr("moves the seq")})
	seq := f.boardSeq(t)
	require.Greater(t, seq, first.Nack.AtSeq)

	results := f.applyConcurrently(t, 10, func(int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardMove, opID, payload)
	})
	for _, r := range results {
		require.NotNil(t, r.out.Nack)
		assert.True(t, r.out.Replayed)
		assert.Equal(t, *first.Nack, *r.out.Nack, "the stored nack is returned, including its original at_seq")
		assert.Nil(t, r.out.Event)
	}
	f.requireUnchanged(t, seq)
}

func TestConcurrentCopiesOfAFailingOperationStoreOneNack(t *testing.T) {
	f := newConcurrentFixture(t)
	w := newReplayWorld(t, f)
	opID := uuid.New()
	payload := protocol.CardMove{CardID: w.card, ToColumnID: w.other, BaseVersion: ptr(int64(5))}
	_, nacksBefore := f.processedOps(t)

	results := f.applyConcurrently(t, 10, func(int) (uuid.UUID, protocol.Op) {
		return f.owner.ID, mustOp(t, protocol.OpCardMove, opID, payload)
	})
	fresh := 0
	for _, r := range results {
		require.NotNil(t, r.out.Nack)
		if !r.out.Replayed {
			fresh++
		}
	}
	assert.Equal(t, 1, fresh)
	_, nacksAfter := f.processedOps(t)
	assert.Equal(t, nacksBefore+1, nacksAfter)
}

func TestOpIDReuseByAnotherUserIsRejected(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	editor := f.member(t, domain.RoleEditor)
	opID := uuid.New()
	cardID := uuid.New()

	first := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, protocol.CardCreate{CardID: cardID, ColumnID: col, Title: "mine"})
	require.NotNil(t, first.Event)
	seq := f.boardSeq(t)
	published := len(f.pub.types())

	for name, payload := range map[string]any{
		"same payload":      protocol.CardCreate{CardID: cardID, ColumnID: col, Title: "mine"},
		"different payload": protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "stolen"},
	} {
		t.Run(name, func(t *testing.T) {
			out := f.applyAs(t, editor.ID, opID, protocol.OpCardCreate, payload)
			require.NotNil(t, out.Nack)
			assert.Equal(t, protocol.ReasonValidation, out.Nack.Reason)
			assert.Nil(t, out.Event)
			assert.False(t, out.Replayed)
			assert.Equal(t, seq, out.Nack.AtSeq)
			assert.NotContains(t, out.Nack.Message, cardID.String())
		})
	}

	f.requireUnchanged(t, seq)
	assert.Len(t, f.pub.types(), published)
	assert.Equal(t, []uuid.UUID{cardID}, f.cardsInColumn(t, col))

	var owner uuid.UUID
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT user_id FROM processed_ops WHERE op_id = $1`, opID).Scan(&owner))
	assert.Equal(t, f.owner.ID, owner, "the original record is not overwritten")

	again := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, protocol.CardCreate{CardID: cardID, ColumnID: col, Title: "mine"})
	require.NotNil(t, again.Event)
	assert.True(t, again.Replayed)
	assert.Equal(t, first.Event.Seq, again.Event.Seq)
}

func TestOpIDReuseOnAnotherBoardIsRejected(t *testing.T) {
	f := newFixture(t)
	f.column(t, "Todo", nil)
	otherBoard := f.newBoard(t, f.owner.ID)
	opID := uuid.New()

	first := f.applyAs(t, f.owner.ID, opID, protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: uuid.New(), Title: "here"})
	require.NotNil(t, first.Event)

	out := f.applyOnBoard(t, otherBoard, f.owner.ID, opID, protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: uuid.New(), Title: "there"})
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonValidation, out.Nack.Reason)
	assert.Zero(t, out.Nack.AtSeq)

	var cols int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM columns WHERE board_id = $1`, otherBoard).Scan(&cols))
	assert.Zero(t, cols)
}

func TestReplayIgnoresChangedPayload(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	opID := uuid.New()
	cardID := uuid.New()
	first := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, protocol.CardCreate{CardID: cardID, ColumnID: col, Title: "original"})
	require.NotNil(t, first.Event)

	again := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "different"})
	require.NotNil(t, again.Event)
	assert.True(t, again.Replayed)
	assert.JSONEq(t, string(first.Event.Payload), string(again.Event.Payload))
	assert.Equal(t, []uuid.UUID{cardID}, f.cardsInColumn(t, col))

	asEdit := f.applyAs(t, f.owner.ID, opID, protocol.OpCardArchive, protocol.CardArchive{CardID: cardID})
	require.NotNil(t, asEdit.Event)
	assert.True(t, asEdit.Replayed, "an op_id is bound to its first execution regardless of the type sent later")
	assert.False(t, f.cardRow(t, cardID).Archived)
}

func TestReplayOfAnOperationWithRebalanceReturnsOnlyThePrimaryEvent(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "first")
	last := f.card(t, col, "last")
	f.setCardRank(t, first, "a0")
	f.setCardRank(t, last, "a0"+strings.Repeat("0", 62)+"1")

	opID := uuid.New()
	payload := protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "mid", AfterID: &first, BeforeID: &last}
	out := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, payload)
	require.NotNil(t, out.Event)
	require.Len(t, out.Extra, 1)
	published := len(f.pub.types())

	again := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, payload)
	require.NotNil(t, again.Event)
	assert.True(t, again.Replayed)
	assert.Equal(t, out.Event.Seq, again.Event.Seq)
	assert.Empty(t, again.Extra, "the rebalance event is delivered once and is recoverable from the event log")
	assert.Len(t, f.pub.types(), published)
}

func TestProcessorUsesTheSameBoardLockForReplay(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	opID := uuid.New()
	f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})

	_, err := f.pool.Exec(context.Background(), `UPDATE boards SET deleted_at = now() WHERE id = $1`, f.board.ID)
	require.NoError(t, err)

	op := mustOp(t, protocol.OpCardCreate, opID, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	_, err = f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	assert.ErrorIs(t, err, domain.ErrNotFound, "a deleted board answers not found even for replays")
}

func TestOutcomeAgainstProcessorOptions(t *testing.T) {
	f := newFixture(t)
	proc := ops.NewProcessor(f.store, ops.WithPublisher(nil), ops.WithLogger(nil))
	col := uuid.New()
	op := mustOp(t, protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{ColumnID: col, Title: "nil options are ignored"})
	out, err := proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	require.NotNil(t, out.Event)
}
