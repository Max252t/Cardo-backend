package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

func TestSnapshotErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		{"not a member or deleted", domain.ErrNotFound, http.StatusNotFound, httpapi.CodeNotFound, false},
		{"busy", domain.ErrBusy, http.StatusServiceUnavailable, httpapi.CodeBusy, true},
		{"overloaded", domain.ErrOverloaded, http.StatusServiceUnavailable, httpapi.CodeOverloaded, true},
		{"unexpected", errors.New("db is down"), http.StatusInternalServerError, httpapi.CodeInternal, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.boards.snapshot = func(context.Context, uuid.UUID, uuid.UUID) (domain.BoardSnapshot, error) {
				return domain.BoardSnapshot{}, tt.err
			}
			rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID, token: "good"})
			assert.Equal(t, tt.status, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.code, code)
			assert.Equal(t, tt.retryAfter, rec.Header().Get("Retry-After") != "")
			assert.NotContains(t, rec.Body.String(), "db is down")
		})
	}
}

func TestSnapshotRejectsInvalidBoardID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"abc", "123", "22222222-2222-7222-8222-22222222222"} {
		e := newEnv(t)
		called := false
		e.boards.snapshot = func(context.Context, uuid.UUID, uuid.UUID) (domain.BoardSnapshot, error) {
			called = true
			return domain.BoardSnapshot{}, nil
		}
		rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + id, token: "good"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, id)
		code, msg := rec.errorBody(t)
		assert.Equal(t, httpapi.CodeValidation, code)
		assert.Contains(t, msg, "id")
		assert.False(t, called)
	}
}

func TestSnapshotOfAnEmptyBoardUsesEmptyArrays(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.boards.snapshot = func(context.Context, uuid.UUID, uuid.UUID) (domain.BoardSnapshot, error) {
		return domain.BoardSnapshot{Board: sampleBoard("Empty", domain.RoleOwner)}, nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID, token: "good"})
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `"columns":[]`)
	assert.Contains(t, body, `"cards":[]`)
	assert.Contains(t, body, `"members":[]`)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
}

func TestSnapshotJSONShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	colID, cardID := uuid.New(), uuid.New()
	at := time.Date(2030, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3*3600))
	e.boards.snapshot = func(context.Context, uuid.UUID, uuid.UUID) (domain.BoardSnapshot, error) {
		return domain.BoardSnapshot{
			Board:   sampleBoard("Shape", domain.RoleEditor),
			Columns: []domain.Column{{ID: colID, BoardID: uuid.New(), Title: "Todo", Rank: "a0", Version: 2}},
			Cards: []domain.Card{{
				ID: cardID, BoardID: uuid.New(), ColumnID: colID, Title: "Card", Description: "text", Rank: "a1", Version: 3, DescriptionVersion: 2,
				CreatedBy: testUserID, UpdatedBy: testUserID, CreatedAt: at, UpdatedAt: at,
			}},
			Members: []domain.Member{{UserID: testUserID, DisplayName: "Ann", AvatarColor: "#112233", Role: domain.RoleEditor, JoinedAt: at}},
			Seq:     12,
		}, nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID, token: "good"})
	require.Equal(t, http.StatusOK, rec.Code)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.ElementsMatch(t, []string{"board", "columns", "cards", "members", "seq"}, mapKeys(raw))
	card := raw["cards"].([]any)[0].(map[string]any)
	assert.Equal(t, "2030-01-02T00:04:05Z", card["created_at"], "timestamps are rendered in UTC")
	assert.NotContains(t, card, "board_id", "internal columns are not exposed")
	assert.NotContains(t, card, "archived_at")
	assert.EqualValues(t, 3, card["version"])
	assert.EqualValues(t, 2, card["description_version"])
	column := raw["columns"].([]any)[0].(map[string]any)
	assert.ElementsMatch(t, []string{"id", "title", "rank", "version"}, mapKeys(column))
	member := raw["members"].([]any)[0].(map[string]any)
	assert.NotContains(t, member, "email", "member emails are not part of the snapshot")
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBoardEventsQueryParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		query     string
		wantAfter int64
		wantOK    bool
	}{
		{"zero", "?after_seq=0", 0, true},
		{"positive", "?after_seq=12", 12, true},
		{"leading zeros", "?after_seq=007", 7, true},
		{"max int64", "?after_seq=9223372036854775807", 9223372036854775807, true},
		{"first of duplicates", "?after_seq=3&after_seq=4", 3, true},
		{"extra params ignored", "?limit=5&after_seq=2&foo=bar", 2, true},
		{"encoded plus", "?after_seq=%2B5", 5, true},
		{"missing", "", 0, false},
		{"empty", "?after_seq=", 0, false},
		{"negative", "?after_seq=-1", 0, false},
		{"minus zero", "?after_seq=-0", 0, true},
		{"overflow", "?after_seq=9223372036854775808", 0, false},
		{"decimal", "?after_seq=1.0", 0, false},
		{"exponent", "?after_seq=1e3", 0, false},
		{"hex", "?after_seq=0x10", 0, false},
		{"space", "?after_seq=%201", 0, false},
		{"letters", "?after_seq=abc", 0, false},
		{"wrong param", "?seq=1", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			var got int64 = -1
			e.boards.events = func(_ context.Context, _, _ uuid.UUID, after int64) (domain.EventPage, error) {
				got = after
				return domain.EventPage{Seq: after}, nil
			}
			rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID + "/events" + tt.query, token: "good"})
			if !tt.wantOK {
				assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				code, msg := rec.errorBody(t)
				assert.Equal(t, httpapi.CodeValidation, code)
				assert.Contains(t, msg, "after_seq")
				assert.EqualValues(t, -1, got, "the service must not be called with a bad cursor")
				return
			}
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantAfter, got)
		})
	}
}

