package boards_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

type env struct {
	pool  *pgxpool.Pool
	store *storage.Store
	svc   *boards.Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	return &env{pool: pool, store: store, svc: boards.NewService(store)}
}

func (e *env) user(t *testing.T, name string) domain.User {
	t.Helper()
	return testutil.CreateUser(t, e.store, name+"-"+uuid.NewString()[:8]+"@example.com")
}

func (e *env) addMember(t *testing.T, boardID, userID uuid.UUID, role domain.Role) {
	t.Helper()
	_, err := e.pool.Exec(context.Background(),
		`INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, $3)`, boardID, userID, string(role))
	require.NoError(t, err)
}

type boardRow struct {
	title     string
	seq       int64
	deleted   bool
	eventsCnt int
}

func (e *env) row(t *testing.T, id uuid.UUID) boardRow {
	t.Helper()
	var r boardRow
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT title, seq, deleted_at IS NOT NULL FROM boards WHERE id = $1`, id).Scan(&r.title, &r.seq, &r.deleted))
	require.NoError(t, e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM board_events WHERE board_id = $1`, id).Scan(&r.eventsCnt))
	return r
}

type eventRow struct {
	seq     int64
	typ     string
	payload string
	actor   *uuid.UUID
}

func (e *env) events(t *testing.T, id uuid.UUID) []eventRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT seq, type, payload::text, actor_id FROM board_events WHERE board_id = $1 ORDER BY seq`, id)
	require.NoError(t, err)
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var r eventRow
		require.NoError(t, rows.Scan(&r.seq, &r.typ, &r.payload, &r.actor))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

func TestCreateBoard(t *testing.T) {
	e := newEnv(t)
	owner := e.user(t, "owner")

	b, err := e.svc.Create(context.Background(), owner.ID, "  Product roadmap  ")
	require.NoError(t, err)
	assert.Equal(t, "Product roadmap", b.Title)
	assert.Equal(t, owner.ID, b.OwnerID)
	assert.Equal(t, domain.RoleOwner, b.Role)
	assert.Zero(t, b.Seq)
	assert.NotEqual(t, uuid.Nil, b.ID)

	assert.Equal(t, boardRow{title: "Product roadmap"}, e.row(t, b.ID))
	assert.Empty(t, e.events(t, b.ID))
}

func TestCreateBoardValidationAndUnknownOwner(t *testing.T) {
	e := newEnv(t)
	owner := e.user(t, "owner")

	for _, title := range []string{"", "   ", strings.Repeat("x", 201)} {
		_, err := e.svc.Create(context.Background(), owner.ID, title)
		var vErr *domain.ValidationError
		require.ErrorAs(t, err, &vErr, title)
		assert.Equal(t, "title", vErr.Field)
	}

	_, err := e.svc.Create(context.Background(), uuid.New(), "Ghost board")
	assert.Error(t, err)

	list, err := e.svc.List(context.Background(), owner.ID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

func TestListBoards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	alice, bob := e.user(t, "alice"), e.user(t, "bob")

	a1, err := e.svc.Create(ctx, alice.ID, "A1")
	require.NoError(t, err)
	a2, err := e.svc.Create(ctx, alice.ID, "A2")
	require.NoError(t, err)
	b1, err := e.svc.Create(ctx, bob.ID, "B1")
	require.NoError(t, err)
	e.addMember(t, b1.ID, alice.ID, domain.RoleEditor)

	list, err := e.svc.List(ctx, alice.ID)
	require.NoError(t, err)
	got := map[uuid.UUID]domain.Role{}
	for _, b := range list {
		got[b.ID] = b.Role
	}
	assert.Equal(t, map[uuid.UUID]domain.Role{a1.ID: domain.RoleOwner, a2.ID: domain.RoleOwner, b1.ID: domain.RoleEditor}, got)

	bobList, err := e.svc.List(ctx, bob.ID)
	require.NoError(t, err)
	require.Len(t, bobList, 1)
	assert.Equal(t, domain.RoleOwner, bobList[0].Role)

	_, err = e.svc.Delete(ctx, alice.ID, a1.ID)
	require.NoError(t, err)
	list, err = e.svc.List(ctx, alice.ID)
	require.NoError(t, err)
	assert.Len(t, list, 2)
}

func TestRenameBoardByOwner(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, "owner")
	b, err := e.svc.Create(ctx, owner.ID, "Old")
	require.NoError(t, err)

	updated, ev, err := e.svc.Rename(ctx, owner.ID, b.ID, "  New title ")
	require.NoError(t, err)
	assert.Equal(t, "New title", updated.Title)
	assert.Equal(t, int64(1), updated.Seq)
	assert.Equal(t, domain.RoleOwner, updated.Role)
	assert.True(t, updated.UpdatedAt.After(b.UpdatedAt) || updated.UpdatedAt.Equal(b.UpdatedAt))

	assert.Equal(t, b.ID, ev.BoardID)
	assert.Equal(t, int64(1), ev.Seq)
	assert.Equal(t, domain.EventBoardRenamed, ev.Type)
	assert.JSONEq(t, `{"title":"New title","actor_id":"`+owner.ID.String()+`"}`, string(ev.Payload))
	require.NotNil(t, ev.ActorID)
	assert.Equal(t, owner.ID, *ev.ActorID)

	_, ev2, err := e.svc.Rename(ctx, owner.ID, b.ID, "Third")
	require.NoError(t, err)
	assert.Equal(t, int64(2), ev2.Seq)

	assert.Equal(t, boardRow{title: "Third", seq: 2, eventsCnt: 2}, e.row(t, b.ID))
	evs := e.events(t, b.ID)
	require.Len(t, evs, 2)
	assert.Equal(t, domain.EventBoardRenamed, evs[0].typ)
	assert.JSONEq(t, `{"title":"New title","actor_id":"`+owner.ID.String()+`"}`, evs[0].payload)
	assert.JSONEq(t, `{"title":"Third","actor_id":"`+owner.ID.String()+`"}`, evs[1].payload)
	require.NotNil(t, evs[1].actor)
	assert.Equal(t, owner.ID, *evs[1].actor)

	list, err := e.svc.List(ctx, owner.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "Third", list[0].Title)
	assert.Equal(t, int64(2), list[0].Seq)
}

func TestRenameValidationDoesNotTouchBoard(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, "owner")
	b, err := e.svc.Create(ctx, owner.ID, "Keep")
	require.NoError(t, err)

	for _, title := range []string{"", "  ", strings.Repeat("y", 201)} {
		_, _, err := e.svc.Rename(ctx, owner.ID, b.ID, title)
		var vErr *domain.ValidationError
		require.ErrorAs(t, err, &vErr)
	}
	assert.Equal(t, boardRow{title: "Keep"}, e.row(t, b.ID))
}

func TestOwnerOnlyPermissions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, editor, viewer, stranger := e.user(t, "owner"), e.user(t, "editor"), e.user(t, "viewer"), e.user(t, "stranger")
	b, err := e.svc.Create(ctx, owner.ID, "Locked")
	require.NoError(t, err)
	e.addMember(t, b.ID, editor.ID, domain.RoleEditor)
	e.addMember(t, b.ID, viewer.ID, domain.RoleViewer)

	tests := []struct {
		name    string
		user    uuid.UUID
		wantErr error
	}{
		{"editor", editor.ID, domain.ErrForbidden},
		{"viewer", viewer.ID, domain.ErrForbidden},
		{"non-member", stranger.ID, domain.ErrNotFound},
		{"unknown user", uuid.New(), domain.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run("rename "+tt.name, func(t *testing.T) {
			_, _, err := e.svc.Rename(ctx, tt.user, b.ID, "Hacked")
			assert.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, boardRow{title: "Locked"}, e.row(t, b.ID), "rejected rename must not change title, seq or events")
		})
		t.Run("delete "+tt.name, func(t *testing.T) {
			_, err := e.svc.Delete(ctx, tt.user, b.ID)
			assert.ErrorIs(t, err, tt.wantErr)
			assert.Equal(t, boardRow{title: "Locked"}, e.row(t, b.ID), "rejected delete must not soft-delete or add events")
		})
	}

	list, err := e.svc.List(ctx, owner.ID)
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestRenameAndDeleteUnknownBoard(t *testing.T) {
	e := newEnv(t)
	owner := e.user(t, "owner")
	_, _, err := e.svc.Rename(context.Background(), owner.ID, uuid.New(), "x")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = e.svc.Delete(context.Background(), owner.ID, uuid.New())
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestDeleteBoardIsSoft(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, "owner")
	b, err := e.svc.Create(ctx, owner.ID, "Doomed")
	require.NoError(t, err)
	_, _, err = e.svc.Rename(ctx, owner.ID, b.ID, "Doomed 2")
	require.NoError(t, err)

	ev, err := e.svc.Delete(ctx, owner.ID, b.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(2), ev.Seq)
	assert.Equal(t, domain.EventBoardDeleted, ev.Type)
	assert.JSONEq(t, `{"actor_id":"`+owner.ID.String()+`"}`, string(ev.Payload))
	require.NotNil(t, ev.ActorID)
	assert.Equal(t, owner.ID, *ev.ActorID)

	assert.Equal(t, boardRow{title: "Doomed 2", seq: 2, deleted: true, eventsCnt: 2}, e.row(t, b.ID))
	var members int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM board_members WHERE board_id = $1`, b.ID).Scan(&members))
	assert.Equal(t, 1, members, "soft delete keeps the board rows")

	list, err := e.svc.List(ctx, owner.ID)
	require.NoError(t, err)
	assert.Empty(t, list)

	_, _, err = e.svc.Rename(ctx, owner.ID, b.ID, "Zombie")
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = e.svc.Delete(ctx, owner.ID, b.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	other := e.user(t, "other")
	_, _, err = e.svc.Rename(ctx, other.ID, b.ID, "Zombie")
	assert.ErrorIs(t, err, domain.ErrNotFound, "deleted boards look the same to everyone")

	assert.Equal(t, boardRow{title: "Doomed 2", seq: 2, deleted: true, eventsCnt: 2}, e.row(t, b.ID))
}

