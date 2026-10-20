package storage_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestAppendEventTimestampsFollowSeqWithinTransaction(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("clock"))
	b := createBoard(t, store, owner.ID, "Clock")

	var events []domain.Event
	err := store.WithBoardLock(context.Background(), b.ID, func(ctx context.Context, btx *storage.BoardTx) error {
		for range 3 {
			ev, err := btx.AppendEvent(ctx, domain.EventDraft{Type: "t.tick"})
			if err != nil {
				return err
			}
			events = append(events, ev)
			time.Sleep(2 * time.Millisecond)
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 3)
	for i := 1; i < len(events); i++ {
		assert.Greater(t, events[i].Seq, events[i-1].Seq)
		assert.True(t, events[i].CreatedAt.After(events[i-1].CreatedAt),
			"created_at must advance with seq inside one transaction: %v then %v", events[i-1].CreatedAt, events[i].CreatedAt)
	}
}
