package storage_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/rank"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type contentFixture struct {
	pool  *pgxpool.Pool
	store *storage.Store
	owner domain.User
	board domain.BoardWithRole
}

func newContentFixture(t *testing.T) *contentFixture {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool, storage.WithLockTimeout(time.Minute))
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	return &contentFixture{pool: pool, store: store, owner: owner, board: createBoard(t, store, owner.ID, "Content")}
}

func (c *contentFixture) addMember(t *testing.T, role domain.Role) domain.User {
	t.Helper()
	u := testutil.CreateUser(t, c.store, testutil.UniqueEmail(string(role)))
	_, err := c.pool.Exec(context.Background(),
		`INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, $3)`, c.board.ID, u.ID, string(role))
	require.NoError(t, err)
	return u
}

func (c *contentFixture) addColumn(t *testing.T, title, rankKey string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := c.store.WithBoardLock(context.Background(), c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		_, err := btx.InsertColumn(ctx, domain.Column{ID: id, Title: title, Rank: rankKey})
		return err
	})
	require.NoError(t, err)
	return id
}

func (c *contentFixture) addCardWithEvent(ctx context.Context, column uuid.UUID, title, rankKey string) (uuid.UUID, error) {
	id := uuid.New()
	err := c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		if _, err := btx.InsertCard(ctx, domain.Card{ID: id, ColumnID: column, Title: title, Rank: rankKey, CreatedBy: c.owner.ID}); err != nil {
			return err
		}
		_, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "card.created", Payload: map[string]string{"title": title}, ActorID: &c.owner.ID})
		return err
	})
	return id, err
}

func (c *contentFixture) appendEvents(t *testing.T, n int) {
	t.Helper()
	err := c.store.WithBoardLock(context.Background(), c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		for i := range n {
			if _, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "test.event", Payload: map[string]int{"n": i}}); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
}

func seqsOfEvents(events []domain.Event) []int64 {
	out := make([]int64, len(events))
	for i, ev := range events {
		out[i] = ev.Seq
	}
	return out
}

func TestSnapshotContent(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	editor := c.addMember(t, domain.RoleEditor)
	viewer := c.addMember(t, domain.RoleViewer)

	second := c.addColumn(t, "Second", "a1")
	first := c.addColumn(t, "First", "a0")
	archived := c.addColumn(t, "Archived", "a2")
	_, err := c.pool.Exec(ctx, `UPDATE columns SET archived_at = now() WHERE id = $1`, archived)
	require.NoError(t, err)

	c1, err := c.addCardWithEvent(ctx, first, "F2", "a1")
	require.NoError(t, err)
	c2, err := c.addCardWithEvent(ctx, first, "F1", "a0")
	require.NoError(t, err)
	c3, err := c.addCardWithEvent(ctx, second, "S1", "a0")
	require.NoError(t, err)
	gone, err := c.addCardWithEvent(ctx, first, "Gone", "a2")
	require.NoError(t, err)
	_, err = c.pool.Exec(ctx, `UPDATE cards SET archived_at = now() WHERE id = $1`, gone)
	require.NoError(t, err)

	snap, err := c.store.Snapshot(ctx, c.board.ID, viewer.ID)
	require.NoError(t, err)

	assert.Equal(t, c.board.ID, snap.Board.ID)
	assert.Equal(t, "Content", snap.Board.Title)
	assert.Equal(t, c.owner.ID, snap.Board.OwnerID)
	assert.Equal(t, domain.RoleViewer, snap.Board.Role, "the role is the requester's role")
	assert.Equal(t, int64(4), snap.Seq)
	assert.Equal(t, snap.Seq, snap.Board.Seq)

	require.Len(t, snap.Columns, 2)
	assert.Equal(t, []uuid.UUID{first, second}, []uuid.UUID{snap.Columns[0].ID, snap.Columns[1].ID}, "columns are ordered by rank")
	assert.Equal(t, "First", snap.Columns[0].Title)
	assert.Equal(t, int64(1), snap.Columns[0].Version)

	require.Len(t, snap.Cards, 3)
	byColumn := map[uuid.UUID][]uuid.UUID{}
	for _, card := range snap.Cards {
		byColumn[card.ColumnID] = append(byColumn[card.ColumnID], card.ID)
		assert.Nil(t, card.ArchivedAt)
		assert.Equal(t, c.board.ID, card.BoardID)
		assert.Equal(t, c.owner.ID, card.CreatedBy)
		assert.Equal(t, int64(1), card.Version)
		assert.Equal(t, int64(1), card.DescriptionVersion)
		assert.False(t, card.CreatedAt.IsZero())
	}
	assert.Equal(t, []uuid.UUID{c2, c1}, byColumn[first], "cards are ordered by rank inside a column")
	assert.Equal(t, []uuid.UUID{c3}, byColumn[second])

	require.Len(t, snap.Members, 3)
	roles := map[uuid.UUID]domain.Role{}
	for _, m := range snap.Members {
		roles[m.UserID] = m.Role
		assert.NotEmpty(t, m.DisplayName)
		assert.NotEmpty(t, m.AvatarColor)
		assert.False(t, m.JoinedAt.IsZero())
	}
	assert.Equal(t, map[uuid.UUID]domain.Role{c.owner.ID: domain.RoleOwner, editor.ID: domain.RoleEditor, viewer.ID: domain.RoleViewer}, roles)
}

