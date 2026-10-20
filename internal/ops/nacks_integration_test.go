package ops_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type nackCase struct {
	name    string
	opType  string
	payload func(w *world) any
	reason  string
}

type world struct {
	todo, done   uuid.UUID
	card, other  uuid.UUID
	archivedCard uuid.UUID
	archivedCol  uuid.UUID
	foreignCard  uuid.UUID
	foreignCol   uuid.UUID
}

func buildWorld(t *testing.T, f *fixture) *world {
	t.Helper()
	w := &world{}
	w.todo = f.column(t, "Todo", nil)
	w.done = f.column(t, "Done", nil)
	w.card = f.card(t, w.todo, "A")
	w.other = f.card(t, w.done, "B")
	w.archivedCard = f.card(t, w.todo, "Gone")
	f.mustEvent(t, protocol.OpCardArchive, protocol.CardArchive{CardID: w.archivedCard})
	w.archivedCol = f.column(t, "Archived", nil)
	f.mustEvent(t, protocol.OpColumnArchive, protocol.ColumnArchive{ColumnID: w.archivedCol})

	foreign := f.newBoard(t, f.owner.ID)
	w.foreignCol = uuid.New()
	w.foreignCard = uuid.New()
	out := f.applyOnBoard(t, foreign, f.owner.ID, uuid.New(), protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: w.foreignCol, Title: "Foreign"})
	require.Nil(t, out.Nack)
	out = f.applyOnBoard(t, foreign, f.owner.ID, uuid.New(), protocol.OpCardCreate, protocol.CardCreate{CardID: w.foreignCard, ColumnID: w.foreignCol, Title: "Foreign card"})
	require.Nil(t, out.Nack)
	return w
}

