package main

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func worldEvent(t *testing.T, typ string, payload any) protocol.Envelope {
	t.Helper()
	return envelope(t, typ, 1, nil, payload)
}

func seededWorld(t *testing.T, columns int) (*world, []uuid.UUID) {
	t.Helper()
	w := newWorld()
	ids := make([]uuid.UUID, columns)
	for i := range ids {
		ids[i] = uuid.New()
		w.apply(worldEvent(t, boardcontent.EventColumnCreated, boardcontent.ColumnCreated{Column: boardcontent.Column{ID: ids[i]}}))
	}
	return w, ids
}

func TestWorldTracksColumnsCardsAndVersions(t *testing.T) {
	w, cols := seededWorld(t, 2)
	assert.Equal(t, 2, w.columnCount())
	card := uuid.New()
	w.apply(worldEvent(t, boardcontent.EventCardCreated, boardcontent.CardCreated{Card: boardcontent.Card{ID: card, ColumnID: cols[0], Version: 1}}))
	require.Len(t, w.cards, 1)
	w.apply(worldEvent(t, boardcontent.EventCardEdited, boardcontent.CardEdited{CardID: card, Version: 3}))
	assert.Equal(t, int64(3), w.index[card].version)
	w.apply(worldEvent(t, boardcontent.EventCardMoved, boardcontent.CardMoved{CardID: card, ToColumnID: cols[1], Version: 4}))
	assert.Equal(t, int64(4), w.index[card].version)
	w.apply(worldEvent(t, boardcontent.EventCardEdited, boardcontent.CardEdited{CardID: uuid.New(), Version: 9}))
	assert.Len(t, w.index, 1)
}

func TestWorldIgnoresMalformedAndUnrelatedEvents(t *testing.T) {
	w := newWorld()
	for _, typ := range []string{boardcontent.EventColumnCreated, boardcontent.EventCardCreated, boardcontent.EventCardEdited, boardcontent.EventCardMoved} {
		w.apply(worldEvent(t, typ, []int{1}))
	}
	w.apply(worldEvent(t, "board.renamed", map[string]string{}))
	assert.Zero(t, w.columnCount())
	assert.Empty(t, w.cards)
}

func TestWorkloadAlwaysTargetsKnownColumnsAndCards(t *testing.T) {
	w, cols := seededWorld(t, 3)
	g := newWorkload(w, 11)
	known := map[uuid.UUID]bool{}
	for _, c := range cols {
		known[c] = true
	}
	kinds := map[string]int{}
	for range 600 {
		op := g.next()
		kinds[op.opType]++
		switch p := op.payload.(type) {
		case boardcontent.CardCreate:
			assert.True(t, known[p.ColumnID])
			w.apply(worldEvent(t, boardcontent.EventCardCreated, boardcontent.CardCreated{Card: boardcontent.Card{ID: p.CardID, ColumnID: p.ColumnID, Version: 1}}))
		case boardcontent.CardEdit:
			assert.NotNil(t, w.index[p.CardID])
			assert.NotNil(t, p.Title)
		case boardcontent.CardMove:
			assert.True(t, known[p.ToColumnID])
			assert.NotNil(t, p.BaseVersion)
			assert.NotNil(t, w.index[p.CardID])
		}
		assert.NotEqual(t, uuid.Nil, op.id)
	}
	assert.Positive(t, kinds[boardcontent.OpCardCreate])
	assert.Positive(t, kinds[boardcontent.OpCardEdit])
}

func TestWorkloadDoesNotMoveFreshCardsAndBumpsTheLocalVersion(t *testing.T) {
	w, _ := seededWorld(t, 1)
	card := uuid.New()
	w.apply(worldEvent(t, boardcontent.EventCardCreated, boardcontent.CardCreated{Card: boardcontent.Card{ID: card, Version: 1}}))
	g := newWorkload(w, 5)
	for range 400 {
		assert.NotEqual(t, boardcontent.OpCardMove, g.next().opType, "cards younger than %s must not be moved", minCardAgeForMove)
	}
	w.index[card].created = time.Now().Add(-time.Minute)
	moves := 0
	for range 400 {
		op := g.next()
		if op.opType != boardcontent.OpCardMove {
			continue
		}
		moves++
		assert.Equal(t, w.index[card].version-1, *op.payload.(boardcontent.CardMove).BaseVersion)
	}
	assert.Positive(t, moves)
}

func TestWorkloadIsDeterministicForASeed(t *testing.T) {
	collect := func(seed int64) []string {
		w, _ := seededWorld(t, 1)
		g := newWorkload(w, seed)
		var kinds []string
		for range 50 {
			op := g.next()
			kinds = append(kinds, op.opType)
			if p, ok := op.payload.(boardcontent.CardCreate); ok {
				w.apply(worldEvent(t, boardcontent.EventCardCreated, boardcontent.CardCreated{Card: boardcontent.Card{ID: p.CardID, ColumnID: p.ColumnID, Version: 1}}))
			}
		}
		return kinds
	}
	assert.Equal(t, collect(3), collect(3))
}

func TestWorkloadAndWorldAreSafeForConcurrentUse(t *testing.T) {
	w, _ := seededWorld(t, 4)
	g := newWorkload(w, 1)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 300 {
				op := g.next()
				if p, ok := op.payload.(boardcontent.CardCreate); ok {
					w.apply(worldEvent(t, boardcontent.EventCardCreated, boardcontent.CardCreated{Card: boardcontent.Card{ID: p.CardID, ColumnID: p.ColumnID, Version: 1}}))
				}
				w.columnCount()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 4, w.columnCount())
}
