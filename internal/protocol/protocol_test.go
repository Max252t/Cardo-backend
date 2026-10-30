package protocol_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

var (
	opID  = uuid.MustParse("8f0c0000-0000-7000-8000-000000000001")
	cardA = uuid.MustParse("c1000000-0000-7000-8000-000000000001")
	cardB = uuid.MustParse("c2000000-0000-7000-8000-000000000002")
	colA  = uuid.MustParse("a1000000-0000-7000-8000-000000000001")
	colB  = uuid.MustParse("a2000000-0000-7000-8000-000000000002")
)

func envelope(opType, payload string) []byte {
	return []byte(`{"v":1,"type":"` + opType + `","op_id":"` + opID.String() + `","payload":` + payload + `}`)
}

func TestParseOpAcceptsEveryOpType(t *testing.T) {
	t.Parallel()
	types := []string{
		boardcontent.OpCardCreate, boardcontent.OpCardEdit, boardcontent.OpCardMove, boardcontent.OpCardArchive,
		boardcontent.OpColumnCreate, boardcontent.OpColumnRename, boardcontent.OpColumnMove, boardcontent.OpColumnArchive,
	}
	for _, typ := range types {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()
			op, err := protocol.ParseOp(envelope(typ, `{"k":1}`))
			require.NoError(t, err)
			assert.Equal(t, typ, op.Type)
			assert.Equal(t, opID, op.OpID)
			assert.JSONEq(t, `{"k":1}`, string(op.Payload))
			assert.True(t, boardcontent.IsOpType(typ))
		})
	}
}

func TestParseOpIgnoresServerOnlyEnvelopeFields(t *testing.T) {
	t.Parallel()
	data := `{"v":1,"type":"op.card.archive","op_id":"` + opID.String() + `","seq":99,"server_ts":123,"extra":true,"payload":{"card_id":"` + cardA.String() + `"}}`
	op, err := protocol.ParseOp([]byte(data))
	require.NoError(t, err)
	assert.Equal(t, boardcontent.OpCardArchive, op.Type)
}

func TestParseOpWithoutPayload(t *testing.T) {
	t.Parallel()
	op, err := protocol.ParseOp([]byte(`{"v":1,"type":"op.card.archive","op_id":"` + opID.String() + `"}`))
	require.NoError(t, err)
	assert.Empty(t, op.Payload)
	_, err = boardcontent.DecodePayload(op)
	var vErr *domain.ValidationError
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, "payload", vErr.Field)
}

func TestParseOpErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want error
	}{
		{"empty input", ``, protocol.ErrMalformed},
		{"not json", `hello`, protocol.ErrMalformed},
		{"truncated json", `{"v":1,`, protocol.ErrMalformed},
		{"json array", `[]`, protocol.ErrMalformed},
		{"json string", `"op.card.move"`, protocol.ErrMalformed},
		{"version as string", `{"v":"1","type":"op.card.archive","op_id":"` + opID.String() + `"}`, protocol.ErrMalformed},
		{"op_id not a uuid", `{"v":1,"type":"op.card.archive","op_id":"nope"}`, protocol.ErrMalformed},
		{"op_id as number", `{"v":1,"type":"op.card.archive","op_id":5}`, protocol.ErrMalformed},
		{"missing op_id", `{"v":1,"type":"op.card.archive"}`, protocol.ErrMalformed},
		{"null op_id", `{"v":1,"type":"op.card.archive","op_id":null}`, protocol.ErrMalformed},
		{"nil uuid op_id", `{"v":1,"type":"op.card.archive","op_id":"00000000-0000-0000-0000-000000000000"}`, protocol.ErrMalformed},
		{"missing version", `{"type":"op.card.archive","op_id":"` + opID.String() + `"}`, protocol.ErrUnsupportedVersion},
		{"zero version", `{"v":0,"type":"op.card.archive","op_id":"` + opID.String() + `"}`, protocol.ErrUnsupportedVersion},
		{"future version", `{"v":2,"type":"op.card.archive","op_id":"` + opID.String() + `"}`, protocol.ErrUnsupportedVersion},
		{"negative version", `{"v":-1,"type":"op.card.archive","op_id":"` + opID.String() + `"}`, protocol.ErrUnsupportedVersion},
		{"missing type", `{"v":1,"op_id":"` + opID.String() + `"}`, boardcontent.ErrUnknownType},
		{"empty type", `{"v":1,"type":"","op_id":"` + opID.String() + `"}`, boardcontent.ErrUnknownType},
		{"unknown type", `{"v":1,"type":"op.card.explode","op_id":"` + opID.String() + `"}`, boardcontent.ErrUnknownType},
		{"event type is not an op", `{"v":1,"type":"card.created","op_id":"` + opID.String() + `"}`, boardcontent.ErrUnknownType},
		{"nack is not an op", `{"v":1,"type":"nack","op_id":"` + opID.String() + `"}`, boardcontent.ErrUnknownType},
		{"hello is not an op", `{"v":1,"type":"hello"}`, boardcontent.ErrUnknownType},
		{"type is case sensitive", `{"v":1,"type":"OP.CARD.ARCHIVE","op_id":"` + opID.String() + `"}`, boardcontent.ErrUnknownType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			op, err := protocol.ParseOp([]byte(tt.data))
			require.ErrorIs(t, err, tt.want)
			assert.Equal(t, boardcontent.Op{}, op)
		})
	}
}

