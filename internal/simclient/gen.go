package simclient

import (
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
)

type Profile struct {
	HotSpot    float64
	MaxColumns int
}

type generator struct {
	rng     *rand.Rand
	name    string
	profile Profile
	counter int
}

func (g *generator) newID() uuid.UUID {
	var id uuid.UUID
	binary.BigEndian.PutUint64(id[:8], g.rng.Uint64())
	binary.BigEndian.PutUint64(id[8:], g.rng.Uint64())
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id
}

func (g *generator) text(prefix string) string {
	g.counter++
	return fmt.Sprintf("%s %s-%d", prefix, g.name, g.counter)
}

func (g *generator) description() string {
	g.counter++
	return fmt.Sprintf("desc %s-%d %d", g.name, g.counter, g.rng.Intn(1000))
}

func (g *generator) anchors(ids []uuid.UUID) (after, before *uuid.UUID) {
	if g.rng.Intn(100) < 3 {
		stale := g.newID()
		return &stale, nil
	}
	pos := g.rng.Intn(len(ids) + 1)
	switch g.rng.Intn(4) {
	case 0:
		if pos > 0 {
			after = &ids[pos-1]
		}
		if pos < len(ids) {
			before = &ids[pos]
		}
	case 1:
		if pos > 0 {
			after = &ids[pos-1]
		} else if len(ids) > 0 {
			before = &ids[0]
		}
	case 2:
		if pos < len(ids) {
			before = &ids[pos]
		} else if len(ids) > 0 {
			after = &ids[len(ids)-1]
		}
	}
	return after, before
}

func (g *generator) allCards(v *view) []*viewCard {
	var out []*viewCard
	for _, c := range v.cols {
		out = append(out, c.Cards...)
	}
	return out
}

func without(ids []uuid.UUID, id uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, x := range ids {
		if x != id {
			out = append(out, x)
		}
	}
	return out
}

func (g *generator) next(v *view) PendingOp {
	if len(v.cols) == 0 {
		return g.columnCreate(v)
	}
	if g.profile.HotSpot > 0 && g.rng.Float64() < g.profile.HotSpot {
		return g.hotCardCreate(v)
	}
	r := g.rng.Intn(100)
	cards := g.allCards(v)
	switch {
	case r < 24:
		return g.cardCreate(v)
	case r < 36 && len(cards) > 0:
		return g.cardEditTitle(cards)
	case r < 44 && len(cards) > 0:
		return g.cardEditDescription(cards)
	case r < 68 && len(cards) > 0:
		return g.cardMove(v, cards)
	case r < 74 && len(cards) > 0:
		return g.cardArchive(cards)
	case r < 79:
		if len(v.cols) < g.profile.MaxColumns {
			return g.columnCreate(v)
		}
		return g.columnRename(v)
	case r < 86:
		return g.columnRename(v)
	case r < 93 && len(v.cols) >= 2:
		return g.columnMove(v)
	case r < 95 && len(v.cols) >= 4:
		return g.columnArchive(v)
	}
	return g.cardCreate(v)
}

func (g *generator) columnCreate(v *view) PendingOp {
	after, before := g.anchors(columnIDs(v.cols))
	return PendingOp{ID: g.newID(), Type: boardcontent.OpColumnCreate, Payload: &boardcontent.ColumnCreate{
		ColumnID: g.newID(),
		Title:    g.text("col"),
		AfterID:  after,
		BeforeID: before,
	}}
}

func (g *generator) columnRename(v *view) PendingOp {
	col := v.cols[g.rng.Intn(len(v.cols))]
	return PendingOp{ID: g.newID(), Type: boardcontent.OpColumnRename, Payload: &boardcontent.ColumnRename{
		ColumnID: col.ID,
		Title:    g.text("col"),
	}}
}

func (g *generator) columnMove(v *view) PendingOp {
	col := v.cols[g.rng.Intn(len(v.cols))]
	after, before := g.anchors(without(columnIDs(v.cols), col.ID))
	version := col.Version
	return PendingOp{ID: g.newID(), Type: boardcontent.OpColumnMove, Payload: &boardcontent.ColumnMove{
		ColumnID:    col.ID,
		AfterID:     after,
		BeforeID:    before,
		BaseVersion: &version,
	}}
}

func (g *generator) columnArchive(v *view) PendingOp {
	col := v.cols[g.rng.Intn(len(v.cols))]
	return PendingOp{ID: g.newID(), Type: boardcontent.OpColumnArchive, Payload: &boardcontent.ColumnArchive{ColumnID: col.ID}}
}

func (g *generator) cardCreate(v *view) PendingOp {
	col := v.cols[g.rng.Intn(len(v.cols))]
	after, before := g.anchors(cardIDs(col.Cards))
	return g.cardCreateAt(col, after, before)
}

func (g *generator) hotCardCreate(v *view) PendingOp {
	col := v.cols[0]
	var after *uuid.UUID
	if len(col.Cards) > 0 {
		first := col.Cards[0].ID
		after = &first
	}
	return g.cardCreateAt(col, after, nil)
}

func (g *generator) cardCreateAt(col *viewColumn, after, before *uuid.UUID) PendingOp {
	create := &boardcontent.CardCreate{
		CardID:   g.newID(),
		ColumnID: col.ID,
		Title:    g.text("card"),
		AfterID:  after,
		BeforeID: before,
	}
	if g.rng.Intn(3) == 0 {
		d := g.description()
		create.Description = &d
	}
	return PendingOp{ID: g.newID(), Type: boardcontent.OpCardCreate, Payload: create}
}

func (g *generator) cardEditTitle(cards []*viewCard) PendingOp {
	card := cards[g.rng.Intn(len(cards))]
	title := g.text("card")
	return PendingOp{ID: g.newID(), Type: boardcontent.OpCardEdit, Payload: &boardcontent.CardEdit{CardID: card.ID, Title: &title}}
}

func (g *generator) cardEditDescription(cards []*viewCard) PendingOp {
	card := cards[g.rng.Intn(len(cards))]
	desc := g.description()
	base := card.DescVersion
	edit := &boardcontent.CardEdit{CardID: card.ID, Description: &desc, BaseDescriptionVersion: &base}
	if g.rng.Intn(3) == 0 {
		title := g.text("card")
		edit.Title = &title
	}
	return PendingOp{ID: g.newID(), Type: boardcontent.OpCardEdit, Payload: edit}
}

func (g *generator) cardMove(v *view, cards []*viewCard) PendingOp {
	card := cards[g.rng.Intn(len(cards))]
	to := v.cols[g.rng.Intn(len(v.cols))]
	after, before := g.anchors(without(cardIDs(to.Cards), card.ID))
	version := card.Version
	return PendingOp{ID: g.newID(), Type: boardcontent.OpCardMove, Payload: &boardcontent.CardMove{
		CardID:      card.ID,
		ToColumnID:  to.ID,
		AfterID:     after,
		BeforeID:    before,
		BaseVersion: &version,
	}}
}

func (g *generator) cardArchive(cards []*viewCard) PendingOp {
	card := cards[g.rng.Intn(len(cards))]
	return PendingOp{ID: g.newID(), Type: boardcontent.OpCardArchive, Payload: &boardcontent.CardArchive{CardID: card.ID}}
}