func TestSnapshotOfAnEmptyBoardHasEmptyNonNilSlices(t *testing.T) {
	c := newContentFixture(t)
	snap, err := c.store.Snapshot(context.Background(), c.board.ID, c.owner.ID)
	require.NoError(t, err)
	assert.NotNil(t, snap.Columns)
	assert.Empty(t, snap.Columns)
	assert.NotNil(t, snap.Cards)
	assert.Empty(t, snap.Cards)
	assert.Len(t, snap.Members, 1)
	assert.Zero(t, snap.Seq)
}

func TestSnapshotAccessRules(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	stranger := testutil.CreateUser(t, c.store, testutil.UniqueEmail("stranger"))

	_, err := c.store.Snapshot(ctx, c.board.ID, stranger.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = c.store.Snapshot(ctx, uuid.New(), c.owner.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = c.store.Snapshot(ctx, c.board.ID, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = c.pool.Exec(ctx, `UPDATE boards SET deleted_at = now() WHERE id = $1`, c.board.ID)
	require.NoError(t, err)
	_, err = c.store.Snapshot(ctx, c.board.ID, c.owner.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 0, 10)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestSnapshotIsConsistentWithSeqUnderConcurrentWrites(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	column := c.addColumn(t, "Todo", "a0")
	ranks, err := rank.Evenly(60)
	require.NoError(t, err)

	const writers = 6
	const perWriter = 10
	var wg sync.WaitGroup
	writeErrs := make(chan error, writers*perWriter)
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				_, err := c.addCardWithEvent(ctx, column, fmt.Sprintf("w%d-%d", w, i), ranks[w*perWriter+i])
				writeErrs <- err
			}
		}()
	}

	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	var mu sync.Mutex
	var mismatches []string
	snapshots := 0
	for range 3 {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snap, err := c.store.Snapshot(ctx, c.board.ID, c.owner.ID)
				mu.Lock()
				snapshots++
				if err != nil {
					mismatches = append(mismatches, "snapshot error: "+err.Error())
				} else if int64(len(snap.Cards)) != snap.Seq || snap.Board.Seq != snap.Seq {
					mismatches = append(mismatches, fmt.Sprintf("seq %d but %d cards", snap.Seq, len(snap.Cards)))
				}
				mu.Unlock()
			}
		}()
	}

	wg.Wait()
	close(writeErrs)
	for err := range writeErrs {
		require.NoError(t, err)
	}
	close(stop)
	readerWg.Wait()

	assert.Empty(t, mismatches, "a snapshot must always match its seq")
	assert.Positive(t, snapshots)

	final, err := c.store.Snapshot(ctx, c.board.ID, c.owner.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(writers*perWriter), final.Seq)
	assert.Len(t, final.Cards, writers*perWriter)
}

func TestEventsAfterIsConsistentWithSeqUnderConcurrentWrites(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()

	const writers = 5
	const perWriter = 12
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				c.appendEvents(t, 1)
			}
		}()
	}

	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	var mu sync.Mutex
	var problems []string
	for range 3 {
		readerWg.Add(1)
		go func() {
			defer readerWg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 0, 1000)
				mu.Lock()
				switch {
				case err != nil:
					problems = append(problems, err.Error())
				case page.HasMore || int64(len(page.Events)) != page.Seq:
					problems = append(problems, fmt.Sprintf("seq %d but %d events", page.Seq, len(page.Events)))
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(stop)
	readerWg.Wait()
	assert.Empty(t, problems)
}

func TestEventsAfterPaging(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	c.appendEvents(t, 25)

	tests := []struct {
		name     string
		after    int64
		limit    int
		wantSeqs []int64
		hasMore  bool
	}{
		{"first page", 0, 10, seqRange(1, 10), true},
		{"second page", 10, 10, seqRange(11, 20), true},
		{"last partial page", 20, 10, seqRange(21, 25), false},
		{"limit equals remaining", 15, 10, seqRange(16, 25), false},
		{"limit one below remaining", 15, 9, seqRange(16, 24), true},
		{"limit one above remaining", 15, 11, seqRange(16, 25), false},
		{"single event pages", 3, 1, seqRange(4, 4), true},
		{"last single event", 24, 1, seqRange(25, 25), false},
		{"everything in one page", 0, 1000, seqRange(1, 25), false},
		{"exactly everything", 0, 25, seqRange(1, 25), false},
		{"one short of everything", 0, 24, seqRange(1, 24), true},
		{"already up to date", 25, 10, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, tt.after, tt.limit)
			require.NoError(t, err)
			assert.Equal(t, int64(25), page.Seq)
			assert.Equal(t, tt.hasMore, page.HasMore)
			assert.NotNil(t, page.Events)
			if tt.wantSeqs == nil {
				assert.Empty(t, page.Events)
				return
			}
			assert.Equal(t, tt.wantSeqs, seqsOfEvents(page.Events))
		})
	}
}