func nackCases() []nackCase {
	v := func(n int64) *int64 { return &n }
	s := func(x string) *string { return &x }
	id := uuid.New
	return []nackCase{
		{"card.create without card_id", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{ColumnID: w.todo, Title: "x"}
		}, protocol.ReasonValidation},
		{"card.create without column_id", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), Title: "x"}
		}, protocol.ReasonValidation},
		{"card.create blank title", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: w.todo, Title: "  \t"}
		}, protocol.ReasonValidation},
		{"card.create title too long", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: w.todo, Title: strings.Repeat("т", 501)}
		}, protocol.ReasonValidation},
		{"card.create title with control character", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: w.todo, Title: "a\x07b"}
		}, protocol.ReasonValidation},
		{"card.create description too long", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: w.todo, Title: "x", Description: s(strings.Repeat("d", 10001))}
		}, protocol.ReasonValidation},
		{"card.create description with control character", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: w.todo, Title: "x", Description: s("a\x00b")}
		}, protocol.ReasonValidation},
		{"card.create id already used", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: w.card, ColumnID: w.todo, Title: "x"}
		}, protocol.ReasonValidation},
		{"card.create id used by an archived card", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: w.archivedCard, ColumnID: w.todo, Title: "x"}
		}, protocol.ReasonValidation},
		{"card.create unknown column", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: id(), Title: "x"}
		}, protocol.ReasonNotFound},
		{"card.create in archived column", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: w.archivedCol, Title: "x"}
		}, protocol.ReasonNotFound},
		{"card.create in a column of another board", protocol.OpCardCreate, func(w *world) any {
			return protocol.CardCreate{CardID: id(), ColumnID: w.foreignCol, Title: "x"}
		}, protocol.ReasonNotFound},

		{"card.edit without card_id", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{Title: s("x")}
		}, protocol.ReasonValidation},
		{"card.edit without fields", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card}
		}, protocol.ReasonValidation},
		{"card.edit blank title", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card, Title: s(" ")}
		}, protocol.ReasonValidation},
		{"card.edit description without base version", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card, Description: s("d")}
		}, protocol.ReasonValidation},
		{"card.edit base version without description", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card, Title: s("x"), BaseDescriptionVersion: v(1)}
		}, protocol.ReasonValidation},
		{"card.edit zero base version", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card, Description: s("d"), BaseDescriptionVersion: v(0)}
		}, protocol.ReasonValidation},
		{"card.edit negative base version", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card, Description: s("d"), BaseDescriptionVersion: v(-3)}
		}, protocol.ReasonValidation},
		{"card.edit description too long", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card, Description: s(strings.Repeat("d", 10001)), BaseDescriptionVersion: v(1)}
		}, protocol.ReasonValidation},
		{"card.edit stale description version", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.card, Description: s("d"), BaseDescriptionVersion: v(7)}
		}, protocol.ReasonVersionConflict},
		{"card.edit unknown card", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: id(), Title: s("x")}
		}, protocol.ReasonNotFound},
		{"card.edit archived card", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.archivedCard, Title: s("x")}
		}, protocol.ReasonNotFound},
		{"card.edit card of another board", protocol.OpCardEdit, func(w *world) any {
			return protocol.CardEdit{CardID: w.foreignCard, Title: s("x")}
		}, protocol.ReasonNotFound},

		{"card.move without card_id", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{ToColumnID: w.done, BaseVersion: v(1)}
		}, protocol.ReasonValidation},
		{"card.move without to_column_id", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.card, BaseVersion: v(1)}
		}, protocol.ReasonValidation},
		{"card.move without base_version", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.card, ToColumnID: w.done}
		}, protocol.ReasonValidation},
		{"card.move zero base_version", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.card, ToColumnID: w.done, BaseVersion: v(0)}
		}, protocol.ReasonValidation},
		{"card.move stale base_version", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.card, ToColumnID: w.done, BaseVersion: v(5)}
		}, protocol.ReasonVersionConflict},
		{"card.move unknown card", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: id(), ToColumnID: w.done, BaseVersion: v(1)}
		}, protocol.ReasonNotFound},
		{"card.move archived card", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.archivedCard, ToColumnID: w.done, BaseVersion: v(1)}
		}, protocol.ReasonNotFound},
		{"card.move to unknown column", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.card, ToColumnID: id(), BaseVersion: v(1)}
		}, protocol.ReasonNotFound},
		{"card.move to archived column", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.card, ToColumnID: w.archivedCol, BaseVersion: v(1)}
		}, protocol.ReasonNotFound},
		{"card.move to a column of another board", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.card, ToColumnID: w.foreignCol, BaseVersion: v(1)}
		}, protocol.ReasonNotFound},
		{"card.move card of another board", protocol.OpCardMove, func(w *world) any {
			return protocol.CardMove{CardID: w.foreignCard, ToColumnID: w.done, BaseVersion: v(1)}
		}, protocol.ReasonNotFound},

		{"card.archive without card_id", protocol.OpCardArchive, func(w *world) any {
			return protocol.CardArchive{}
		}, protocol.ReasonValidation},
		{"card.archive unknown card", protocol.OpCardArchive, func(w *world) any {
			return protocol.CardArchive{CardID: id()}
		}, protocol.ReasonNotFound},
		{"card.archive already archived", protocol.OpCardArchive, func(w *world) any {
			return protocol.CardArchive{CardID: w.archivedCard}
		}, protocol.ReasonNotFound},
		{"card.archive card of another board", protocol.OpCardArchive, func(w *world) any {
			return protocol.CardArchive{CardID: w.foreignCard}
		}, protocol.ReasonNotFound},

		{"column.create without column_id", protocol.OpColumnCreate, func(w *world) any {
			return protocol.ColumnCreate{Title: "x"}
		}, protocol.ReasonValidation},
		{"column.create blank title", protocol.OpColumnCreate, func(w *world) any {
			return protocol.ColumnCreate{ColumnID: id(), Title: ""}
		}, protocol.ReasonValidation},
		{"column.create title too long", protocol.OpColumnCreate, func(w *world) any {
			return protocol.ColumnCreate{ColumnID: id(), Title: strings.Repeat("c", 201)}
		}, protocol.ReasonValidation},
		{"column.create id already used", protocol.OpColumnCreate, func(w *world) any {
			return protocol.ColumnCreate{ColumnID: w.todo, Title: "x"}
		}, protocol.ReasonValidation},
		{"column.create id used by an archived column", protocol.OpColumnCreate, func(w *world) any {
			return protocol.ColumnCreate{ColumnID: w.archivedCol, Title: "x"}
		}, protocol.ReasonValidation},
		{"column.create id used on another board", protocol.OpColumnCreate, func(w *world) any {
			return protocol.ColumnCreate{ColumnID: w.foreignCol, Title: "x"}
		}, protocol.ReasonValidation},

		{"column.rename without column_id", protocol.OpColumnRename, func(w *world) any {
			return protocol.ColumnRename{Title: "x"}
		}, protocol.ReasonValidation},
		{"column.rename blank title", protocol.OpColumnRename, func(w *world) any {
			return protocol.ColumnRename{ColumnID: w.todo, Title: "   "}
		}, protocol.ReasonValidation},
		{"column.rename unknown column", protocol.OpColumnRename, func(w *world) any {
			return protocol.ColumnRename{ColumnID: id(), Title: "x"}
		}, protocol.ReasonNotFound},
		{"column.rename archived column", protocol.OpColumnRename, func(w *world) any {
			return protocol.ColumnRename{ColumnID: w.archivedCol, Title: "x"}
		}, protocol.ReasonNotFound},
		{"column.rename column of another board", protocol.OpColumnRename, func(w *world) any {
			return protocol.ColumnRename{ColumnID: w.foreignCol, Title: "x"}
		}, protocol.ReasonNotFound},

		{"column.move without column_id", protocol.OpColumnMove, func(w *world) any {
			return protocol.ColumnMove{BaseVersion: v(1)}
		}, protocol.ReasonValidation},
		{"column.move without base_version", protocol.OpColumnMove, func(w *world) any {
			return protocol.ColumnMove{ColumnID: w.todo}
		}, protocol.ReasonValidation},
		{"column.move zero base_version", protocol.OpColumnMove, func(w *world) any {
			return protocol.ColumnMove{ColumnID: w.todo, BaseVersion: v(0)}
		}, protocol.ReasonValidation},
		{"column.move stale base_version", protocol.OpColumnMove, func(w *world) any {
			return protocol.ColumnMove{ColumnID: w.todo, BaseVersion: v(9)}
		}, protocol.ReasonVersionConflict},
		{"column.move unknown column", protocol.OpColumnMove, func(w *world) any {
			return protocol.ColumnMove{ColumnID: id(), BaseVersion: v(1)}
		}, protocol.ReasonNotFound},
		{"column.move archived column", protocol.OpColumnMove, func(w *world) any {
			return protocol.ColumnMove{ColumnID: w.archivedCol, BaseVersion: v(1)}
		}, protocol.ReasonNotFound},

		{"column.archive without column_id", protocol.OpColumnArchive, func(w *world) any {
			return protocol.ColumnArchive{}
		}, protocol.ReasonValidation},
		{"column.archive unknown column", protocol.OpColumnArchive, func(w *world) any {
			return protocol.ColumnArchive{ColumnID: id()}
		}, protocol.ReasonNotFound},
		{"column.archive already archived", protocol.OpColumnArchive, func(w *world) any {
			return protocol.ColumnArchive{ColumnID: w.archivedCol}
		}, protocol.ReasonNotFound},
		{"column.archive column of another board", protocol.OpColumnArchive, func(w *world) any {
			return protocol.ColumnArchive{ColumnID: w.foreignCol}
		}, protocol.ReasonNotFound},
	}
}

