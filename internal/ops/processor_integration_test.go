package ops_test

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

type recorder struct {
	mu      sync.Mutex
	events  []domain.Event
	batches [][]domain.Event
}

func (r *recorder) Publish(_ context.Context, events []domain.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, events...)
	r.batches = append(r.batches, append([]domain.Event(nil), events...))
	return nil
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, ev := range r.events {
		out[i] = ev.Type
	}
	return out
}

type fixture struct {
	pool  *pgxpool.Pool
	store *storage.Store
	proc  *ops.Processor
	pub   *recorder
	owner domain.User
	board domain.BoardWithRole
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	pub := &recorder{}
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	board, err := store.CreateBoardWithOwner(context.Background(), domain.Board{ID: uuid.New(), Title: "Board", OwnerID: owner.ID})
	require.NoError(t, err)
	return &fixture{pool: pool, store: store, proc: ops.NewProcessor(store, ops.WithPublisher(pub)), pub: pub, owner: owner, board: board}
}

func (f *fixture) member(t *testing.T, role domain.Role) domain.User {
	t.Helper()
	u := testutil.CreateUser(t, f.store, testutil.UniqueEmail(string(role)))
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, $3)`, f.board.ID, u.ID, string(role))
	require.NoError(t, err)
	return u
}

func (f *fixture) applyAs(t *testing.T, user uuid.UUID, opID uuid.UUID, opType string, payload any) ops.Outcome {
	t.Helper()
	op, err := protocol.NewOp(opType, opID, payload)
	require.NoError(t, err)
	out, err := f.proc.Apply(context.Background(), user, f.board.ID, op)
	require.NoError(t, err)
	return out
}

func (f *fixture) apply(t *testing.T, opType string, payload any) ops.Outcome {
	t.Helper()
	return f.applyAs(t, f.owner.ID, uuid.New(), opType, payload)
}

func (f *fixture) mustEvent(t *testing.T, opType string, payload any) domain.Event {
	t.Helper()
	out := f.apply(t, opType, payload)
	require.Nil(t, out.Nack, "unexpected nack: %+v", out.Nack)
	require.NotNil(t, out.Event)
	return *out.Event
}

func (f *fixture) mustNack(t *testing.T, reason, opType string, payload any) protocol.Nack {
	t.Helper()
	out := f.apply(t, opType, payload)
	require.NotNil(t, out.Nack, "expected nack %s", reason)
	assert.Equal(t, reason, out.Nack.Reason)
	assert.Nil(t, out.Event)
	return *out.Nack
}

func (f *fixture) column(t *testing.T, title string, after *uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.mustEvent(t, protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: id, Title: title, AfterID: after})
	return id
}

func (f *fixture) card(t *testing.T, column uuid.UUID, title string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.mustEvent(t, protocol.OpCardCreate, protocol.CardCreate{CardID: id, ColumnID: column, Title: title})
	return id
}

func (f *fixture) snapshot(t *testing.T) domain.BoardSnapshot {
	t.Helper()
	s, err := f.store.Snapshot(context.Background(), f.board.ID, f.owner.ID)
	require.NoError(t, err)
	return s
}

func cardTitles(s domain.BoardSnapshot, column uuid.UUID) []string {
	var out []string
	for _, c := range s.Cards {
		if c.ColumnID == column {
			out = append(out, c.Title)
		}
	}
	return out
}

func ptr[T any](v T) *T {
	return &v
}

func decode[T any](t *testing.T, ev domain.Event) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(ev.Payload, &v))
	return v
}

func TestCreateAndOrderColumnsAndCards(t *testing.T) {
	f := newFixture(t)
	todo := f.column(t, "Todo", nil)
	done := f.column(t, "Done", nil)
	doing := f.column(t, "Doing", &todo)

	a := f.card(t, todo, "A")
	f.card(t, todo, "B")
	c := uuid.New()
	ev := f.mustEvent(t, protocol.OpCardCreate, protocol.CardCreate{CardID: c, ColumnID: todo, Title: "C", BeforeID: &a, Description: ptr("multi\nline")})
	created := decode[protocol.CardCreated](t, ev)
	assert.Equal(t, c, created.ID)
	assert.Equal(t, "multi\nline", created.Description)
	assert.Equal(t, int64(1), created.Version)
	assert.Equal(t, f.owner.ID, created.ActorID)
	assert.Equal(t, protocol.EventCardCreated, ev.Type)
	require.NotNil(t, ev.OpID)

	s := f.snapshot(t)
	require.Len(t, s.Columns, 3)
	assert.Equal(t, []uuid.UUID{todo, doing, done}, []uuid.UUID{s.Columns[0].ID, s.Columns[1].ID, s.Columns[2].ID})
	assert.Equal(t, []string{"C", "A", "B"}, cardTitles(s, todo))
	assert.Equal(t, int64(6), s.Seq)
	require.Len(t, s.Members, 1)
	assert.Equal(t, f.owner.DisplayName, s.Members[0].DisplayName)
	assert.Equal(t, domain.RoleOwner, s.Members[0].Role)
	assert.Len(t, f.pub.types(), 6)
}

func TestMoveCardChecksVersionAndFallsBackOnMissingAnchors(t *testing.T) {
	f := newFixture(t)
	todo := f.column(t, "Todo", nil)
	done := f.column(t, "Done", nil)
	a := f.card(t, todo, "A")
	b := f.card(t, done, "B")
	c := f.card(t, done, "C")

	ev := f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: a, ToColumnID: done, AfterID: &b, BeforeID: &c, BaseVersion: ptr(int64(1))})
	moved := decode[protocol.CardMoved](t, ev)
	assert.Equal(t, todo, moved.FromColumnID)
	assert.Equal(t, done, moved.ToColumnID)
	assert.Equal(t, int64(2), moved.Version)
	assert.Equal(t, []string{"B", "A", "C"}, cardTitles(f.snapshot(t), done))

	n := f.mustNack(t, protocol.ReasonVersionConflict, protocol.OpCardMove, protocol.CardMove{CardID: a, ToColumnID: todo, BaseVersion: ptr(int64(1))})
	assert.Equal(t, f.snapshot(t).Seq, n.AtSeq)

	ghost := uuid.New()
	f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: a, ToColumnID: done, AfterID: &ghost, BeforeID: &b, BaseVersion: ptr(int64(2))})
	assert.Equal(t, []string{"A", "B", "C"}, cardTitles(f.snapshot(t), done))

	f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: a, ToColumnID: done, AfterID: &ghost, BaseVersion: ptr(int64(3))})
	assert.Equal(t, []string{"B", "C", "A"}, cardTitles(f.snapshot(t), done))

	f.mustEvent(t, protocol.OpCardMove, protocol.CardMove{CardID: c, ToColumnID: done, BaseVersion: ptr(int64(1))})
	assert.Equal(t, []string{"B", "A", "C"}, cardTitles(f.snapshot(t), done))

	f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardMove, protocol.CardMove{CardID: a, ToColumnID: uuid.New(), BaseVersion: ptr(int64(4))})
}

func TestEditCardRules(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")

	ev := f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr("A2")})
	edited := decode[protocol.CardEdited](t, ev)
	assert.Equal(t, "A2", *edited.Title)
	assert.Nil(t, edited.Description)
	assert.Equal(t, int64(2), edited.Version)
	assert.Equal(t, int64(1), edited.DescriptionVersion)

	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("x")})
	ev = f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("desc"), BaseDescriptionVersion: ptr(int64(1))})
	edited = decode[protocol.CardEdited](t, ev)
	assert.Equal(t, int64(3), edited.Version)
	assert.Equal(t, int64(2), edited.DescriptionVersion)
	f.mustNack(t, protocol.ReasonVersionConflict, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Description: ptr("stale"), BaseDescriptionVersion: ptr(int64(1))})
	f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr("LWW")})

	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardEdit, protocol.CardEdit{CardID: card})
	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr("bad\x00title")})
	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardEdit, protocol.CardEdit{CardID: card, Title: ptr("   ")})
}

func TestArchiveRules(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	keep := f.column(t, "Keep", nil)
	a := f.card(t, col, "A")
	b := f.card(t, col, "B")
	other := f.card(t, keep, "Other")

	f.mustEvent(t, protocol.OpCardArchive, protocol.CardArchive{CardID: a})
	f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardArchive, protocol.CardArchive{CardID: a})
	f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardEdit, protocol.CardEdit{CardID: a, Title: ptr("x")})

	ev := f.mustEvent(t, protocol.OpColumnArchive, protocol.ColumnArchive{ColumnID: col})
	archived := decode[protocol.ColumnArchived](t, ev)
	assert.Equal(t, []uuid.UUID{b}, archived.CardIDs)
	f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "late"})
	f.mustNack(t, protocol.ReasonNotFound, protocol.OpCardMove, protocol.CardMove{CardID: other, ToColumnID: col, BaseVersion: ptr(int64(1))})
	f.mustNack(t, protocol.ReasonNotFound, protocol.OpColumnRename, protocol.ColumnRename{ColumnID: col, Title: "x"})

	s := f.snapshot(t)
	require.Len(t, s.Columns, 1)
	assert.Equal(t, []string{"Other"}, cardTitles(s, keep))
	assert.Len(t, s.Cards, 1)
}

func TestColumnRenameAndMove(t *testing.T) {
	f := newFixture(t)
	a := f.column(t, "A", nil)
	b := f.column(t, "B", nil)
	ev := f.mustEvent(t, protocol.OpColumnRename, protocol.ColumnRename{ColumnID: a, Title: "A2"})
	assert.Equal(t, int64(2), decode[protocol.ColumnRenamed](t, ev).Version)

	f.mustNack(t, protocol.ReasonVersionConflict, protocol.OpColumnMove, protocol.ColumnMove{ColumnID: a, AfterID: &b, BaseVersion: ptr(int64(1))})
	f.mustEvent(t, protocol.OpColumnMove, protocol.ColumnMove{ColumnID: a, AfterID: &b, BaseVersion: ptr(int64(2))})
	s := f.snapshot(t)
	assert.Equal(t, []uuid.UUID{b, a}, []uuid.UUID{s.Columns[0].ID, s.Columns[1].ID})
}

func TestRoleChecks(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	viewer := f.member(t, domain.RoleViewer)
	editor := f.member(t, domain.RoleEditor)
	outsider := testutil.CreateUser(t, f.store, testutil.UniqueEmail("outsider"))

	out := f.applyAs(t, viewer.ID, uuid.New(), protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonForbidden, out.Nack.Reason)

	out = f.applyAs(t, outsider.ID, uuid.New(), protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonForbidden, out.Nack.Reason)

	out = f.applyAs(t, editor.ID, uuid.New(), protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "x"})
	assert.Nil(t, out.Nack)
}

func TestIdempotentReplay(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	opID := uuid.New()
	cardID := uuid.New()
	payload := protocol.CardCreate{CardID: cardID, ColumnID: col, Title: "once"}

	first := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, payload)
	require.NotNil(t, first.Event)
	published := len(f.pub.types())
	for range 3 {
		again := f.applyAs(t, f.owner.ID, opID, protocol.OpCardCreate, payload)
		require.NotNil(t, again.Event)
		assert.True(t, again.Replayed)
		assert.Equal(t, first.Event.Seq, again.Event.Seq)
		assert.JSONEq(t, string(first.Event.Payload), string(again.Event.Payload))
	}
	assert.Len(t, f.pub.types(), published, "replays are not republished")
	assert.Len(t, f.snapshot(t).Cards, 1)

	nackOp := uuid.New()
	bad := protocol.CardMove{CardID: cardID, ToColumnID: col, BaseVersion: ptr(int64(9))}
	n1 := f.applyAs(t, f.owner.ID, nackOp, protocol.OpCardMove, bad)
	require.NotNil(t, n1.Nack)
	f.mustEvent(t, protocol.OpCardEdit, protocol.CardEdit{CardID: cardID, Title: ptr("bump")})
	n2 := f.applyAs(t, f.owner.ID, nackOp, protocol.OpCardMove, bad)
	require.NotNil(t, n2.Nack)
	assert.True(t, n2.Replayed)
	assert.Equal(t, *n1.Nack, *n2.Nack)

	other := f.member(t, domain.RoleEditor)
	stolen := f.applyAs(t, other.ID, opID, protocol.OpCardCreate, payload)
	require.NotNil(t, stolen.Nack)
	assert.Equal(t, protocol.ReasonValidation, stolen.Nack.Reason)

	dup := f.mustNack(t, protocol.ReasonValidation, protocol.OpCardCreate, protocol.CardCreate{CardID: cardID, ColumnID: col, Title: "dup"})
	assert.Contains(t, dup.Message, "card_id")
}

func TestValidationNacks(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	seqBefore := f.snapshot(t).Seq

	op := protocol.Op{Type: protocol.OpCardCreate, OpID: uuid.New(), Payload: json.RawMessage(`{"card_id":"` + uuid.NewString() + `","column_id":"` + col.String() + `","title":"x","extra":1}`)}
	out, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonValidation, out.Nack.Reason)

	op = protocol.Op{Type: "op.card.explode", OpID: uuid.New(), Payload: json.RawMessage(`{}`)}
	out, err = f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
	require.NoError(t, err)
	require.NotNil(t, out.Nack)
	assert.Equal(t, protocol.ReasonValidation, out.Nack.Reason)

	long := make([]byte, 501)
	for i := range long {
		long[i] = 'x'
	}
	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: string(long)})
	f.mustNack(t, protocol.ReasonValidation, protocol.OpColumnCreate, protocol.ColumnCreate{ColumnID: uuid.New(), Title: string(long[:201])})
	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardMove, protocol.CardMove{CardID: uuid.New(), ToColumnID: col})
	f.mustNack(t, protocol.ReasonValidation, protocol.OpCardCreate, protocol.CardCreate{ColumnID: col, Title: "x"})

	assert.Equal(t, seqBefore, f.snapshot(t).Seq, "nacks never bump seq")
	var stored int
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT count(*) FROM processed_ops WHERE outcome->>'kind' = 'nack'`).Scan(&stored))
	assert.Equal(t, 6, stored)
}