func seqRange(from, to int64) []int64 {
	out := make([]int64, 0, to-from+1)
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func TestEventsAfterWalksTheWholeLogByPages(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	c.appendEvents(t, 47)

	var all []int64
	after := int64(0)
	pages := 0
	for {
		page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, after, 10)
		require.NoError(t, err)
		pages++
		all = append(all, seqsOfEvents(page.Events)...)
		if !page.HasMore {
			break
		}
		after = page.Events[len(page.Events)-1].Seq
		require.Less(t, pages, 20)
	}
	assert.Equal(t, 5, pages)
	assert.Equal(t, seqRange(1, 47), all)
}

func TestEventsAfterReturnsFullEventData(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	opID := uuid.New()
	err := c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		_, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "x.y", Payload: map[string]any{"k": "v", "n": 1}, OpID: &opID, ActorID: &c.owner.ID})
		return err
	})
	require.NoError(t, err)

	page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	ev := page.Events[0]
	assert.Equal(t, c.board.ID, ev.BoardID)
	assert.Equal(t, int64(1), ev.Seq)
	assert.Equal(t, "x.y", ev.Type)
	assert.JSONEq(t, `{"k":"v","n":1}`, string(ev.Payload))
	require.NotNil(t, ev.OpID)
	assert.Equal(t, opID, *ev.OpID)
	require.NotNil(t, ev.ActorID)
	assert.Equal(t, c.owner.ID, *ev.ActorID)
	assert.WithinDuration(t, time.Now(), ev.CreatedAt, time.Minute)
}

func TestEventsAfterBoundaries(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()

	page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 0, 10)
	require.NoError(t, err, "a board without events is up to date at 0")
	assert.Empty(t, page.Events)
	assert.NotNil(t, page.Events)
	assert.Zero(t, page.Seq)
	assert.False(t, page.HasMore)

	_, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 1, 10)
	var vErr *domain.ValidationError
	require.ErrorAs(t, err, &vErr)
	assert.Equal(t, "after_seq", vErr.Field)

	c.appendEvents(t, 10)

	for _, after := range []int64{11, 12, 1 << 40} {
		_, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, after, 10)
		require.ErrorAs(t, err, &vErr, "after_seq=%d is ahead of the board", after)
		assert.Equal(t, "after_seq", vErr.Field)
	}

	page, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 10, 10)
	require.NoError(t, err)
	assert.Empty(t, page.Events)
	assert.Equal(t, int64(10), page.Seq)

	page, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 9, 10)
	require.NoError(t, err)
	assert.Equal(t, []int64{10}, seqsOfEvents(page.Events))
}

func TestEventsAfterReturnsGoneOutsideRetention(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	c.appendEvents(t, 10)

	_, err := c.pool.Exec(ctx, `DELETE FROM board_events WHERE board_id = $1 AND seq <= 4`, c.board.ID)
	require.NoError(t, err)

	for _, after := range []int64{0, 1, 2, 3} {
		_, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, after, 100)
		assert.ErrorIs(t, err, domain.ErrGone, "after_seq=%d needs pruned events", after)
	}
}

func TestEventsAfterEdgeOfRetention(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	c.appendEvents(t, 10)
	_, err := c.pool.Exec(ctx, `DELETE FROM board_events WHERE board_id = $1 AND seq <= 4`, c.board.ID)
	require.NoError(t, err)

	_, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 3, 100)
	assert.ErrorIs(t, err, domain.ErrGone, "seq 4 was pruned, so a client at 3 cannot be caught up")

	page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 4, 100)
	require.NoError(t, err, "a client at 4 needs 5.. which are retained")
	assert.Equal(t, seqRange(5, 10), seqsOfEvents(page.Events))

	page, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 10, 100)
	require.NoError(t, err)
	assert.Empty(t, page.Events)
}