func TestEveryNackReason(t *testing.T) {
	f := newFixture(t)
	w := buildWorld(t, f)
	published := len(f.pub.types())

	for _, tc := range nackCases() {
		t.Run(tc.name, func(t *testing.T) {
			seq := f.boardSeq(t)
			_, nacksBefore := f.processedOps(t)

			opID := uuid.New()
			out := f.applyAs(t, f.owner.ID, opID, tc.opType, tc.payload(w))

			require.NotNil(t, out.Nack, "expected a nack")
			assert.Nil(t, out.Event)
			assert.Empty(t, out.Extra)
			assert.False(t, out.Replayed)
			assert.Equal(t, tc.reason, out.Nack.Reason)
			assert.NotEmpty(t, out.Nack.Message)
			assert.Equal(t, seq, out.Nack.AtSeq)
			f.requireUnchanged(t, seq)

			_, nacksAfter := f.processedOps(t)
			assert.Equal(t, nacksBefore+1, nacksAfter, "the nack is remembered for idempotent replays")
			assert.Len(t, f.pub.types(), published, "nothing is published for a nack")

			again := f.applyAs(t, f.owner.ID, opID, tc.opType, tc.payload(w))
			require.NotNil(t, again.Nack)
			assert.True(t, again.Replayed)
			assert.Equal(t, *out.Nack, *again.Nack)
		})
	}
}

