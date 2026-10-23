package ops_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

const concurrentLockTimeout = 60 * time.Second

func newConcurrentFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool, storage.WithLockTimeout(concurrentLockTimeout))
	pub := &recorder{}
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	board, err := store.CreateBoardWithOwner(context.Background(), domain.Board{ID: uuid.New(), Title: "Board", OwnerID: owner.ID})
	require.NoError(t, err)
	return &fixture{pool: pool, store: store, proc: ops.NewProcessor(store, ops.WithPublisher(pub)), pub: pub, owner: owner, board: board}
}

func (f *fixture) newBoard(t *testing.T, owner uuid.UUID) uuid.UUID {
	t.Helper()
	b, err := f.store.CreateBoardWithOwner(context.Background(), domain.Board{ID: uuid.New(), Title: "Other", OwnerID: owner})
	require.NoError(t, err)
	return b.ID
}

func (f *fixture) applyOnBoard(t *testing.T, boardID, user, opID uuid.UUID, opType string, payload any) boardcontent.OpOutcome {
	t.Helper()
	op, err := boardcontent.NewOp(opType, opID, payload)
	require.NoError(t, err)
	out, err := f.proc.Apply(context.Background(), user, boardID, op)
	require.NoError(t, err)
	return out
}

func (f *fixture) applyRaw(t *testing.T, user uuid.UUID, opType, payload string) boardcontent.OpOutcome {
	t.Helper()
	op := boardcontent.Op{Type: opType, OpID: uuid.New(), Payload: json.RawMessage(payload)}
	out, err := f.proc.Apply(context.Background(), user, f.board.ID, op)
	require.NoError(t, err)
	return out
}

type cardState struct {
	ColumnID           uuid.UUID
	Title              string
	Description        string
	Rank               string
	Version            int64
	DescriptionVersion int64
	Archived           bool
	UpdatedBy          uuid.UUID
}

type columnState struct {
	Title    string
	Rank     string
	Version  int64
	Archived bool
}

func (f *fixture) cardRow(t *testing.T, id uuid.UUID) cardState {
	t.Helper()
	var s cardState
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT column_id, title, description, rank, version, description_version, archived_at IS NOT NULL, updated_by FROM cards WHERE id = $1`,
		id).Scan(&s.ColumnID, &s.Title, &s.Description, &s.Rank, &s.Version, &s.DescriptionVersion, &s.Archived, &s.UpdatedBy))
	return s
}

func (f *fixture) columnRow(t *testing.T, id uuid.UUID) columnState {
	t.Helper()
	var s columnState
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT title, rank, version, archived_at IS NOT NULL FROM columns WHERE id = $1`,
		id).Scan(&s.Title, &s.Rank, &s.Version, &s.Archived))
	return s
}

func (f *fixture) setCardRank(t *testing.T, id uuid.UUID, rank string) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), `UPDATE cards SET rank = $2 WHERE id = $1`, id, rank)
	require.NoError(t, err)
}

func (f *fixture) setColumnRank(t *testing.T, id uuid.UUID, rank string) {
	t.Helper()
	_, err := f.pool.Exec(context.Background(), `UPDATE columns SET rank = $2 WHERE id = $1`, id, rank)
	require.NoError(t, err)
}

func (f *fixture) boardSeq(t *testing.T) int64 {
	t.Helper()
	var seq int64
	require.NoError(t, f.pool.QueryRow(context.Background(), `SELECT seq FROM boards WHERE id = $1`, f.board.ID).Scan(&seq))
	return seq
}

func (f *fixture) eventSeqs(t *testing.T) []int64 {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `SELECT seq FROM board_events WHERE board_id = $1 ORDER BY seq`, f.board.ID)
	require.NoError(t, err)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var s int64
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

func (f *fixture) requireGapFreeSeq(t *testing.T) int64 {
	t.Helper()
	seqs := f.eventSeqs(t)
	for i, s := range seqs {
		require.Equal(t, int64(i+1), s, "board_events seq must be contiguous from 1")
	}
	board := f.boardSeq(t)
	require.Equal(t, int64(len(seqs)), board, "boards.seq must equal the number of events")
	return board
}

func (f *fixture) eventsAfter(t *testing.T, afterSeq int64) []domain.Event {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT board_id, seq, type, payload, op_id, actor_id, created_at FROM board_events WHERE board_id = $1 AND seq > $2 ORDER BY seq`,
		f.board.ID, afterSeq)
	require.NoError(t, err)
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var ev domain.Event
		var payload []byte
		require.NoError(t, rows.Scan(&ev.BoardID, &ev.Seq, &ev.Type, &payload, &ev.OpID, &ev.ActorID, &ev.CreatedAt))
		ev.Payload = payload
		out = append(out, ev)
	}
	require.NoError(t, rows.Err())
	return out
}

func (f *fixture) processedOps(t *testing.T) (events, nacks int) {
	t.Helper()
	require.NoError(t, f.pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE outcome->>'kind' = 'event'), count(*) FILTER (WHERE outcome->>'kind' = 'nack') FROM processed_ops WHERE board_id = $1`,
		f.board.ID).Scan(&events, &nacks))
	return events, nacks
}

func (f *fixture) cardsInColumn(t *testing.T, column uuid.UUID) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	for _, c := range f.snapshot(t).Cards {
		if c.ColumnID == column {
			out = append(out, c.ID)
		}
	}
	return out
}

func (f *fixture) columnOrder(t *testing.T) []uuid.UUID {
	t.Helper()
	var out []uuid.UUID
	for _, c := range f.snapshot(t).Columns {
		out = append(out, c.ID)
	}
	return out
}

func (f *fixture) requireUnchanged(t *testing.T, seq int64) {
	t.Helper()
	assert.Equal(t, seq, f.boardSeq(t), "a rejected operation must not change the board seq")
}

type result struct {
	out boardcontent.OpOutcome
	err error
}

func (f *fixture) applyConcurrently(t *testing.T, n int, build func(i int) (user uuid.UUID, op boardcontent.Op)) []result {
	t.Helper()
	prepared := make([]boardcontent.Op, n)
	users := make([]uuid.UUID, n)
	for i := range n {
		users[i], prepared[i] = build(i)
	}
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, err := f.proc.Apply(context.Background(), users[i], f.board.ID, prepared[i])
			results[i] = result{out: out, err: err}
		}()
	}
	close(start)
	wg.Wait()
	for i, r := range results {
		require.NoError(t, r.err, "operation %d", i)
	}
	return results
}

func mustOp(t *testing.T, opType string, opID uuid.UUID, payload any) boardcontent.Op {
	t.Helper()
	op, err := boardcontent.NewOp(opType, opID, payload)
	require.NoError(t, err)
	return op
}