func TestEventsAfterWhenEveryEventWasPruned(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	c.appendEvents(t, 5)
	_, err := c.pool.Exec(ctx, `DELETE FROM board_events WHERE board_id = $1`, c.board.ID)
	require.NoError(t, err)

	_, err = c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 2, 10)
	assert.ErrorIs(t, err, domain.ErrGone)

	page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 5, 10)
	require.NoError(t, err, "a client that is already current never needs the pruned log")
	assert.Empty(t, page.Events)
}

func TestEventsAfterIsScopedToTheBoard(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	other := createBoard(t, c.store, c.owner.ID, "Other")
	c.appendEvents(t, 3)
	err := c.store.WithBoardLock(ctx, other.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		for range 7 {
			if _, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "other"}); err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)

	page, err := c.store.EventsAfter(ctx, c.board.ID, c.owner.ID, 0, 100)
	require.NoError(t, err)
	assert.Equal(t, int64(3), page.Seq)
	assert.Equal(t, seqRange(1, 3), seqsOfEvents(page.Events))
	for _, ev := range page.Events {
		assert.Equal(t, c.board.ID, ev.BoardID)
	}

	stranger := testutil.CreateUser(t, c.store, testutil.UniqueEmail("stranger"))
	_, err = c.store.EventsAfter(ctx, c.board.ID, stranger.ID, 0, 100)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = c.store.EventsAfter(ctx, other.ID, stranger.ID, 0, 100)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestEveryRoleCanReadSnapshotAndEvents(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	editor := c.addMember(t, domain.RoleEditor)
	viewer := c.addMember(t, domain.RoleViewer)
	c.appendEvents(t, 2)

	for name, id := range map[string]uuid.UUID{"owner": c.owner.ID, "editor": editor.ID, "viewer": viewer.ID} {
		snap, err := c.store.Snapshot(ctx, c.board.ID, id)
		require.NoError(t, err, name)
		assert.Equal(t, int64(2), snap.Seq, name)
		page, err := c.store.EventsAfter(ctx, c.board.ID, id, 0, 10)
		require.NoError(t, err, name)
		assert.Len(t, page.Events, 2, name)
	}
}

func TestBoardContentQueriesHideArchivedAndForeignRows(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	other := createBoard(t, c.store, c.owner.ID, "Other")
	col := c.addColumn(t, "Todo", "a0")
	card, err := c.addCardWithEvent(ctx, col, "A", "a0")
	require.NoError(t, err)

	err = c.store.WithBoardLock(ctx, other.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		_, cerr := btx.Column(ctx, col)
		assert.ErrorIs(t, cerr, domain.ErrNotFound, "a column of another board is invisible")
		_, cerr = btx.Card(ctx, card)
		assert.ErrorIs(t, cerr, domain.ErrNotFound, "a card of another board is invisible")
		exists, cerr := btx.CardIDExists(ctx, card)
		assert.NoError(t, cerr)
		assert.True(t, exists, "id uniqueness is global")
		exists, cerr = btx.ColumnIDExists(ctx, col)
		assert.NoError(t, cerr)
		assert.True(t, exists)
		ranks, cerr := btx.ColumnRanks(ctx)
		assert.NoError(t, cerr)
		assert.Empty(t, ranks)
		return nil
	})
	require.NoError(t, err)

	err = c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		ranks, cerr := btx.CardRanks(ctx, col)
		require.NoError(t, cerr)
		require.Len(t, ranks, 1)
		assert.Equal(t, card, ranks[0].ID)

		archived, cerr := btx.ArchiveCard(ctx, card, c.owner.ID)
		require.NoError(t, cerr)
		assert.Equal(t, int64(2), archived.Version)
		assert.NotNil(t, archived.ArchivedAt)

		_, cerr = btx.Card(ctx, card)
		assert.ErrorIs(t, cerr, domain.ErrNotFound)
		_, cerr = btx.ArchiveCard(ctx, card, c.owner.ID)
		assert.ErrorIs(t, cerr, domain.ErrNotFound)
		_, cerr = btx.EditCard(ctx, card, c.owner.ID, domain.CardEdit{Title: new("x")})
		assert.ErrorIs(t, cerr, domain.ErrNotFound)
		_, cerr = btx.MoveCard(ctx, card, c.owner.ID, col, "a5")
		assert.ErrorIs(t, cerr, domain.ErrNotFound)
		ranks, cerr = btx.CardRanks(ctx, col)
		require.NoError(t, cerr)
		assert.Empty(t, ranks)
		return nil
	})
	require.NoError(t, err)
}

