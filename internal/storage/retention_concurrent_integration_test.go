package storage_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestPruneKeepsSeqContiguousWhileTheBoardReceivesWrites(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("race"))
	b := createBoard(t, store, owner.ID, "busy")
	appendEvents(t, store, b.ID, 300)
	ageEvents(t, pool, b.ID, 300, 9*24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writers, readers sync.WaitGroup
	var appended atomic.Int64

	writers.Add(1)
	go func() {
		defer writers.Done()
		for range 150 {
			appendEvents(t, store, b.ID, 1)
			appended.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()

	var gaps atomic.Int64
	readers.Add(1)
	go func() {
		defer readers.Done()
		for after := int64(0); ctx.Err() == nil; after = (after + 37) % 300 {
			page, err := store.EventsAfter(ctx, b.ID, owner.ID, after, 1000)
			if errors.Is(err, domain.ErrGone) || err != nil {
				continue
			}
			for i, ev := range page.Events {
				if ev.Seq != after+1+int64(i) {
					gaps.Add(1)
					break
				}
			}
		}
	}()

	for range 12 {
		_, err := store.PruneEvents(context.Background(), time.Now().Add(-7*24*time.Hour), 50, 10)
		require.NoError(t, err)
	}
	writers.Wait()
	cancel()
	readers.Wait()

	assert.Zero(t, gaps.Load(), "the event log must never return a page with a gap")
	seqs := retainedSeqs(t, pool, b.ID)
	require.NotEmpty(t, seqs)
	last, _ := boardSeq(t, pool, b.ID)
	assert.Equal(t, seqSpan(seqs[0], last), seqs, "retained events form one contiguous suffix")
	assert.GreaterOrEqual(t, len(seqs), 50)
}