func TestRebalanceWhenRanksGrowTooLong(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	first := f.card(t, col, "first")
	last := f.card(t, col, "last")
	f.mustEvent(t, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "mid", AfterID: &first, BeforeID: &last})

	rebalanced := false
	for i := 0; i < 500 && !rebalanced; i++ {
		out := f.apply(t, protocol.OpCardCreate, protocol.CardCreate{CardID: uuid.New(), ColumnID: col, Title: "n", AfterID: &first})
		require.Nil(t, out.Nack)
		for _, ev := range out.Extra {
			if ev.Type == protocol.EventColumnRebalanced {
				rebalanced = true
				payload := decode[protocol.ColumnRebalanced](t, ev)
				assert.Equal(t, col, payload.ColumnID)
				assert.Greater(t, ev.Seq, out.Event.Seq)
				assert.Nil(t, ev.OpID)
			}
		}
	}
	require.True(t, rebalanced)

	s := f.snapshot(t)
	ranks := make([]string, 0, len(s.Cards))
	for _, c := range s.Cards {
		assert.LessOrEqual(t, len(c.Rank), 64)
		ranks = append(ranks, c.Rank)
	}
	assert.True(t, sort.StringsAreSorted(ranks))
	titles := cardTitles(s, col)
	assert.Equal(t, "first", titles[0])
	assert.Equal(t, "last", titles[len(titles)-1])
}