func TestConcurrentRenamesProduceGapFreeSeq(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, "owner")
	b, err := e.svc.Create(ctx, owner.ID, "Race")
	require.NoError(t, err)

	const workers = 40
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]domain.Event, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i], errs[i] = e.svc.Rename(ctx, owner.ID, b.ID, "title-"+uuid.NewString())
		}()
	}
	close(start)
	wg.Wait()

	seqs := make([]int, 0, workers)
	for i := range workers {
		require.NoError(t, errs[i])
		seqs = append(seqs, int(results[i].Seq))
	}
	sort.Ints(seqs)
	for i, s := range seqs {
		require.Equal(t, i+1, s, "returned seq values must be exactly 1..N with no gaps or duplicates")
	}

	evs := e.events(t, b.ID)
	require.Len(t, evs, workers)
	for i, ev := range evs {
		assert.Equal(t, int64(i+1), ev.seq)
		assert.Equal(t, domain.EventBoardRenamed, ev.typ)
	}
	row := e.row(t, b.ID)
	assert.Equal(t, int64(workers), row.seq)
	assert.Equal(t, workers, row.eventsCnt)

	assert.JSONEq(t, `{"title":"`+row.title+`","actor_id":"`+owner.ID.String()+`"}`, evs[workers-1].payload, "final title must come from the event with the highest seq")
}

