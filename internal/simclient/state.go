package simclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type Column struct {
	ID      uuid.UUID
	Title   string
	Rank    string
	Version int64
}

type Card struct {
	ID                 uuid.UUID
	ColumnID           uuid.UUID
	Title              string
	Description        string
	Rank               string
	Version            int64
	DescriptionVersion int64
}

type State struct {
	Seq     int64
	Columns map[uuid.UUID]*Column
	Cards   map[uuid.UUID]*Card
}

func NewState() *State {
	return &State{Columns: make(map[uuid.UUID]*Column), Cards: make(map[uuid.UUID]*Card)}
}

func (s *State) Clone() *State {
	out := &State{Seq: s.Seq, Columns: make(map[uuid.UUID]*Column, len(s.Columns)), Cards: make(map[uuid.UUID]*Card, len(s.Cards))}
	for id, c := range s.Columns {
		cp := *c
		out.Columns[id] = &cp
	}
	for id, c := range s.Cards {
		cp := *c
		out.Cards[id] = &cp
	}
	return out
}

func (s *State) Load(snap protocol.Snapshot) {
	s.Seq = snap.Seq
	s.Columns = make(map[uuid.UUID]*Column, len(snap.Columns))
	s.Cards = make(map[uuid.UUID]*Card, len(snap.Cards))
	for _, c := range snap.Columns {
		s.Columns[c.ID] = &Column{ID: c.ID, Title: c.Title, Rank: c.Rank, Version: c.Version}
	}
	for _, c := range snap.Cards {
		s.Cards[c.ID] = &Card{
			ID:                 c.ID,
			ColumnID:           c.ColumnID,
			Title:              c.Title,
			Description:        c.Description,
			Rank:               c.Rank,
			Version:            c.Version,
			DescriptionVersion: c.DescriptionVersion,
		}
	}
}

func lessKey(rankA string, idA uuid.UUID, rankB string, idB uuid.UUID) bool {
	if rankA != rankB {
		return rankA < rankB
	}
	return bytes.Compare(idA[:], idB[:]) < 0
}

func (s *State) OrderedColumns() []*Column {
	out := make([]*Column, 0, len(s.Columns))
	for _, c := range s.Columns {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Rank, out[i].ID, out[j].Rank, out[j].ID) })
	return out
}

func (s *State) OrderedCards(columnID uuid.UUID) []*Card {
	var out []*Card
	for _, c := range s.Cards {
		if c.ColumnID == columnID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessKey(out[i].Rank, out[i].ID, out[j].Rank, out[j].ID) })
	return out
}

func decode[T any](env protocol.Envelope) (T, error) {
	var v T
	if err := json.Unmarshal(env.Payload, &v); err != nil {
		return v, fmt.Errorf("decode %s payload: %w", env.Type, err)
	}
	return v, nil
}

func (s *State) Apply(env protocol.Envelope) error {
	if err := s.applyPayload(env); err != nil {
		return fmt.Errorf("apply seq %d %s: %w", env.Seq, env.Type, err)
	}
	s.Seq = env.Seq
	return nil
}

