package main

import (
	"encoding/json"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const minCardAgeForMove = 1500 * time.Millisecond

type cardInfo struct {
	id      uuid.UUID
	version int64
	created time.Time
}

type world struct {
	mu      sync.Mutex
	columns []uuid.UUID
	cards   []*cardInfo
	index   map[uuid.UUID]*cardInfo
}

func newWorld() *world {
	return &world{index: make(map[uuid.UUID]*cardInfo)}
}

func (w *world) apply(env protocol.Envelope) {
	switch env.Type {
	case boardcontent.EventColumnCreated:
		var p boardcontent.ColumnCreated
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		w.mu.Lock()
		w.columns = append(w.columns, p.ID)
		w.mu.Unlock()
	case boardcontent.EventCardCreated:
		var p boardcontent.CardCreated
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		w.mu.Lock()
		info := &cardInfo{id: p.ID, version: p.Version, created: time.Now()}
		w.cards = append(w.cards, info)
		w.index[p.ID] = info
		w.mu.Unlock()
	case boardcontent.EventCardEdited:
		var p boardcontent.CardEdited
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		w.setVersion(p.CardID, p.Version)
	case boardcontent.EventCardMoved:
		var p boardcontent.CardMoved
		if json.Unmarshal(env.Payload, &p) != nil {
			return
		}
		w.setVersion(p.CardID, p.Version)
	}
}

func (w *world) setVersion(id uuid.UUID, version int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if info, ok := w.index[id]; ok {
		info.version = version
	}
}

func (w *world) columnCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.columns)
}

type workload struct {
	world   *world
	rng     *rand.Rand
	mu      sync.Mutex
	counter int
}

func newWorkload(w *world, seed int64) *workload {
	return &workload{world: w, rng: rand.New(rand.NewSource(seed))}
}

type plannedOp struct {
	opType  string
	id      uuid.UUID
	payload any
}

func (g *workload) next() plannedOp {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.counter++
	w := g.world
	w.mu.Lock()
	defer w.mu.Unlock()
	column := w.columns[g.rng.Intn(len(w.columns))]
	roll := g.rng.Intn(100)
	if roll >= 45 && len(w.cards) > 0 {
		card := w.cards[g.rng.Intn(len(w.cards))]
		if roll < 80 {
			title := g.title()
			return plannedOp{opType: boardcontent.OpCardEdit, id: uuid.New(), payload: boardcontent.CardEdit{CardID: card.id, Title: &title}}
		}
		if time.Since(card.created) > minCardAgeForMove {
			version := card.version
			card.version++
			return plannedOp{opType: boardcontent.OpCardMove, id: uuid.New(), payload: boardcontent.CardMove{
				CardID:      card.id,
				ToColumnID:  column,
				BaseVersion: &version,
			}}
		}
	}
	return plannedOp{opType: boardcontent.OpCardCreate, id: uuid.New(), payload: boardcontent.CardCreate{
		CardID:   uuid.New(),
		ColumnID: column,
		Title:    g.title(),
	}}
}

func (g *workload) title() string {
	return "load card " + uuid.NewString()[:8]
}
