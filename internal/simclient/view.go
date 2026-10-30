package simclient

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
)

type PendingOp struct {
	ID      uuid.UUID
	Type    string
	Payload any
}

func (p PendingOp) raw() (json.RawMessage, error) {
	return json.Marshal(p.Payload)
}

type viewCard struct {
	ID          uuid.UUID
	Title       string
	Description string
	Version     int64
	DescVersion int64
}

type viewColumn struct {
	ID      uuid.UUID
	Title   string
	Version int64
	Cards   []*viewCard
}

type view struct {
	cols []*viewColumn
}

func buildView(s *State) *view {
	v := &view{}
	for _, c := range s.OrderedColumns() {
		vc := &viewColumn{ID: c.ID, Title: c.Title, Version: c.Version}
		for _, card := range s.OrderedCards(c.ID) {
			vc.Cards = append(vc.Cards, &viewCard{
				ID:          card.ID,
				Title:       card.Title,
				Description: card.Description,
				Version:     card.Version,
				DescVersion: card.DescriptionVersion,
			})
		}
		v.cols = append(v.cols, vc)
	}
	return v
}

func (v *view) column(id uuid.UUID) (int, *viewColumn) {
	for i, c := range v.cols {
		if c.ID == id {
			return i, c
		}
	}
	return -1, nil
}

func (v *view) card(id uuid.UUID) (*viewColumn, int, *viewCard) {
	for _, c := range v.cols {
		for i, card := range c.Cards {
			if card.ID == id {
				return c, i, card
			}
		}
	}
	return nil, -1, nil
}

func anchorID(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

func insertPos(ids []uuid.UUID, after, before *uuid.UUID) int {
	if a := anchorID(after); a != uuid.Nil {
		for i, id := range ids {
			if id == a {
				return i + 1
			}
		}
	}
	if b := anchorID(before); b != uuid.Nil {
		for i, id := range ids {
			if id == b {
				return i
			}
		}
	}
	return len(ids)
}

func insertAt[T any](items []T, pos int, item T) []T {
	items = append(items, item)
	copy(items[pos+1:], items[pos:])
	items[pos] = item
	return items
}

func removeAt[T any](items []T, idx int) []T {
	return append(items[:idx], items[idx+1:]...)
}

func cardIDs(cards []*viewCard) []uuid.UUID {
	out := make([]uuid.UUID, len(cards))
	for i, c := range cards {
		out[i] = c.ID
	}
	return out
}

func columnIDs(cols []*viewColumn) []uuid.UUID {
	out := make([]uuid.UUID, len(cols))
	for i, c := range cols {
		out[i] = c.ID
	}
	return out
}

func (v *view) apply(op PendingOp) bool {
	switch p := op.Payload.(type) {
	case *boardcontent.ColumnCreate:
		return v.columnCreate(p)
	case *boardcontent.ColumnRename:
		return v.columnRename(p)
	case *boardcontent.ColumnMove:
		return v.columnMove(p)
	case *boardcontent.ColumnArchive:
		return v.columnArchive(p)
	case *boardcontent.CardCreate:
		return v.cardCreate(p)
	case *boardcontent.CardEdit:
		return v.cardEdit(p)
	case *boardcontent.CardMove:
		return v.cardMove(p)
	case *boardcontent.CardArchive:
		return v.cardArchive(p)
	}
	return false
}

func (v *view) columnCreate(p *boardcontent.ColumnCreate) bool {
	if i, _ := v.column(p.ColumnID); i >= 0 {
		return false
	}
	pos := insertPos(columnIDs(v.cols), p.AfterID, p.BeforeID)
	v.cols = insertAt(v.cols, pos, &viewColumn{ID: p.ColumnID, Title: p.Title, Version: 1})
	return true
}

func (v *view) columnRename(p *boardcontent.ColumnRename) bool {
	_, col := v.column(p.ColumnID)
	if col == nil {
		return false
	}
	col.Title = p.Title
	col.Version++
	return true
}

func (v *view) columnMove(p *boardcontent.ColumnMove) bool {
	idx, col := v.column(p.ColumnID)
	if col == nil || p.BaseVersion == nil || *p.BaseVersion != col.Version {
		return false
	}
	v.cols = removeAt(v.cols, idx)
	pos := insertPos(columnIDs(v.cols), p.AfterID, p.BeforeID)
	v.cols = insertAt(v.cols, pos, col)
	col.Version++
	return true
}

func (v *view) columnArchive(p *boardcontent.ColumnArchive) bool {
	idx, col := v.column(p.ColumnID)
	if col == nil {
		return false
	}
	v.cols = removeAt(v.cols, idx)
	return true
}

func (v *view) cardCreate(p *boardcontent.CardCreate) bool {
	if col, _, card := v.card(p.CardID); col != nil || card != nil {
		return false
	}
	_, col := v.column(p.ColumnID)
	if col == nil {
		return false
	}
	card := &viewCard{ID: p.CardID, Title: p.Title, Version: 1, DescVersion: 1}
	if p.Description != nil {
		card.Description = *p.Description
	}
	col.Cards = insertAt(col.Cards, insertPos(cardIDs(col.Cards), p.AfterID, p.BeforeID), card)
	return true
}

func (v *view) cardEdit(p *boardcontent.CardEdit) bool {
	_, _, card := v.card(p.CardID)
	if card == nil {
		return false
	}
	if p.Description != nil {
		if p.BaseDescriptionVersion == nil || *p.BaseDescriptionVersion != card.DescVersion {
			return false
		}
		card.Description = *p.Description
		card.DescVersion++
	}
	if p.Title != nil {
		card.Title = *p.Title
	}
	card.Version++
	return true
}

func (v *view) cardMove(p *boardcontent.CardMove) bool {
	from, idx, card := v.card(p.CardID)
	if card == nil || p.BaseVersion == nil || *p.BaseVersion != card.Version {
		return false
	}
	_, to := v.column(p.ToColumnID)
	if to == nil {
		return false
	}
	from.Cards = removeAt(from.Cards, idx)
	to.Cards = insertAt(to.Cards, insertPos(cardIDs(to.Cards), p.AfterID, p.BeforeID), card)
	card.Version++
	return true
}

func (v *view) cardArchive(p *boardcontent.CardArchive) bool {
	col, idx, card := v.card(p.CardID)
	if card == nil {
		return false
	}
	col.Cards = removeAt(col.Cards, idx)
	return true
}