func TestNackDoesNotLeakPartialWrites(t *testing.T) {
	f := newFixture(t)
	w := buildWorld(t, f)
	before := f.cardRow(t, w.card)

	f.mustNack(t, protocol.ReasonVersionConflict, protocol.OpCardEdit, protocol.CardEdit{
		CardID: w.card, Title: ptr("sneaky title"), Description: ptr("d"), BaseDescriptionVersion: ptr(int64(9)),
	})
	assert.Equal(t, before, f.cardRow(t, w.card), "a stale description must roll back the title change in the same op")

	f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardMove, protocol.CardMove{
		CardID: w.card, ToColumnID: uuid.New(), BaseVersion: ptr(int64(1)),
	})
	assert.Equal(t, before, f.cardRow(t, w.card))

	f.mustNack(t, protocol.ReasonVersionConflict, protocol.OpCardMove, protocol.CardMove{
		CardID: w.card, ToColumnID: w.done, BaseVersion: ptr(int64(42)),
	})
	assert.Equal(t, before, f.cardRow(t, w.card))
	assert.Len(t, f.cardsInColumn(t, w.todo), 1)
}

func TestForbiddenForViewersAndOutsidersOnEveryOpType(t *testing.T) {
	f := newFixture(t)
	w := buildWorld(t, f)
	viewer := f.member(t, domain.RoleViewer)
	outsider := testutil.CreateUser(t, f.store, testutil.UniqueEmail("outsider"))

	payloads := map[string]any{
		protocol.OpCardCreate:    protocol.CardCreate{CardID: uuid.New(), ColumnID: w.todo, Title: "x"},
		protocol.OpCardEdit:      protocol.CardEdit{CardID: w.card, Title: ptr("x")},
		protocol.OpCardMove:      protocol.CardMove{CardID: w.card, ToColumnID: w.done, BaseVersion: ptr(int64(1))},
		protocol.OpCardArchive:   protocol.CardArchive{CardID: w.card},
		protocol.OpColumnCreate:  protocol.ColumnCreate{ColumnID: uuid.New(), Title: "x"},
		protocol.OpColumnRename:  protocol.ColumnRename{ColumnID: w.todo, Title: "x"},
		protocol.OpColumnMove:    protocol.ColumnMove{ColumnID: w.todo, BaseVersion: ptr(int64(1))},
		protocol.OpColumnArchive: protocol.ColumnArchive{ColumnID: w.todo},
	}
	require.Len(t, payloads, 8)

	snapshotBefore := f.snapshot(t)
	for opType, payload := range payloads {
		for name, user := range map[string]uuid.UUID{"viewer": viewer.ID, "outsider": outsider.ID} {
			t.Run(opType+" as "+name, func(t *testing.T) {
				seq := f.boardSeq(t)
				published := len(f.pub.types())
				out := f.applyAs(t, user, uuid.New(), opType, payload)
				require.NotNil(t, out.Nack)
				assert.Equal(t, protocol.ReasonForbidden, out.Nack.Reason)
				assert.Equal(t, seq, out.Nack.AtSeq)
				assert.Nil(t, out.Event)
				f.requireUnchanged(t, seq)
				assert.Len(t, f.pub.types(), published)
			})
		}
	}
	snapshotAfter := f.snapshot(t)
	assert.Equal(t, snapshotBefore.Cards, snapshotAfter.Cards)
	assert.Equal(t, snapshotBefore.Columns, snapshotAfter.Columns)
}

func TestForbiddenIsCheckedBeforePayloadValidation(t *testing.T) {
	f := newFixture(t)
	viewer := f.member(t, domain.RoleViewer)
	out := f.applyRaw(t, viewer.ID, protocol.OpCardCreate, `{"nonsense":true}`)
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonForbidden, out.Nack.Reason)
}

func TestForbiddenNackIsStickyForReplays(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	user := f.member(t, domain.RoleViewer)
	opID := uuid.New()
	payload := protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"}

	first := f.applyAs(t, user.ID, opID, protocol.OpCardCreate, payload)
	require.NotNil(t, first.Nack)
	assert.Equal(t, protocol.ReasonForbidden, first.Nack.Reason)

	_, err := f.pool.Exec(context.Background(), `UPDATE board_members SET role = 'editor' WHERE board_id = $1 AND user_id = $2`, f.board.ID, user.ID)
	require.NoError(t, err)

	again := f.applyAs(t, user.ID, opID, protocol.OpCardCreate, payload)
	require.NotNil(t, again.Nack)
	assert.True(t, again.Replayed)
	assert.Equal(t, protocol.ReasonForbidden, again.Nack.Reason)

	fresh := f.applyAs(t, user.ID, uuid.New(), protocol.OpCardCreate, payload)
	assert.Nil(t, fresh.Nack, "a new op_id works once the role is upgraded")
}

