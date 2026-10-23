package boardcontent_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func TestPersistedEventPayloadShapesAreStable(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	other := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	actor := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	title := "T"
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name string
		v    any
		want string
	}{
		{"card.moved", boardcontent.CardMoved{CardID: id, FromColumnID: other, ToColumnID: id, Rank: "a0", Version: 3, ActorID: actor},
			`{"card_id":"11111111-1111-1111-1111-111111111111","from_column_id":"22222222-2222-2222-2222-222222222222","to_column_id":"11111111-1111-1111-1111-111111111111","rank":"a0","version":3,"actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"card.edited", boardcontent.CardEdited{CardID: id, Title: &title, Version: 2, DescriptionVersion: 1, ActorID: actor},
			`{"card_id":"11111111-1111-1111-1111-111111111111","title":"T","version":2,"description_version":1,"actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"card.archived", boardcontent.CardArchived{CardID: id, ColumnID: other, Version: 4, ActorID: actor},
			`{"card_id":"11111111-1111-1111-1111-111111111111","column_id":"22222222-2222-2222-2222-222222222222","version":4,"actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"column.created", boardcontent.ColumnCreated{Column: boardcontent.Column{ID: id, Title: "Todo", Rank: "a0", Version: 1}, ActorID: actor},
			`{"id":"11111111-1111-1111-1111-111111111111","title":"Todo","rank":"a0","version":1,"actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"column.archived", boardcontent.ColumnArchived{ColumnID: id, CardIDs: []uuid.UUID{other}, Version: 2, ActorID: actor},
			`{"column_id":"11111111-1111-1111-1111-111111111111","card_ids":["22222222-2222-2222-2222-222222222222"],"version":2,"actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"column.rebalanced", boardcontent.ColumnRebalanced{ColumnID: id, Cards: []boardcontent.CardRank{{CardID: other, Rank: "b"}}, ActorID: actor},
			`{"column_id":"11111111-1111-1111-1111-111111111111","cards":[{"card_id":"22222222-2222-2222-2222-222222222222","rank":"b"}],"actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"board.renamed", boardcontent.BoardRenamed{Title: "B", ActorID: actor},
			`{"title":"B","actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"board.deleted", boardcontent.BoardDeleted{ActorID: actor},
			`{"actor_id":"33333333-3333-3333-3333-333333333333"}`},
		{"nack", boardcontent.Nack{Reason: boardcontent.ReasonVersionConflict, AtSeq: 9, Message: "m"},
			`{"reason":"version_conflict","at_seq":9,"message":"m"}`},
		{"card", boardcontent.FromCard(domain.Card{ID: id, ColumnID: other, Title: "c", Description: "d", Rank: "a0", Version: 1, DescriptionVersion: 1, CreatedBy: id, UpdatedBy: id, CreatedAt: at, UpdatedAt: at}),
			`{"id":"11111111-1111-1111-1111-111111111111","column_id":"22222222-2222-2222-2222-222222222222","title":"c","description":"d","rank":"a0","version":1,"description_version":1,"created_by":"11111111-1111-1111-1111-111111111111","updated_by":"11111111-1111-1111-1111-111111111111","created_at":"2026-01-02T03:04:05Z","updated_at":"2026-01-02T03:04:05Z"}`},
	}
	for _, tt := range tests {
		assert.JSONEq(t, tt.want, mustJSON(t, tt.v), tt.name)
		assert.Equal(t, tt.want, mustJSON(t, tt.v), tt.name+" byte order")
	}
}

func TestOpPayloadShapesDecodeFromTheDocumentedWireFormat(t *testing.T) {
	t.Parallel()
	op := boardcontent.Op{
		Type:    boardcontent.OpCardMove,
		OpID:    uuid.New(),
		Payload: json.RawMessage(`{"card_id":"11111111-1111-1111-1111-111111111111","to_column_id":"22222222-2222-2222-2222-222222222222","after_id":"33333333-3333-3333-3333-333333333333","base_version":14}`),
	}
	decoded, err := boardcontent.DecodePayload(op)
	require.NoError(t, err)
	move, ok := decoded.(*boardcontent.CardMove)
	require.True(t, ok)
	require.NotNil(t, move.BaseVersion)
	assert.Equal(t, int64(14), *move.BaseVersion)
	assert.Nil(t, move.BeforeID)
}