func TestConcurrentRenamesAcrossBoardsAreIndependent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, "owner")
	const boardsN, perBoard = 4, 10
	ids := make([]uuid.UUID, boardsN)
	for i := range ids {
		b, err := e.svc.Create(ctx, owner.ID, "B")
		require.NoError(t, err)
		ids[i] = b.ID
	}

	var wg sync.WaitGroup
	errs := make(chan error, boardsN*perBoard)
	for _, id := range ids {
		for range perBoard {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _, err := e.svc.Rename(ctx, owner.ID, id, "t-"+uuid.NewString())
				errs <- err
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	for _, id := range ids {
		row := e.row(t, id)
		assert.Equal(t, int64(perBoard), row.seq)
		assert.Equal(t, perBoard, row.eventsCnt)
	}
}

func TestConcurrentRenameAndDelete(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, "owner")
	b, err := e.svc.Create(ctx, owner.ID, "Racy")
	require.NoError(t, err)

	const renamers = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, renamers)
	var deleteErr error
	for i := range renamers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, errs[i] = e.svc.Rename(ctx, owner.ID, b.ID, "r-"+uuid.NewString())
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, deleteErr = e.svc.Delete(ctx, owner.ID, b.ID)
	}()
	close(start)
	wg.Wait()

	require.NoError(t, deleteErr)
	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		assert.ErrorIs(t, err, domain.ErrNotFound, "a rename that loses to delete must report not found")
	}

	evs := e.events(t, b.ID)
	require.Len(t, evs, succeeded+1)
	for i, ev := range evs {
		assert.Equal(t, int64(i+1), ev.seq)
	}
	assert.Equal(t, domain.EventBoardDeleted, evs[len(evs)-1].typ, "no event may follow board.deleted")
	row := e.row(t, b.ID)
	assert.True(t, row.deleted)
	assert.Equal(t, int64(succeeded+1), row.seq)
}