func TestBoardEventsResponseShape(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	opID := uuid.New()
	e.boards.events = func(context.Context, uuid.UUID, uuid.UUID, int64) (domain.EventPage, error) {
		return domain.EventPage{
			Seq:     20,
			HasMore: true,
			Events: []domain.Event{
				{Seq: 18, Type: boardcontent.EventCardMoved, OpID: &opID, Payload: json.RawMessage(`{"card_id":"c"}`), CreatedAt: time.UnixMilli(1710000000123)},
				{Seq: 19, Type: boardcontent.EventColumnRebalanced, Payload: json.RawMessage(`{"column_id":"x","cards":[]}`), CreatedAt: time.UnixMilli(1710000000456)},
			},
		}, nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID + "/events?after_seq=17", token: "good"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{
		"seq": 20,
		"has_more": true,
		"events": [
			{"v":1,"type":"card.moved","op_id":"`+opID.String()+`","seq":18,"server_ts":1710000000123,"payload":{"card_id":"c"}},
			{"v":1,"type":"column.rebalanced","seq":19,"server_ts":1710000000456,"payload":{"column_id":"x","cards":[]}}
		]
	}`, rec.Body.String())
}

func TestBoardEventsEmptyPageIsAnEmptyArray(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.boards.events = func(context.Context, uuid.UUID, uuid.UUID, int64) (domain.EventPage, error) {
		return domain.EventPage{Seq: 5}, nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID + "/events?after_seq=5", token: "good"})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"events":[],"seq":5,"has_more":false}`, rec.Body.String())
}

func TestBoardEventsErrorMapping(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		{"gone", domain.ErrGone, http.StatusGone, httpapi.CodeGone, false},
		{"not found", domain.ErrNotFound, http.StatusNotFound, httpapi.CodeNotFound, false},
		{"cursor ahead of board", domain.NewValidationError("after_seq", "is greater than the current board seq"), http.StatusBadRequest, httpapi.CodeValidation, false},
		{"busy", domain.ErrBusy, http.StatusServiceUnavailable, httpapi.CodeBusy, true},
		{"unexpected", errors.New("boom"), http.StatusInternalServerError, httpapi.CodeInternal, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.boards.events = func(context.Context, uuid.UUID, uuid.UUID, int64) (domain.EventPage, error) {
				return domain.EventPage{}, tt.err
			}
			rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID + "/events?after_seq=1", token: "good"})
			assert.Equal(t, tt.status, rec.Code)
			code, msg := rec.errorBody(t)
			assert.Equal(t, tt.code, code)
			assert.Equal(t, tt.retryAfter, rec.Header().Get("Retry-After") != "")
			if tt.status == http.StatusGone {
				assert.Contains(t, strings.ToLower(msg), "snapshot")
			}
		})
	}
}

func TestBoardEventsPassesUserAndBoard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.boards.events = func(_ context.Context, userID, boardID uuid.UUID, after int64) (domain.EventPage, error) {
		assert.Equal(t, testUserID, userID)
		assert.Equal(t, sampleBoardID, boardID.String())
		assert.EqualValues(t, 4, after)
		return domain.EventPage{}, nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/boards/" + sampleBoardID + "/events?after_seq=4", token: "good"})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestBoardReadRoutesRejectOtherMethods(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := e.do(t, request{method: method, path: "/api/boards/" + sampleBoardID + "/events?after_seq=0", token: "good"})
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		code, _ := rec.errorBody(t)
		assert.Equal(t, httpapi.CodeMethodNotAllowed, code)
	}
}
