package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/rank"
)

type boardFlow struct {
	*flow
	proc    *ops.Processor
	owner   session
	boardID uuid.UUID
}

func newBoardFlowWithBoard(t *testing.T) *boardFlow {
	t.Helper()
	f := newBoardFlow(t)
	owner := f.register(t, "owner")
	board := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Ops"}`, owner.access, http.StatusCreated)
	return &boardFlow{
		flow:    f,
		proc:    ops.NewProcessor(f.store),
		owner:   owner,
		boardID: uuid.MustParse(board["id"].(string)),
	}
}

func (b *boardFlow) apply(t *testing.T, user session, opType string, payload any) boardcontent.OpOutcome {
	t.Helper()
	op, err := boardcontent.NewOp(opType, uuid.New(), payload)
	require.NoError(t, err)
	out, err := b.proc.Apply(context.Background(), uuid.MustParse(user.userID), b.boardID, op)
	require.NoError(t, err)
	return out
}

func (b *boardFlow) mustApply(t *testing.T, opType string, payload any) boardcontent.OpOutcome {
	t.Helper()
	out := b.apply(t, b.owner, opType, payload)
	require.Nil(t, out.Nack, "unexpected nack %+v", out.Nack)
	return out
}

func (b *boardFlow) path(suffix string) string {
	return "/api/boards/" + b.boardID.String() + suffix
}

func (b *boardFlow) addMember(t *testing.T, s session, role domain.Role) {
	t.Helper()
	_, err := b.pool.Exec(context.Background(),
		`INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, $3)`, b.boardID, s.userID, string(role))
	require.NoError(t, err)
}

func TestFlowSnapshotReflectsOperations(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	todo, done := uuid.New(), uuid.New()
	cardA, cardB := uuid.New(), uuid.New()

	b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: todo, Title: "Todo"})
	b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: done, Title: "Done", AfterID: &todo})
	b.mustApply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: cardA, ColumnID: todo, Title: "A", Description: new("first card")})
	b.mustApply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: cardB, ColumnID: todo, Title: "B", BeforeID: &cardA})
	b.mustApply(t, boardcontent.OpCardMove, boardcontent.CardMove{CardID: cardA, ToColumnID: done, BaseVersion: new(int64(1))})
	b.mustApply(t, boardcontent.OpCardEdit, boardcontent.CardEdit{CardID: cardB, Title: new("B2")})

	snap := b.mustJSON(t, http.MethodGet, b.path(""), "", b.owner.access, http.StatusOK)
	assert.EqualValues(t, 6, snap["seq"])

	board := snap["board"].(map[string]any)
	assert.Equal(t, b.boardID.String(), board["id"])
	assert.Equal(t, "Ops", board["title"])
	assert.Equal(t, "owner", board["role"])
	assert.Equal(t, b.owner.userID, board["owner_id"])

	columns := snap["columns"].([]any)
	require.Len(t, columns, 2)
	assert.Equal(t, todo.String(), columns[0].(map[string]any)["id"])
	assert.Equal(t, done.String(), columns[1].(map[string]any)["id"])
	assert.EqualValues(t, 1, columns[0].(map[string]any)["version"])

	cards := map[string]map[string]any{}
	for _, c := range snap["cards"].([]any) {
		card := c.(map[string]any)
		cards[card["id"].(string)] = card
	}
	require.Len(t, cards, 2)
	assert.Equal(t, done.String(), cards[cardA.String()]["column_id"])
	assert.EqualValues(t, 2, cards[cardA.String()]["version"])
	assert.Equal(t, "first card", cards[cardA.String()]["description"])
	assert.Equal(t, "B2", cards[cardB.String()]["title"])
	assert.EqualValues(t, 2, cards[cardB.String()]["version"])
	assert.EqualValues(t, 1, cards[cardB.String()]["description_version"])
	assert.Equal(t, b.owner.userID, cards[cardB.String()]["created_by"])
	assert.NoError(t, rank.Validate(cards[cardA.String()]["rank"].(string)))

	members := snap["members"].([]any)
	require.Len(t, members, 1)
	assert.Equal(t, "owner", members[0].(map[string]any)["role"])
	assert.Equal(t, "OWNER", members[0].(map[string]any)["display_name"])
}

func TestFlowSnapshotShowsTheRequestersRole(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	editor := b.register(t, "editor")
	viewer := b.register(t, "viewer")
	b.addMember(t, editor, domain.RoleEditor)
	b.addMember(t, viewer, domain.RoleViewer)

	for name, tc := range map[string]struct {
		s    session
		role string
	}{"owner": {b.owner, "owner"}, "editor": {editor, "editor"}, "viewer": {viewer, "viewer"}} {
		snap := b.mustJSON(t, http.MethodGet, b.path(""), "", tc.s.access, http.StatusOK)
		assert.Equal(t, tc.role, snap["board"].(map[string]any)["role"], name)
		assert.Len(t, snap["members"], 3, name)
		b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=0"), "", tc.s.access, http.StatusOK)
	}
}

