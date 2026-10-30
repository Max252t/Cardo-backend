package httpapi_test

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
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func TestGetBoardSnapshot(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	colID, cardID := uuid.New(), uuid.New()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	e.boards.snapshot = func(_ context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error) {
		assert.Equal(t, testUserID, userID)
		assert.Equal(t, sampleBoardID, boardID.String())
		return domain.BoardSnapshot{
			Board:   sampleBoard("Snap", domain.RoleViewer),
			Columns: []domain.Column{{ID: colID, Title: "Todo", Rank: "a0", Version: 2}},
			Cards:   []domain.Card{{ID: cardID, ColumnID: colID, Title: "Card", Rank: "a0", Version: 1, DescriptionVersion: 1, CreatedAt: now, UpdatedAt: now}},
			Members: []domain.Member{{UserID: testUserID, DisplayName: "Ann", AvatarColor: "#112233", Role: domain.RoleViewer, JoinedAt: now}},
			Seq:     7,
		}, nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID, token: "good"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snap protocol.Snapshot
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	assert.Equal(t, int64(7), snap.Seq)
	assert.Equal(t, domain.RoleViewer, snap.Board.Role)
	require.Len(t, snap.Columns, 1)
	assert.Equal(t, colID, snap.Columns[0].ID)
	require.Len(t, snap.Cards, 1)
	assert.Equal(t, colID, snap.Cards[0].ColumnID)
	require.Len(t, snap.Members, 1)
	assert.Equal(t, "Ann", snap.Members[0].DisplayName)
}

func TestBoardEvents(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opID := uuid.New()
	e.boards.events = func(_ context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error) {
		assert.Equal(t, int64(5), afterSeq)
		return domain.EventPage{
			Events: []domain.Event{{Seq: 6, Type: boardcontent.EventCardMoved, OpID: &opID, Payload: json.RawMessage(`{"card_id":"x"}`), CreatedAt: time.UnixMilli(1710000000000)}},
			Seq:    6,
		}, nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID + "/events?after_seq=5", token: "good"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page protocol.EventsPage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	require.Len(t, page.Events, 1)
	ev := page.Events[0]
	assert.Equal(t, protocol.Version, ev.V)
	assert.Equal(t, int64(6), ev.Seq)
	assert.Equal(t, int64(1710000000000), ev.ServerTS)
	assert.Equal(t, opID, *ev.OpID)
	assert.JSONEq(t, `{"card_id":"x"}`, string(ev.Payload))
	assert.False(t, page.HasMore)
}

func TestBoardEventsErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		query  string
		err    error
		status int
		code   string
	}{
		{"missing after_seq", "", nil, http.StatusBadRequest, httpapi.CodeValidation},
		{"negative after_seq", "?after_seq=-1", nil, http.StatusBadRequest, httpapi.CodeValidation},
		{"non numeric after_seq", "?after_seq=abc", nil, http.StatusBadRequest, httpapi.CodeValidation},
		{"ahead of board", "?after_seq=99", domain.NewValidationError("after_seq", "is greater than the current board seq"), http.StatusBadRequest, httpapi.CodeValidation},
		{"outside retention", "?after_seq=1", domain.ErrGone, http.StatusGone, httpapi.CodeGone},
		{"not a member", "?after_seq=1", domain.ErrNotFound, http.StatusNotFound, httpapi.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			called := false
			e.boards.events = func(context.Context, uuid.UUID, uuid.UUID, int64) (domain.EventPage, error) {
				called = true
				return domain.EventPage{}, tt.err
			}
			rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID + "/events" + tt.query, token: "good"})
			assert.Equal(t, tt.status, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.code, code)
			assert.Equal(t, tt.err != nil, called)
		})
	}
}

func TestBoardReadsRequireAuth(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, path := range []string{"/api/boards/" + sampleBoardID, "/api/boards/" + sampleBoardID + "/events?after_seq=0"} {
		rec := e.do(t, request{method: http.MethodGet, path: path})
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	}
	e.boards.snapshot = func(context.Context, uuid.UUID, uuid.UUID) (domain.BoardSnapshot, error) {
		return domain.BoardSnapshot{}, domain.ErrNotFound
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID, token: "good"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
