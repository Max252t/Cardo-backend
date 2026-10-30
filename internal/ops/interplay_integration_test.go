package ops_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

func TestBoardRenamesAndOperationsShareOneGapFreeSequence(t *testing.T) {
	f := newConcurrentFixture(t)
	svc := boards.NewService(f.store)
	col := f.column(t, "Todo", nil)

	const renames = 10
	const creates = 20
	var wg sync.WaitGroup
	errs := make(chan error, renames+creates)
	for i := range renames {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Rename(context.Background(), f.owner.ID, f.board.ID, fmt.Sprintf("title-%d", i))
			errs <- err
		}()
	}
	for i := range creates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op := mustOp(t, boardcontent.OpCardCreate, uuid.New(), boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: fmt.Sprintf("c%d", i)})
			out, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
			if err == nil && out.Nack != nil {
				err = fmt.Errorf("unexpected nack: %+v", out.Nack)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	total := f.requireGapFreeSeq(t)
	assert.Equal(t, int64(1+renames+creates), total)

	counts := map[string]int{}
	for _, ev := range f.eventsAfter(t, 0) {
		counts[ev.Type]++
	}
	assert.Equal(t, map[string]int{boardcontent.EventColumnCreated: 1, boardcontent.EventBoardRenamed: renames, boardcontent.EventCardCreated: creates}, counts)
}

func TestOperationsAfterBoardDeletionAreNotFound(t *testing.T) {
	f := newFixture(t)
	svc := boards.NewService(f.store)
	col := f.column(t, "Todo", nil)
	card := f.card(t, col, "A")

	_, err := svc.Delete(context.Background(), f.owner.ID, f.board.ID)
	require.NoError(t, err)
	seq := f.boardSeq(t)

	for _, tc := range []struct {
		opType  string
		payload any
	}{
		{boardcontent.OpCardEdit, boardcontent.CardEdit{CardID: card, Title: ptr("x")}},
		{boardcontent.OpCardArchive, boardcontent.CardArchive{CardID: card}},
		{boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "x"}},
	} {
		op := mustOp(t, tc.opType, uuid.New(), tc.payload)
		_, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
		assert.ErrorIs(t, err, domain.ErrNotFound, tc.opType)
	}
	f.requireUnchanged(t, seq)

	_, err = f.store.Snapshot(context.Background(), f.board.ID, f.owner.ID)
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestConcurrentOperationsAndBoardDeletionNeverWriteAfterTheDeleteEvent(t *testing.T) {
	f := newConcurrentFixture(t)
	svc := boards.NewService(f.store)
	col := f.column(t, "Todo", nil)

	const creators = 20
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, creators)
	nacks := make([]bool, creators)
	for i := range creators {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			op := mustOp(t, boardcontent.OpCardCreate, uuid.New(), boardcontent.CardCreate{CardID: uuid.New(), ColumnID: col, Title: fmt.Sprintf("c%d", i)})
			out, err := f.proc.Apply(context.Background(), f.owner.ID, f.board.ID, op)
			results[i] = err
			nacks[i] = out.Nack != nil
		}()
	}
	var deleteErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, deleteErr = svc.Delete(context.Background(), f.owner.ID, f.board.ID)
	}()
	close(start)
	wg.Wait()

	require.NoError(t, deleteErr)
	succeeded := 0
	for i, err := range results {
		assert.False(t, nacks[i])
		if err == nil {
			succeeded++
			continue
		}
		assert.ErrorIs(t, err, domain.ErrNotFound)
	}

	events := f.eventsAfter(t, 0)
	require.Len(t, events, 2+succeeded)
	assert.Equal(t, boardcontent.EventBoardDeleted, events[len(events)-1].Type)
	f.requireGapFreeSeq(t)
}