func TestEventsAfter(t *testing.T) {
	f := newFixture(t)
	col := f.column(t, "Todo", nil)
	f.card(t, col, "A")
	f.card(t, col, "B")

	page, err := f.store.EventsAfter(context.Background(), f.board.ID, f.owner.ID, 1, 1)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	assert.Equal(t, int64(2), page.Events[0].Seq)
	assert.True(t, page.HasMore)
	assert.Equal(t, int64(3), page.Seq)

	page, err = f.store.EventsAfter(context.Background(), f.board.ID, f.owner.ID, 3, 100)
	require.NoError(t, err)
	assert.Empty(t, page.Events)

	_, err = f.store.EventsAfter(context.Background(), f.board.ID, f.owner.ID, 4, 100)
	var vErr *domain.ValidationError
	assert.ErrorAs(t, err, &vErr)

	_, err = f.pool.Exec(context.Background(), `DELETE FROM board_events WHERE board_id = $1 AND seq < 3`, f.board.ID)
	require.NoError(t, err)
	_, err = f.store.EventsAfter(context.Background(), f.board.ID, f.owner.ID, 1, 100)
	assert.ErrorIs(t, err, domain.ErrGone)
	page, err = f.store.EventsAfter(context.Background(), f.board.ID, f.owner.ID, 2, 100)
	require.NoError(t, err)
	assert.Len(t, page.Events, 1)

	outsider := testutil.CreateUser(t, f.store, testutil.UniqueEmail("outsider"))
	_, err = f.store.EventsAfter(context.Background(), f.board.ID, outsider.ID, 0, 100)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.store.Snapshot(context.Background(), f.board.ID, outsider.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestApplyOnMissingBoard(t *testing.T) {
	f := newFixture(t)
	op, err := protocol.NewOp(protocol.OpColumnCreate, uuid.New(), protocol.ColumnCreate{ColumnID: uuid.New(), Title: "x"})
	require.NoError(t, err)
	_, err = f.proc.Apply(context.Background(), f.owner.ID, uuid.New(), op)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
