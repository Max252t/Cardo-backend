package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func createBoard(t *testing.T, store *storage.Store, owner uuid.UUID, title string) domain.BoardWithRole {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)
	b, err := store.CreateBoardWithOwner(context.Background(), domain.Board{ID: id, Title: title, OwnerID: owner})
	require.NoError(t, err)
	return b
}

func boardSeq(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (seq int64, events int) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT seq FROM boards WHERE id = $1`, id).Scan(&seq))
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM board_events WHERE board_id = $1`, id).Scan(&events))
	return seq, events
}

func TestCreateBoardWithOwner(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, "owner@example.com")

	b := createBoard(t, store, owner.ID, "Roadmap")
	assert.Equal(t, "Roadmap", b.Title)
	assert.Equal(t, owner.ID, b.OwnerID)
	assert.Equal(t, domain.RoleOwner, b.Role)
	assert.Zero(t, b.Seq)
	assert.Nil(t, b.DeletedAt)
	assert.False(t, b.CreatedAt.IsZero())

	var role string
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT role FROM board_members WHERE board_id = $1 AND user_id = $2`, b.ID, owner.ID).Scan(&role))
	assert.Equal(t, "owner", role)
}

func TestCreateBoardWithUnknownOwnerRollsBack(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	id, err := uuid.NewV7()
	require.NoError(t, err)

	_, err = store.CreateBoardWithOwner(context.Background(), domain.Board{ID: id, Title: "Orphan", OwnerID: uuid.New()})
	require.Error(t, err)

	var n int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM boards`).Scan(&n))
	assert.Zero(t, n)
}

func TestListBoardsForUser(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	alice := testutil.CreateUser(t, store, "alice@example.com")
	bob := testutil.CreateUser(t, store, "bob@example.com")

	empty, err := store.ListBoardsForUser(ctx, alice.ID)
	require.NoError(t, err)
	require.NotNil(t, empty)
	assert.Empty(t, empty)

	first := createBoard(t, store, alice.ID, "First")
	second := createBoard(t, store, alice.ID, "Second")
	bobs := createBoard(t, store, bob.ID, "Bob's")
	deleted := createBoard(t, store, alice.ID, "Deleted")
	_, err = pool.Exec(ctx, `UPDATE boards SET deleted_at = now() WHERE id = $1`, deleted.ID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'viewer')`, bobs.ID, alice.ID)
	require.NoError(t, err)

	list, err := store.ListBoardsForUser(ctx, alice.ID)
	require.NoError(t, err)
	require.Len(t, list, 3)

	roles := map[uuid.UUID]domain.Role{}
	titles := map[uuid.UUID]string{}
	for _, b := range list {
		roles[b.ID] = b.Role
		titles[b.ID] = b.Title
	}
	assert.Equal(t, domain.RoleOwner, roles[first.ID])
	assert.Equal(t, domain.RoleOwner, roles[second.ID])
	assert.Equal(t, domain.RoleViewer, roles[bobs.ID])
	assert.NotContains(t, titles, deleted.ID)

	assert.Equal(t, []uuid.UUID{bobs.ID, second.ID, first.ID}, []uuid.UUID{list[0].ID, list[1].ID, list[2].ID}, "newest first")

	bobList, err := store.ListBoardsForUser(ctx, bob.ID)
	require.NoError(t, err)
	require.Len(t, bobList, 1)
	assert.Equal(t, bobs.ID, bobList[0].ID)
}

func TestWithBoardLockNotFound(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, "lock@example.com")
	b := createBoard(t, store, owner.ID, "Gone")

	called := false
	noop := func(context.Context, *storage.BoardTx) error { called = true; return nil }

	err := store.WithBoardLock(ctx, uuid.New(), noop)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	_, err = pool.Exec(ctx, `UPDATE boards SET deleted_at = now() WHERE id = $1`, b.ID)
	require.NoError(t, err)
	err = store.WithBoardLock(ctx, b.ID, noop)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.False(t, called)
}

func TestBoardTxRoles(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, "o@example.com")
	editor := testutil.CreateUser(t, store, "e@example.com")
	viewer := testutil.CreateUser(t, store, "v@example.com")
	stranger := testutil.CreateUser(t, store, "s@example.com")
	b := createBoard(t, store, owner.ID, "Roles")
	_, err := pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'editor'), ($1, $3, 'viewer')`, b.ID, editor.ID, viewer.ID)
	require.NoError(t, err)

	tests := []struct {
		name     string
		user     uuid.UUID
		min      domain.Role
		wantRole domain.Role
		wantErr  error
	}{
		{"owner as owner", owner.ID, domain.RoleOwner, domain.RoleOwner, nil},
		{"owner as viewer", owner.ID, domain.RoleViewer, domain.RoleOwner, nil},
		{"editor as editor", editor.ID, domain.RoleEditor, domain.RoleEditor, nil},
		{"editor as owner", editor.ID, domain.RoleOwner, domain.RoleEditor, domain.ErrForbidden},
		{"viewer as viewer", viewer.ID, domain.RoleViewer, domain.RoleViewer, nil},
		{"viewer as editor", viewer.ID, domain.RoleEditor, domain.RoleViewer, domain.ErrForbidden},
		{"stranger", stranger.ID, domain.RoleViewer, "", domain.ErrNotFound},
		{"unknown user", uuid.New(), domain.RoleViewer, "", domain.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := store.WithBoardLock(ctx, b.ID, func(ctx context.Context, btx *storage.BoardTx) error {
				role, err := btx.RequireRole(ctx, tt.user, tt.min)
				assert.Equal(t, tt.wantRole, role)
				return err
			})
			if tt.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}

func TestAppendEventAssignsSequentialSeq(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, "events@example.com")
	b := createBoard(t, store, owner.ID, "Events")
	opID := uuid.New()

	var events []domain.Event
	err := store.WithBoardLock(ctx, b.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		for i, in := range []domain.EventDraft{
			{Type: "t.nil", Payload: nil},
			{Type: "t.raw", Payload: json.RawMessage(`{"a":1}`), OpID: &opID, ActorID: &owner.ID},
			{Type: "t.empty-raw", Payload: json.RawMessage{}},
			{Type: "t.struct", Payload: map[string]any{"k": "v"}},
		} {
			ev, err := btx.AppendEvent(ctx, in)
			if err != nil {
				return err
			}
			events = append(events, ev)
			assert.Equal(t, int64(i+1), ev.Seq)
			assert.Equal(t, int64(i+1), btx.Board().Seq)
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 4)

	seq, count := boardSeq(t, pool, b.ID)
	assert.Equal(t, int64(4), seq)
	assert.Equal(t, 4, count)

	rows, err := pool.Query(ctx, `SELECT seq, type, payload::text, op_id, actor_id FROM board_events WHERE board_id = $1 ORDER BY seq`, b.ID)
	require.NoError(t, err)
	defer rows.Close()
	type row struct {
		seq     int64
		typ     string
		payload string
		opID    *uuid.UUID
		actorID *uuid.UUID
	}
	var got []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.seq, &r.typ, &r.payload, &r.opID, &r.actorID))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 4)
	assert.JSONEq(t, `{}`, got[0].payload)
	assert.JSONEq(t, `{"a":1}`, got[1].payload)
	require.NotNil(t, got[1].opID)
	assert.Equal(t, opID, *got[1].opID)
	require.NotNil(t, got[1].actorID)
	assert.Equal(t, owner.ID, *got[1].actorID)
	assert.JSONEq(t, `{}`, got[2].payload)
	assert.JSONEq(t, `{"k":"v"}`, got[3].payload)
	assert.Nil(t, got[0].opID)
	assert.Nil(t, got[0].actorID)
}