func TestFlowSnapshotAndEventsAccessErrors(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	stranger := b.register(t, "stranger")

	for _, path := range []string{b.path(""), b.path("/events?after_seq=0")} {
		b.expectError(t, http.MethodGet, path, "", "", http.StatusUnauthorized, httpapi.CodeUnauthorized)
		b.expectError(t, http.MethodGet, path, "", "garbage", http.StatusUnauthorized, httpapi.CodeInvalidToken)
		b.expectError(t, http.MethodGet, path, "", stranger.access, http.StatusNotFound, httpapi.CodeNotFound)
	}

	unknown := "/api/boards/" + uuid.NewString()
	b.expectError(t, http.MethodGet, unknown, "", b.owner.access, http.StatusNotFound, httpapi.CodeNotFound)
	b.expectError(t, http.MethodGet, unknown+"/events?after_seq=0", "", b.owner.access, http.StatusNotFound, httpapi.CodeNotFound)
	b.expectError(t, http.MethodGet, "/api/boards/not-a-uuid", "", b.owner.access, http.StatusBadRequest, httpapi.CodeValidation)
	b.expectError(t, http.MethodGet, "/api/boards/not-a-uuid/events?after_seq=0", "", b.owner.access, http.StatusBadRequest, httpapi.CodeValidation)

	b.mustJSON(t, http.MethodDelete, b.path(""), "", b.owner.access, http.StatusNoContent)
	b.expectError(t, http.MethodGet, b.path(""), "", b.owner.access, http.StatusNotFound, httpapi.CodeNotFound)
	b.expectError(t, http.MethodGet, b.path("/events?after_seq=0"), "", b.owner.access, http.StatusNotFound, httpapi.CodeNotFound)
}

func TestFlowEventLogMatchesOperationOutcomes(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	col, first, last := uuid.New(), uuid.New(), uuid.New()

	b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: col, Title: "Todo"})
	b.mustApply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: first, ColumnID: col, Title: "first"})
	b.mustApply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: last, ColumnID: col, Title: "last"})
	_, err := b.pool.Exec(context.Background(), `UPDATE cards SET rank = 'a0' WHERE id = $1`, first)
	require.NoError(t, err)
	_, err = b.pool.Exec(context.Background(), `UPDATE cards SET rank = $2 WHERE id = $1`, last, "a0"+strings.Repeat("0", 62)+"1")
	require.NoError(t, err)
	out := b.mustApply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "wedge", AfterID: &first, BeforeID: &last})
	require.Len(t, out.Extra, 1, "the wedge insert needs a rebalance")

	nack := b.apply(t, b.owner, boardcontent.OpCardMove, boardcontent.CardMove{CardID: first, ToColumnID: col, BaseVersion: new(int64(9))})
	require.NotNil(t, nack.Nack)

	page := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=0"), "", b.owner.access, http.StatusOK)
	assert.EqualValues(t, 5, page["seq"])
	assert.Equal(t, false, page["has_more"])
	events := page["events"].([]any)
	require.Len(t, events, 5, "a nack never appears in the event log")

	wantTypes := []string{boardcontent.EventColumnCreated, boardcontent.EventCardCreated, boardcontent.EventCardCreated, boardcontent.EventColumnRebalanced, boardcontent.EventCardCreated}
	for i, e := range events {
		ev := e.(map[string]any)
		assert.EqualValues(t, 1, ev["v"])
		assert.EqualValues(t, i+1, ev["seq"])
		assert.Equal(t, wantTypes[i], ev["type"])
		assert.Greater(t, ev["server_ts"].(float64), float64(1_600_000_000_000))
		assert.NotNil(t, ev["payload"])
		if i != 3 {
			assert.NotEmpty(t, ev["op_id"])
		} else {
			assert.NotContains(t, ev, "op_id", "rebalance events are not tied to a client op")
		}
	}
	assert.Equal(t, out.Event.OpID.String(), events[4].(map[string]any)["op_id"], "the rebalance precedes the confirming event")

	rebalanced := events[3].(map[string]any)["payload"].(map[string]any)
	assert.Equal(t, col.String(), rebalanced["column_id"])
	assert.Len(t, rebalanced["cards"], 2, "only cards that already existed are respaced")
}