func TestMemberRoleMatrix(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	tests := []struct {
		role      domain.Role
		forbidden bool
	}{
		{domain.RoleOwner, false},
		{domain.RoleEditor, false},
		{domain.RoleViewer, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			user := f.member(t, tt.role)
			out := f.applyAs(t, user.ID, uuid.New(), protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
			if tt.forbidden {
				require.NotNil(t, out.Nack)
				assert.Equal(t, protocol.ReasonForbidden, out.Nack.Reason)
				return
			}
			require.Nil(t, out.Nack)
			require.NotNil(t, out.Event)
			require.NotNil(t, out.Event.ActorID)
			assert.Equal(t, user.ID, *out.Event.ActorID)
			assert.Equal(t, protocol.EventCardCreated, out.Event.Type)
		})
	}
}

func TestDeletedBoardIsNotFoundNotNack(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	_, err := f.pool.Exec(context.Background(), `UPDATE boards SET deleted_at = now() WHERE id = $1`, f.board.ID)
	require.NoError(t, err)
	published := len(f.pub.types())

	op := mustOp(t, protocol.OpCardCreate, uuid.New(), protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	out, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.ErrorIs(t, err, domain.ErrNotFound)
	assert.Nil(t, out.Event)
	assert.Nil(t, out.Nack)

	var stored int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_ops WHERE op_id = $1`, op.OpID).Scan(&stored))
	assert.Zero(t, stored, "an infrastructure-level not found is not remembered as an outcome")
	assert.Len(t, f.pub.types(), published)
}

func TestUnknownBoardIsNotFoundForEveryUser(t *testing.T) {
	f := newFixture(t)
	outsider := testutil.CreateUser(t, f.store, testutil.UniqueEmail("outsider"))
	op := mustOp(t, protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{ColumnID: uuid.New(), Title: "x"})
	for _, user := range []uuid.UUID{f.owner.ID, outsider.ID, uuid.New()} {
		_, err := f.proc.Apply(context.Background(), user, uuid.New(), op)
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}
}

func TestApplyRejectsNilOpID(t *testing.T) {
	f := newFixture(t)
	op := protocol.Op{Type: protocol.OpColumnCreate, Payload: json.RawMessage(`{}`)}
	_, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	var vErr *domain.ValidationError
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, "op_id", vErr.Field)
}

func TestRawPayloadProblemsBecomeValidationNacks(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	seq := f.boardSeq(t)
	tests := []struct {
		name    string
		opType  string
		payload string
	}{
		{"unknown field", protocol.OpColumnArchive, `{"column_id":"` + col.String() + `","force":true}`},
		{"wrong field type", protocol.OpColumnRename, `{"column_id":"` + col.String() + `","title":7}`},
		{"not an object", protocol.OpColumnArchive, `[1,2]`},
		{"empty payload", protocol.OpColumnArchive, ``},
		{"two documents", protocol.OpColumnArchive, `{}{}`},
		{"invalid uuid", protocol.OpColumnArchive, `{"column_id":"abc"}`},
		{"unknown op type", "op.column.explode", `{}`},
		{"event type used as op", protocol.EventCardCreated, `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := f.applyRaw(t, f.owner.ID, tt.opType, tt.payload)
			require.NotNil(t, out.Nack)
			assert.Equal(t, protocol.ReasonValidation, out.Nack.Reason)
			assert.Equal(t, seq, out.Nack.AtSeq)
		})
	}
	f.requireUnchanged(t, seq)
}

func TestOutcomeEvents(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "first")
	last := f.card(t, col, "last")
	f.setCardRank(t, first, "a0")
	f.setCardRank(t, last, "a0"+strings.Repeat("0", 62)+"1")

	out := f.apply(t, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "mid", AfterID: &first, BeforeID: &last})
	require.NotNil(t, out.Event)
	require.Len(t, out.Extra, 1)
	events := out.Events()
	require.Len(t, events, 2)
	assert.Equal(t, out.Extra[0], events[0], "events are returned in seq order")
	assert.Equal(t, *out.Event, events[1])
	assert.Less(t, events[0].Seq, events[1].Seq)

	nack := f.apply(t, protocol.OpCardArchive, protocol.CardArchive{CardID: uuid.New()})
	assert.Nil(t, nack.Events())
}