func TestAppendEventRejectsEmptyType(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, "empty-type@example.com")
	b := createBoard(t, store, owner.ID, "T")

	err := store.WithBoardLock(context.Background(), b.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		_, err := btx.AppendEvent(ctx, domain.EventDraft{Payload: nil})
		return err
	})
	require.Error(t, err)
	seq, count := boardSeq(t, pool, b.ID)
	assert.Zero(t, seq)
	assert.Zero(t, count)
}

func TestWithBoardLockRollsBackOnError(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	owner := testutil.CreateUser(t, store, "rollback@example.com")
	b := createBoard(t, store, owner.ID, "Original")
	boom := errors.New("boom")

	err := store.WithBoardLock(ctx, b.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		if err := btx.SetTitle(ctx, "Changed"); err != nil {
			return err
		}
		if _, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "t.x"}); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	seq, count := boardSeq(t, pool, b.ID)
	assert.Zero(t, seq, "seq increment must roll back with the transaction")
	assert.Zero(t, count)
	var title string
	require.NoError(t, pool.QueryRow(ctx, `SELECT title FROM boards WHERE id = $1`, b.ID).Scan(&title))
	assert.Equal(t, "Original", title)
}

func TestAppendEventUnmarshalablePayload(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, "badpayload@example.com")
	b := createBoard(t, store, owner.ID, "T")

	err := store.WithBoardLock(context.Background(), b.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		_, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "t.bad", Payload: make(chan int)})
		return err
	})
	require.Error(t, err)
	seq, _ := boardSeq(t, pool, b.ID)
	assert.Zero(t, seq, "seq must not advance when the payload cannot be encoded")
}