func TestParseOpVersionCheckPrecedesTypeCheck(t *testing.T) {
	t.Parallel()
	_, err := protocol.ParseOp([]byte(`{"v":9,"type":"nonsense"}`))
	assert.ErrorIs(t, err, protocol.ErrUnsupportedVersion)
	assert.NotErrorIs(t, err, boardcontent.ErrUnknownType)
}

func TestIsOpType(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"", "card.created", "nack", "op.card", "op.board.rename"} {
		assert.False(t, boardcontent.IsOpType(typ), typ)
	}
}

func TestDecodePayloadHappyPaths(t *testing.T) {
	t.Parallel()
	title := "T"
	desc := "D"
	version := int64(14)

	tests := []struct {
		name    string
		opType  string
		payload string
		want    any
	}{
		{
			"card create full", boardcontent.OpCardCreate,
			`{"card_id":"` + cardA.String() + `","column_id":"` + colA.String() + `","title":"T","description":"D","after_id":"` + cardB.String() + `","before_id":"` + cardA.String() + `"}`,
			&boardcontent.CardCreate{CardID: cardA, ColumnID: colA, Title: "T", Description: &desc, AfterID: &cardB, BeforeID: &cardA},
		},
		{
			"card create minimal", boardcontent.OpCardCreate,
			`{"card_id":"` + cardA.String() + `","column_id":"` + colA.String() + `","title":"T"}`,
			&boardcontent.CardCreate{CardID: cardA, ColumnID: colA, Title: "T"},
		},
		{
			"card edit title", boardcontent.OpCardEdit,
			`{"card_id":"` + cardA.String() + `","title":"T"}`,
			&boardcontent.CardEdit{CardID: cardA, Title: &title},
		},
		{
			"card edit description", boardcontent.OpCardEdit,
			`{"card_id":"` + cardA.String() + `","description":"D","base_description_version":14}`,
			&boardcontent.CardEdit{CardID: cardA, Description: &desc, BaseDescriptionVersion: &version},
		},
		{
			"card move", boardcontent.OpCardMove,
			`{"card_id":"` + cardA.String() + `","to_column_id":"` + colB.String() + `","after_id":"` + cardB.String() + `","base_version":14}`,
			&boardcontent.CardMove{CardID: cardA, ToColumnID: colB, AfterID: &cardB, BaseVersion: &version},
		},
		{
			"card archive", boardcontent.OpCardArchive,
			`{"card_id":"` + cardA.String() + `"}`,
			&boardcontent.CardArchive{CardID: cardA},
		},
		{
			"column create", boardcontent.OpColumnCreate,
			`{"column_id":"` + colA.String() + `","title":"T","before_id":"` + colB.String() + `"}`,
			&boardcontent.ColumnCreate{ColumnID: colA, Title: "T", BeforeID: &colB},
		},
		{
			"column rename", boardcontent.OpColumnRename,
			`{"column_id":"` + colA.String() + `","title":"T"}`,
			&boardcontent.ColumnRename{ColumnID: colA, Title: "T"},
		},
		{
			"column move", boardcontent.OpColumnMove,
			`{"column_id":"` + colA.String() + `","after_id":"` + colB.String() + `","base_version":14}`,
			&boardcontent.ColumnMove{ColumnID: colA, AfterID: &colB, BaseVersion: &version},
		},
		{
			"column archive", boardcontent.OpColumnArchive,
			`{"column_id":"` + colA.String() + `"}`,
			&boardcontent.ColumnArchive{ColumnID: colA},
		},
		{
			"whitespace around object", boardcontent.OpColumnArchive,
			"  \n{\"column_id\":\"" + colA.String() + "\"}\n  ",
			&boardcontent.ColumnArchive{ColumnID: colA},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := boardcontent.DecodePayload(boardcontent.Op{Type: tt.opType, OpID: opID, Payload: json.RawMessage(tt.payload)})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDecodePayloadIsStrict(t *testing.T) {
	t.Parallel()
	card, col := cardA.String(), colA.String()
	tests := []struct {
		name        string
		opType      string
		payload     string
		wantMessage string
	}{
		{"unknown field on card create", boardcontent.OpCardCreate, `{"card_id":"` + card + `","column_id":"` + col + `","title":"T","extra":1}`, "unknown field"},
		{"unknown field on card edit", boardcontent.OpCardEdit, `{"card_id":"` + card + `","title":"T","assignee":"x"}`, "unknown field"},
		{"unknown field on card move", boardcontent.OpCardMove, `{"card_id":"` + card + `","to_column_id":"` + col + `","base_version":1,"rank":"a0"}`, "unknown field"},
		{"unknown field on card archive", boardcontent.OpCardArchive, `{"card_id":"` + card + `","force":true}`, "unknown field"},
		{"unknown field on column create", boardcontent.OpColumnCreate, `{"column_id":"` + col + `","title":"T","rank":"a0"}`, "unknown field"},
		{"unknown field on column rename", boardcontent.OpColumnRename, `{"column_id":"` + col + `","title":"T","x":1}`, "unknown field"},
		{"unknown field on column move", boardcontent.OpColumnMove, `{"column_id":"` + col + `","base_version":1,"x":1}`, "unknown field"},
		{"unknown field on column archive", boardcontent.OpColumnArchive, `{"column_id":"` + col + `","x":1}`, "unknown field"},
		{"wrong type for title", boardcontent.OpCardCreate, `{"card_id":"` + card + `","column_id":"` + col + `","title":5}`, "title"},
		{"wrong type for base_version", boardcontent.OpCardMove, `{"card_id":"` + card + `","to_column_id":"` + col + `","base_version":"1"}`, "base_version"},
		{"fractional base_version", boardcontent.OpCardMove, `{"card_id":"` + card + `","to_column_id":"` + col + `","base_version":1.5}`, "base_version"},
		{"invalid uuid", boardcontent.OpCardArchive, `{"card_id":"nope"}`, "is not a valid JSON object"},
		{"payload is an array", boardcontent.OpCardArchive, `[]`, "is not a valid JSON object"},
		{"payload is a string", boardcontent.OpCardArchive, `"x"`, "is not a valid JSON object"},
		{"payload is a number", boardcontent.OpCardArchive, `7`, "is not a valid JSON object"},
		{"truncated payload", boardcontent.OpCardArchive, `{"card_id":`, "is not a valid JSON object"},
		{"two objects", boardcontent.OpCardArchive, `{"card_id":"` + card + `"}{"card_id":"` + card + `"}`, "single JSON object"},
		{"trailing garbage", boardcontent.OpCardArchive, `{"card_id":"` + card + `"} x`, "single JSON object"},
		{"empty payload", boardcontent.OpCardArchive, ``, "is required"},
		{"blank payload", boardcontent.OpCardArchive, "  \n\t", "is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := boardcontent.DecodePayload(boardcontent.Op{Type: tt.opType, OpID: opID, Payload: json.RawMessage(tt.payload)})
			require.Error(t, err)
			assert.Nil(t, got)
			var vErr *domain.ValidationError
			require.True(t, errors.As(err, &vErr), "expected a validation error, got %T: %v", err, err)
			assert.Equal(t, "payload", vErr.Field)
			assert.Contains(t, vErr.Message, tt.wantMessage)
		})
	}
}

func TestDecodePayloadDoesNotEchoPayloadValues(t *testing.T) {
	t.Parallel()
	secret := "very-secret-token-value"
	_, err := boardcontent.DecodePayload(boardcontent.Op{Type: boardcontent.OpCardArchive, Payload: json.RawMessage(`{"card_id":"` + cardA.String() + `","` + secret + `":1}`)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), secret, "unknown field names are reported so clients can fix typos")

	_, err = boardcontent.DecodePayload(boardcontent.Op{Type: boardcontent.OpCardCreate, Payload: json.RawMessage(`{"title":"` + secret + `","card_id":5}`)})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

func TestDecodePayloadAcceptsBaseDescriptionVersionAloneAtTheWireLevel(t *testing.T) {
	t.Parallel()
	got, err := boardcontent.DecodePayload(boardcontent.Op{
		Type:    boardcontent.OpCardEdit,
		Payload: json.RawMessage(`{"card_id":"` + cardA.String() + `","base_description_version":3}`),
	})
	require.NoError(t, err)
	edit, ok := got.(*boardcontent.CardEdit)
	require.True(t, ok)
	require.NotNil(t, edit.BaseDescriptionVersion)
	assert.Equal(t, int64(3), *edit.BaseDescriptionVersion)
	assert.Nil(t, edit.Description)
}

func TestDecodePayloadNullPayload(t *testing.T) {
	t.Parallel()
	got, err := boardcontent.DecodePayload(boardcontent.Op{Type: boardcontent.OpCardArchive, Payload: json.RawMessage(`null`)})
	require.NoError(t, err)
	archive, ok := got.(*boardcontent.CardArchive)
	require.True(t, ok)
	assert.Equal(t, uuid.Nil, archive.CardID, "required ids are enforced by the processor, not by the decoder")
}

func TestDecodePayloadUnknownType(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"", "op.nope", "card.created"} {
		got, err := boardcontent.DecodePayload(boardcontent.Op{Type: typ, Payload: json.RawMessage(`{}`)})
		assert.ErrorIs(t, err, boardcontent.ErrUnknownType, typ)
		assert.Nil(t, got)
	}
}

func TestNewOpRoundTripsThroughParseOp(t *testing.T) {
	t.Parallel()
	title := "Hello"
	op, err := boardcontent.NewOp(boardcontent.OpCardEdit, opID, boardcontent.CardEdit{CardID: cardA, Title: &title})
	require.NoError(t, err)
	assert.Equal(t, boardcontent.OpCardEdit, op.Type)
	assert.Equal(t, opID, op.OpID)

	wire, err := json.Marshal(protocol.Envelope{V: protocol.Version, Type: op.Type, OpID: &op.OpID, Payload: op.Payload})
	require.NoError(t, err)
	parsed, err := protocol.ParseOp(wire)
	require.NoError(t, err)
	decoded, err := boardcontent.DecodePayload(parsed)
	require.NoError(t, err)
	assert.Equal(t, &boardcontent.CardEdit{CardID: cardA, Title: &title}, decoded)

	_, err = boardcontent.NewOp(boardcontent.OpCardEdit, opID, make(chan int))
	assert.Error(t, err)
}

func marshalMap(t *testing.T, v any) map[string]any {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func TestEventEnvelopeShape(t *testing.T) {
	t.Parallel()
	actor := uuid.New()
	created := time.UnixMilli(1710000000123).UTC()
	payload := boardcontent.CardMoved{CardID: cardA, FromColumnID: colA, ToColumnID: colB, Rank: "a0V", Version: 15, ActorID: actor}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)

	env := protocol.EventEnvelope(domain.Event{
		BoardID: uuid.New(), Seq: 129, Type: boardcontent.EventCardMoved, Payload: raw, OpID: &opID, ActorID: &actor, CreatedAt: created,
	})
	m := marshalMap(t, env)
	assert.Equal(t, float64(1), m["v"])
	assert.Equal(t, "card.moved", m["type"])
	assert.Equal(t, opID.String(), m["op_id"])
	assert.Equal(t, float64(129), m["seq"])
	assert.Equal(t, float64(1710000000123), m["server_ts"])
	assert.NotContains(t, m, "board_id")
	assert.NotContains(t, m, "actor_id")
	assert.Equal(t, map[string]any{
		"card_id": cardA.String(), "from_column_id": colA.String(), "to_column_id": colB.String(),
		"rank": "a0V", "version": float64(15), "actor_id": actor.String(),
	}, m["payload"])
}

func TestEventEnvelopeOmitsOpIDForSystemEvents(t *testing.T) {
	t.Parallel()
	env := protocol.EventEnvelope(domain.Event{
		Seq: 5, Type: boardcontent.EventColumnRebalanced, Payload: json.RawMessage(`{"column_id":"x","cards":[]}`), CreatedAt: time.UnixMilli(1),
	})
	m := marshalMap(t, env)
	assert.NotContains(t, m, "op_id")
	assert.Equal(t, float64(5), m["seq"])
	assert.Equal(t, "column.rebalanced", m["type"])
}

func TestNackEnvelopeShape(t *testing.T) {
	t.Parallel()
	env, err := protocol.NackEnvelope(opID, boardcontent.Nack{Reason: boardcontent.ReasonVersionConflict, AtSeq: 131, Message: "card was changed by another user"})
	require.NoError(t, err)
	data, err := json.Marshal(env)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"v": 1,
		"type": "nack",
		"op_id": "`+opID.String()+`",
		"payload": {"reason": "version_conflict", "at_seq": 131, "message": "card was changed by another user"}
	}`, string(data))
	assert.NotContains(t, string(data), `"seq"`)
	assert.NotContains(t, string(data), "server_ts")
}

func TestNackEnvelopeDoesNotAliasOpID(t *testing.T) {
	t.Parallel()
	id := opID
	env, err := protocol.NackEnvelope(id, boardcontent.Nack{Reason: boardcontent.ReasonInternal})
	require.NoError(t, err)
	id = uuid.New()
	require.NotNil(t, env.OpID)
	assert.Equal(t, opID, *env.OpID)
	assert.NotEqual(t, id, *env.OpID)
}

func TestNackReasonsMatchSpec(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "version_conflict", boardcontent.ReasonVersionConflict)
	assert.Equal(t, "not_found", boardcontent.ReasonNotFound)
	assert.Equal(t, "forbidden", boardcontent.ReasonForbidden)
	assert.Equal(t, "validation_error", boardcontent.ReasonValidation)
	assert.Equal(t, "rate_limited", boardcontent.ReasonRateLimited)
	assert.Equal(t, "internal", boardcontent.ReasonInternal)
	assert.Equal(t, "protocol_error", protocol.CodeProtocolError)
}

func TestEventTypeNamesMatchSpec(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		boardcontent.EventCardCreated:       "card.created",
		boardcontent.EventCardEdited:        "card.edited",
		boardcontent.EventCardMoved:         "card.moved",
		boardcontent.EventCardArchived:      "card.archived",
		boardcontent.EventColumnCreated:     "column.created",
		boardcontent.EventColumnRenamed:     "column.renamed",
		boardcontent.EventColumnMoved:       "column.moved",
		boardcontent.EventColumnArchived:    "column.archived",
		boardcontent.EventColumnRebalanced:  "column.rebalanced",
		boardcontent.EventColumnsRebalanced: "columns.rebalanced",
		boardcontent.EventBoardRenamed:      "board.renamed",
		boardcontent.EventBoardDeleted:      "board.deleted",
		boardcontent.OpCardCreate:           "op.card.create",
		boardcontent.OpCardEdit:             "op.card.edit",
		boardcontent.OpCardMove:             "op.card.move",
		boardcontent.OpCardArchive:          "op.card.archive",
		boardcontent.OpColumnCreate:         "op.column.create",
		boardcontent.OpColumnRename:         "op.column.rename",
		boardcontent.OpColumnMove:           "op.column.move",
		boardcontent.OpColumnArchive:        "op.column.archive",
	}
	for got, spec := range want {
		assert.Equal(t, spec, got)
	}
	assert.Equal(t, 1, protocol.Version)
}

func TestEventPayloadShapes(t *testing.T) {
	t.Parallel()
	actor := uuid.New()
	title := "New"
	desc := "Text"
	now := time.Date(2030, 5, 6, 7, 8, 9, 0, time.UTC)
	tests := []struct {
		name  string
		value any
		want  map[string]any
	}{
		{
			"card.created", boardcontent.CardCreated{
				Card:    boardcontent.Card{ID: cardA, ColumnID: colA, Title: "T", Description: "", Rank: "a0", Version: 1, DescriptionVersion: 1, CreatedBy: actor, UpdatedBy: actor, CreatedAt: now, UpdatedAt: now},
				ActorID: actor,
			},
			map[string]any{
				"id": cardA.String(), "column_id": colA.String(), "title": "T", "description": "", "rank": "a0",
				"version": float64(1), "description_version": float64(1), "created_by": actor.String(), "updated_by": actor.String(),
				"created_at": "2030-05-06T07:08:09Z", "updated_at": "2030-05-06T07:08:09Z", "actor_id": actor.String(),
			},
		},
		{
			"card.edited title only", boardcontent.CardEdited{CardID: cardA, Title: &title, Version: 2, DescriptionVersion: 1, ActorID: actor},
			map[string]any{"card_id": cardA.String(), "title": "New", "version": float64(2), "description_version": float64(1), "actor_id": actor.String()},
		},
		{
			"card.edited description only", boardcontent.CardEdited{CardID: cardA, Description: &desc, Version: 3, DescriptionVersion: 2, ActorID: actor},
			map[string]any{"card_id": cardA.String(), "description": "Text", "version": float64(3), "description_version": float64(2), "actor_id": actor.String()},
		},
		{
			"card.archived", boardcontent.CardArchived{CardID: cardA, ColumnID: colA, Version: 4, ActorID: actor},
			map[string]any{"card_id": cardA.String(), "column_id": colA.String(), "version": float64(4), "actor_id": actor.String()},
		},
		{
			"column.created", boardcontent.ColumnCreated{Column: boardcontent.Column{ID: colA, Title: "Todo", Rank: "a0", Version: 1}, ActorID: actor},
			map[string]any{"id": colA.String(), "title": "Todo", "rank": "a0", "version": float64(1), "actor_id": actor.String()},
		},
		{
			"column.renamed", boardcontent.ColumnRenamed{ColumnID: colA, Title: "Doing", Version: 2, ActorID: actor},
			map[string]any{"column_id": colA.String(), "title": "Doing", "version": float64(2), "actor_id": actor.String()},
		},
		{
			"column.moved", boardcontent.ColumnMoved{ColumnID: colA, Rank: "a1", Version: 3, ActorID: actor},
			map[string]any{"column_id": colA.String(), "rank": "a1", "version": float64(3), "actor_id": actor.String()},
		},
		{
			"column.archived", boardcontent.ColumnArchived{ColumnID: colA, CardIDs: []uuid.UUID{cardA, cardB}, Version: 5, ActorID: actor},
			map[string]any{"column_id": colA.String(), "card_ids": []any{cardA.String(), cardB.String()}, "version": float64(5), "actor_id": actor.String()},
		},
		{
			"column.rebalanced", boardcontent.ColumnRebalanced{ColumnID: colA, Cards: []boardcontent.CardRank{{CardID: cardA, Rank: "a0"}, {CardID: cardB, Rank: "a1"}}, ActorID: actor},
			map[string]any{
				"column_id": colA.String(),
				"cards":     []any{map[string]any{"card_id": cardA.String(), "rank": "a0"}, map[string]any{"card_id": cardB.String(), "rank": "a1"}},
				"actor_id":  actor.String(),
			},
		},
		{
			"columns.rebalanced", boardcontent.ColumnsRebalanced{Columns: []boardcontent.ColumnRank{{ColumnID: colA, Rank: "a0"}}, ActorID: actor},
			map[string]any{"columns": []any{map[string]any{"column_id": colA.String(), "rank": "a0"}}, "actor_id": actor.String()},
		},
		{
			"board.renamed", boardcontent.BoardRenamed{Title: "Roadmap", ActorID: actor},
			map[string]any{"title": "Roadmap", "actor_id": actor.String()},
		},
		{
			"board.deleted", boardcontent.BoardDeleted{ActorID: actor},
			map[string]any{"actor_id": actor.String()},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, marshalMap(t, tt.value))
		})
	}
}

func TestFromCardAndColumnConvertToUTC(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("x", 5*3600)
	at := time.Date(2030, 1, 1, 5, 0, 0, 0, loc)
	card := boardcontent.FromCard(domain.Card{ID: cardA, ColumnID: colA, Title: "T", Description: "D", Rank: "a0", Version: 2, DescriptionVersion: 3, CreatedAt: at, UpdatedAt: at})
	assert.Equal(t, time.UTC, card.CreatedAt.Location())
	assert.True(t, card.CreatedAt.Equal(at))
	assert.Equal(t, int64(3), card.DescriptionVersion)

	col := boardcontent.FromColumn(domain.Column{ID: colA, BoardID: uuid.New(), Title: "C", Rank: "a1", Version: 4})
	assert.Equal(t, boardcontent.Column{ID: colA, Title: "C", Rank: "a1", Version: 4}, col)
}

func TestFromSnapshotShape(t *testing.T) {
	t.Parallel()
	owner := uuid.New()
	at := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	snap := protocol.FromSnapshot(domain.BoardSnapshot{
		Board: domain.BoardWithRole{
			Board: domain.Board{ID: uuid.New(), Title: "B", OwnerID: owner, Seq: 9, CreatedAt: at, UpdatedAt: at},
			Role:  domain.RoleEditor,
		},
		Columns: []domain.Column{{ID: colA, Title: "C", Rank: "a0", Version: 1}},
		Cards:   []domain.Card{{ID: cardA, ColumnID: colA, Title: "T", Rank: "a0", Version: 1, DescriptionVersion: 1, CreatedAt: at, UpdatedAt: at}},
		Members: []domain.Member{{UserID: owner, DisplayName: "Ann", AvatarColor: "#112233", Role: domain.RoleOwner, JoinedAt: at}},
		Seq:     9,
	})
	m := marshalMap(t, snap)
	assert.ElementsMatch(t, []string{"board", "columns", "cards", "members", "seq"}, keys(m))
	assert.Equal(t, float64(9), m["seq"])
	board := m["board"].(map[string]any)
	assert.ElementsMatch(t, []string{"id", "title", "owner_id", "role", "created_at", "updated_at"}, keys(board))
	assert.Equal(t, "editor", board["role"])
	member := m["members"].([]any)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"user_id", "display_name", "avatar_color", "role", "joined_at"}, keys(member))
	column := m["columns"].([]any)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"id", "title", "rank", "version"}, keys(column))
	card := m["cards"].([]any)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"id", "column_id", "title", "description", "rank", "version", "description_version", "created_by", "updated_by", "created_at", "updated_at"}, keys(card))
}

func TestFromSnapshotEmptyCollectionsAreArrays(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(protocol.FromSnapshot(domain.BoardSnapshot{}))
	require.NoError(t, err)
	s := string(data)
	assert.Contains(t, s, `"columns":[]`)
	assert.Contains(t, s, `"cards":[]`)
	assert.Contains(t, s, `"members":[]`)
	assert.False(t, strings.Contains(s, "null"), s)
}

func TestFromEventPage(t *testing.T) {
	t.Parallel()
	empty, err := json.Marshal(protocol.FromEventPage(domain.EventPage{Seq: 4}))
	require.NoError(t, err)
	assert.JSONEq(t, `{"events":[],"seq":4,"has_more":false}`, string(empty))

	page := protocol.FromEventPage(domain.EventPage{
		Seq:     10,
		HasMore: true,
		Events: []domain.Event{
			{Seq: 8, Type: boardcontent.EventCardCreated, Payload: json.RawMessage(`{"a":1}`), OpID: &opID, CreatedAt: time.UnixMilli(1000)},
			{Seq: 9, Type: boardcontent.EventColumnRebalanced, Payload: json.RawMessage(`{"b":2}`), CreatedAt: time.UnixMilli(2000)},
		},
	})
	m := marshalMap(t, page)
	assert.Equal(t, true, m["has_more"])
	assert.Equal(t, float64(10), m["seq"])
	events := m["events"].([]any)
	require.Len(t, events, 2)
	first := events[0].(map[string]any)
	assert.Equal(t, float64(1), first["v"])
	assert.Equal(t, float64(8), first["seq"])
	assert.Equal(t, float64(1000), first["server_ts"])
	assert.Equal(t, opID.String(), first["op_id"])
	assert.NotContains(t, events[1].(map[string]any), "op_id")
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