func TestRenameAndDeletePublishAfterCommit(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	var (
		mu        sync.Mutex
		published []domain.Event
	)
	svc := boards.NewService(store, boards.WithPublisher(pubsub.PublisherFunc(func(ctx context.Context, events []domain.Event) error {
		mu.Lock()
		defer mu.Unlock()
		for _, ev := range events {
			var seq int64
			require.NoError(t, pool.QueryRow(ctx, `SELECT seq FROM boards WHERE id = $1`, ev.BoardID).Scan(&seq))
			assert.GreaterOrEqual(t, seq, ev.Seq, "event must be committed before it is published")
		}
		published = append(published, events...)
		return errors.New("redis down")
	})))
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("pub"))
	b, err := svc.Create(ctx, owner.ID, "Pub")
	require.NoError(t, err)

	_, _, err = svc.Rename(ctx, owner.ID, b.ID, "Renamed")
	require.NoError(t, err, "publish failures do not fail the committed rename")
	_, _, err = svc.Rename(ctx, uuid.New(), b.ID, "Nope")
	require.Error(t, err)
	_, err = svc.Delete(ctx, owner.ID, b.ID)
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, published, 2)
	assert.Equal(t, domain.EventBoardRenamed, published[0].Type)
	assert.Equal(t, domain.EventBoardDeleted, published[1].Type)
}

func TestSnapshotAndEventsRequireMembership(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner := e.user(t, "owner")
	viewer := e.user(t, "viewer")
	outsider := e.user(t, "outsider")
	b, err := e.svc.Create(ctx, owner.ID, "Snap")
	require.NoError(t, err)
	e.addMember(t, b.ID, viewer.ID, domain.RoleViewer)
	_, _, err = e.svc.Rename(ctx, owner.ID, b.ID, "Snap 2")
	require.NoError(t, err)

	snap, err := e.svc.Snapshot(ctx, viewer.ID, b.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.RoleViewer, snap.Board.Role)
	assert.Equal(t, "Snap 2", snap.Board.Title)
	assert.Equal(t, int64(1), snap.Seq)
	assert.Len(t, snap.Members, 2)
	assert.Empty(t, snap.Columns)
	assert.Empty(t, snap.Cards)

	page, err := e.svc.Events(ctx, viewer.ID, b.ID, 0)
	require.NoError(t, err)
	require.Len(t, page.Events, 1)
	assert.Equal(t, domain.EventBoardRenamed, page.Events[0].Type)

	_, err = e.svc.Events(ctx, viewer.ID, b.ID, -1)
	var vErr *domain.ValidationError
	assert.ErrorAs(t, err, &vErr)
	_, err = e.svc.Snapshot(ctx, outsider.ID, b.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	_, err = e.svc.Events(ctx, outsider.ID, b.ID, 0)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}