func (s *State) applyPayload(env protocol.Envelope) error {
	switch env.Type {
	case boardcontent.EventColumnCreated:
		p, err := decode[boardcontent.ColumnCreated](env)
		if err != nil {
			return err
		}
		if _, dup := s.Columns[p.ID]; dup {
			return fmt.Errorf("column %s already exists", p.ID)
		}
		s.Columns[p.ID] = &Column{ID: p.ID, Title: p.Title, Rank: p.Rank, Version: p.Version}
	case boardcontent.EventColumnRenamed:
		p, err := decode[boardcontent.ColumnRenamed](env)
		if err != nil {
			return err
		}
		col, ok := s.Columns[p.ColumnID]
		if !ok {
			return fmt.Errorf("column %s is unknown", p.ColumnID)
		}
		col.Title, col.Version = p.Title, p.Version
	case boardcontent.EventColumnMoved:
		p, err := decode[boardcontent.ColumnMoved](env)
		if err != nil {
			return err
		}
		col, ok := s.Columns[p.ColumnID]
		if !ok {
			return fmt.Errorf("column %s is unknown", p.ColumnID)
		}
		col.Rank, col.Version = p.Rank, p.Version
	case boardcontent.EventColumnArchived:
		p, err := decode[boardcontent.ColumnArchived](env)
		if err != nil {
			return err
		}
		if _, ok := s.Columns[p.ColumnID]; !ok {
			return fmt.Errorf("column %s is unknown", p.ColumnID)
		}
		delete(s.Columns, p.ColumnID)
		for _, id := range p.CardIDs {
			delete(s.Cards, id)
		}
	case boardcontent.EventColumnsRebalanced:
		p, err := decode[boardcontent.ColumnsRebalanced](env)
		if err != nil {
			return err
		}
		for _, r := range p.Columns {
			col, ok := s.Columns[r.ColumnID]
			if !ok {
				return fmt.Errorf("column %s is unknown", r.ColumnID)
			}
			col.Rank = r.Rank
		}
	case boardcontent.EventColumnRebalanced:
		p, err := decode[boardcontent.ColumnRebalanced](env)
		if err != nil {
			return err
		}
		for _, r := range p.Cards {
			card, ok := s.Cards[r.CardID]
			if !ok {
				return fmt.Errorf("card %s is unknown", r.CardID)
			}
			card.Rank = r.Rank
		}
	case boardcontent.EventCardCreated:
		p, err := decode[boardcontent.CardCreated](env)
		if err != nil {
			return err
		}
		if _, dup := s.Cards[p.ID]; dup {
			return fmt.Errorf("card %s already exists", p.ID)
		}
		s.Cards[p.ID] = &Card{
			ID:                 p.ID,
			ColumnID:           p.ColumnID,
			Title:              p.Title,
			Description:        p.Description,
			Rank:               p.Rank,
			Version:            p.Version,
			DescriptionVersion: p.DescriptionVersion,
		}
	case boardcontent.EventCardEdited:
		p, err := decode[boardcontent.CardEdited](env)
		if err != nil {
			return err
		}
		card, ok := s.Cards[p.CardID]
		if !ok {
			return fmt.Errorf("card %s is unknown", p.CardID)
		}
		if p.Title != nil {
			card.Title = *p.Title
		}
		if p.Description != nil {
			card.Description = *p.Description
		}
		card.Version, card.DescriptionVersion = p.Version, p.DescriptionVersion
	case boardcontent.EventCardMoved:
		p, err := decode[boardcontent.CardMoved](env)
		if err != nil {
			return err
		}
		card, ok := s.Cards[p.CardID]
		if !ok {
			return fmt.Errorf("card %s is unknown", p.CardID)
		}
		card.ColumnID, card.Rank, card.Version = p.ToColumnID, p.Rank, p.Version
	case boardcontent.EventCardArchived:
		p, err := decode[boardcontent.CardArchived](env)
		if err != nil {
			return err
		}
		if _, ok := s.Cards[p.CardID]; !ok {
			return fmt.Errorf("card %s is unknown", p.CardID)
		}
		delete(s.Cards, p.CardID)
	}
	return nil
}

func (s *State) Diff(snap protocol.Snapshot) []string {
	var diffs []string
	if s.Seq != snap.Seq {
		diffs = append(diffs, fmt.Sprintf("seq: client %d, database %d", s.Seq, snap.Seq))
	}
	cols := s.OrderedColumns()
	if len(cols) != len(snap.Columns) {
		diffs = append(diffs, fmt.Sprintf("columns: client has %d, database has %d", len(cols), len(snap.Columns)))
	}
	for i := 0; i < len(cols) && i < len(snap.Columns); i++ {
		want, got := snap.Columns[i], cols[i]
		if want.ID != got.ID || want.Title != got.Title || want.Rank != got.Rank || want.Version != got.Version {
			diffs = append(diffs, fmt.Sprintf("column #%d: client %+v, database %+v", i, *got, want))
		}
	}
	wantCards := make(map[uuid.UUID][]boardcontent.Card)
	for _, c := range snap.Cards {
		wantCards[c.ColumnID] = append(wantCards[c.ColumnID], c)
	}
	total := 0
	for _, col := range snap.Columns {
		want := wantCards[col.ID]
		got := s.OrderedCards(col.ID)
		total += len(got)
		if len(got) != len(want) {
			diffs = append(diffs, fmt.Sprintf("column %s: client has %d cards, database has %d", col.ID, len(got), len(want)))
			continue
		}
		for i := range want {
			w, g := want[i], got[i]
			if w.ID != g.ID || w.Title != g.Title || w.Description != g.Description || w.Rank != g.Rank ||
				w.Version != g.Version || w.DescriptionVersion != g.DescriptionVersion {
				diffs = append(diffs, fmt.Sprintf("column %s card #%d: client %+v, database id=%s title=%q desc=%q rank=%s v=%d dv=%d",
					col.ID, i, *g, w.ID, w.Title, w.Description, w.Rank, w.Version, w.DescriptionVersion))
			}
		}
	}
	if total != len(s.Cards) {
		diffs = append(diffs, fmt.Sprintf("cards: client has %d in known columns but %d overall", total, len(s.Cards)))
	}
	return diffs
}