func TestFlowEventLogPaginationAcrossThePageLimit(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	const total = 1500
	_, err := b.pool.Exec(context.Background(),
		`INSERT INTO board_events (board_id, seq, type, payload)
		 SELECT $1, g, 'test.event', jsonb_build_object('n', g) FROM generate_series(1, $2::int) AS g`,
		b.boardID, total)
	require.NoError(t, err)
	_, err = b.pool.Exec(context.Background(), `UPDATE boards SET seq = $2 WHERE id = $1`, b.boardID, total)
	require.NoError(t, err)

	first := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=0"), "", b.owner.access, http.StatusOK)
	assert.Len(t, first["events"], 1000)
	assert.Equal(t, true, first["has_more"])
	assert.EqualValues(t, total, first["seq"])
	firstEvents := first["events"].([]any)
	assert.EqualValues(t, 1, firstEvents[0].(map[string]any)["seq"])
	assert.EqualValues(t, 1000, firstEvents[999].(map[string]any)["seq"])

	second := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=1000"), "", b.owner.access, http.StatusOK)
	assert.Len(t, second["events"], 500)
	assert.Equal(t, false, second["has_more"])
	secondEvents := second["events"].([]any)
	assert.EqualValues(t, 1001, secondEvents[0].(map[string]any)["seq"])
	assert.EqualValues(t, total, secondEvents[499].(map[string]any)["seq"])

	exact := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=500"), "", b.owner.access, http.StatusOK)
	assert.Len(t, exact["events"], 1000)
	assert.Equal(t, false, exact["has_more"], "a page that ends exactly at the head has no more")

	current := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq="+strconv.Itoa(total)), "", b.owner.access, http.StatusOK)
	assert.Empty(t, current["events"])
	assert.Equal(t, false, current["has_more"])
}

func TestFlowEventLogQueryValidation(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "Todo"})

	bad := []string{"", "?after_seq=", "?after_seq=-1", "?after_seq=abc", "?after_seq=1.5", "?after_seq=0x1", "?after_seq=99999999999999999999", "?after_seq=%20", "?seq=0"}
	for _, q := range bad {
		b.expectError(t, http.MethodGet, b.path("/events"+q), "", b.owner.access, http.StatusBadRequest, httpapi.CodeValidation)
	}
	b.expectError(t, http.MethodGet, b.path("/events?after_seq=2"), "", b.owner.access, http.StatusBadRequest, httpapi.CodeValidation)
	b.expectError(t, http.MethodGet, b.path("/events?after_seq=9223372036854775807"), "", b.owner.access, http.StatusBadRequest, httpapi.CodeValidation)

	for _, q := range []string{"?after_seq=0", "?after_seq=1", "?after_seq=00", "?after_seq=0&after_seq=1"} {
		b.mustJSON(t, http.MethodGet, b.path("/events"+q), "", b.owner.access, http.StatusOK)
	}
}

func TestFlowEventLogGoneAfterPruning(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	for i := range 6 {
		b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: fmt.Sprintf("c%d", i)})
	}
	_, err := b.pool.Exec(context.Background(), `DELETE FROM board_events WHERE board_id = $1 AND seq <= 3`, b.boardID)
	require.NoError(t, err)

	for _, after := range []int{0, 1, 2} {
		rec := b.expectError(t, http.MethodGet, b.path("/events?after_seq="+strconv.Itoa(after)), "", b.owner.access, http.StatusGone, httpapi.CodeGone)
		_, msg := rec.errorBody(t)
		assert.Contains(t, msg, "snapshot")
	}
	page := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=3"), "", b.owner.access, http.StatusOK)
	assert.Len(t, page["events"], 3)

	snap := b.mustJSON(t, http.MethodGet, b.path(""), "", b.owner.access, http.StatusOK)
	assert.EqualValues(t, 6, snap["seq"])
	assert.Len(t, snap["columns"], 6, "the snapshot is the recovery path when the delta is gone")
}

func TestFlowSnapshotSeqMatchesEventLogHead(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	col := uuid.New()
	b.mustApply(t, boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: col, Title: "Todo"})
	for i := range 8 {
		b.mustApply(t, boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: fmt.Sprintf("c%d", i)})
	}
	b.mustJSON(t, http.MethodPatch, b.path(""), `{"title":"Renamed"}`, b.owner.access, http.StatusOK)

	snap := b.mustJSON(t, http.MethodGet, b.path(""), "", b.owner.access, http.StatusOK)
	page := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=0"), "", b.owner.access, http.StatusOK)
	assert.Equal(t, snap["seq"], page["seq"])
	events := page["events"].([]any)
	assert.EqualValues(t, snap["seq"], events[len(events)-1].(map[string]any)["seq"])
	assert.Equal(t, "board.renamed", events[len(events)-1].(map[string]any)["type"])
	assert.Equal(t, "Renamed", snap["board"].(map[string]any)["title"])
	assert.Len(t, snap["cards"], 8)
}

func TestFlowRenameAndDeleteEventsCarryActor(t *testing.T) {
	b := newBoardFlowWithBoard(t)
	b.mustJSON(t, http.MethodPatch, b.path(""), `{"title":"New name"}`, b.owner.access, http.StatusOK)

	page := b.mustJSON(t, http.MethodGet, b.path("/events?after_seq=0"), "", b.owner.access, http.StatusOK)
	events := page["events"].([]any)
	require.Len(t, events, 1)
	payload := events[0].(map[string]any)["payload"].(map[string]any)
	assert.Equal(t, "New name", payload["title"])
	assert.Equal(t, b.owner.userID, payload["actor_id"])
}