func TestProcessedOpsRoundTrip(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	opID := uuid.New()

	err := c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		_, found, err := btx.ProcessedOp(ctx, opID)
		require.NoError(t, err)
		assert.False(t, found)

		saved, err := btx.SaveProcessedOp(ctx, opID, c.owner.ID, map[string]any{"kind": "event", "seq": 3})
		require.NoError(t, err)
		assert.True(t, saved)
		saved, err = btx.SaveProcessedOp(ctx, opID, c.owner.ID, map[string]any{"kind": "nack"})
		require.NoError(t, err)
		assert.False(t, saved, "a second insert for the same op_id is ignored")
		got, found, err := btx.ProcessedOp(ctx, opID)
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, opID, got.OpID)
		assert.Equal(t, c.board.ID, got.BoardID)
		assert.Equal(t, c.owner.ID, got.UserID)
		assert.JSONEq(t, `{"kind":"event","seq":3}`, string(got.Outcome))

		_, err = btx.EventBySeq(ctx, 3)
		assert.ErrorIs(t, err, domain.ErrNotFound)
		return nil
	})
	require.NoError(t, err)
}

func TestSavepointRollsBackOnlyTheInnerWork(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	col := c.addColumn(t, "Todo", "a0")
	boom := fmt.Errorf("boom")

	var kept, dropped uuid.UUID
	err := c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		kept = uuid.New()
		if _, err := btx.InsertCard(ctx, domain.Card{ID: kept, ColumnID: col, Title: "kept", Rank: "a0", CreatedBy: c.owner.ID}); err != nil {
			return err
		}
		spErr := btx.Savepoint(ctx, func(ctx context.Context, sp *storage.BoardTx) error {
			dropped = uuid.New()
			if _, err := sp.InsertCard(ctx, domain.Card{ID: dropped, ColumnID: col, Title: "dropped", Rank: "a1", CreatedBy: c.owner.ID}); err != nil {
				return err
			}
			if _, err := sp.AppendEvent(ctx, domain.EventDraft{Type: "inner"}); err != nil {
				return err
			}
			return boom
		})
		require.ErrorIs(t, spErr, boom)
		assert.Zero(t, btx.Board().Seq, "the outer view of seq is untouched by a rolled back savepoint")

		return btx.Savepoint(ctx, func(ctx context.Context, sp *storage.BoardTx) error {
			_, err := sp.AppendEvent(ctx, domain.EventDraft{Type: "inner.ok"})
			return err
		})
	})
	require.NoError(t, err)

	snap, err := c.store.Snapshot(ctx, c.board.ID, c.owner.ID)
	require.NoError(t, err)
	require.Len(t, snap.Cards, 1)
	assert.Equal(t, kept, snap.Cards[0].ID)
	assert.NotEqual(t, dropped, snap.Cards[0].ID)
	assert.Equal(t, int64(1), snap.Seq, "the rolled back event does not consume a seq")
}

func TestRerankUsesUniqueIndexSafely(t *testing.T) {
	c := newContentFixture(t)
	ctx := context.Background()
	col := c.addColumn(t, "Todo", "a0")
	ids := make([]uuid.UUID, 3)
	for i, r := range []string{"a0", "a1", "a2"} {
		id, err := c.addCardWithEvent(ctx, col, fmt.Sprintf("c%d", i), r)
		require.NoError(t, err)
		ids[i] = id
	}

	err := c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		return btx.RerankCards(ctx, col, []domain.RankedItem{{ID: ids[0], Rank: "a1"}, {ID: ids[1], Rank: "a2"}, {ID: ids[2], Rank: "a3"}})
	})
	require.NoError(t, err, "shifting every rank by one must not trip the unique index")

	snap, err := c.store.Snapshot(ctx, c.board.ID, c.owner.ID)
	require.NoError(t, err)
	got := make([]string, len(snap.Cards))
	for i, card := range snap.Cards {
		got[i] = card.Rank
	}
	assert.Equal(t, []string{"a1", "a2", "a3"}, got)

	err = c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		return btx.RerankCards(ctx, col, []domain.RankedItem{{ID: uuid.New(), Rank: "a9"}})
	})
	require.Error(t, err, "reranking a card that is not in the column is an error, not a silent no-op")

	err = c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		return btx.RerankCards(ctx, col, nil)
	})
	assert.NoError(t, err)
	err = c.store.WithBoardLock(ctx, c.board.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		return btx.RerankColumns(ctx, nil)
	})
	assert.NoError(t, err)
}
