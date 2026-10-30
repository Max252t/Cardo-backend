package simclient

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func seededState(t *testing.T) (*State, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	col1, col2, card := uuid.New(), uuid.New(), uuid.New()
	s := NewState()
	s.Load(protocol.Snapshot{
		Seq: 10,
		Columns: []boardcontent.Column{
			{ID: col1, Title: "One", Rank: "b", Version: 1},
			{ID: col2, Title: "Two", Rank: "d", Version: 1},
		},
		Cards: []boardcontent.Card{{ID: card, ColumnID: col1, Title: "K", Description: "d", Rank: "m", Version: 1, DescriptionVersion: 1}},
	})
	return s, col1, col2, card
}

func TestStateLoadReplacesEverything(t *testing.T) {
	s, _, _, _ := seededState(t)
	s.Load(protocol.Snapshot{Seq: 2})
	assert.Equal(t, int64(2), s.Seq)
	assert.Empty(t, s.Columns)
	assert.Empty(t, s.Cards)
}

func TestStateCloneIsDeep(t *testing.T) {
	s, col1, _, card := seededState(t)
	cp := s.Clone()
	cp.Columns[col1].Title = "changed"
	cp.Cards[card].Title = "changed"
	cp.Seq = 99
	assert.Equal(t, "One", s.Columns[col1].Title)
	assert.Equal(t, "K", s.Cards[card].Title)
	assert.Equal(t, int64(10), s.Seq)
}

func TestStateAppliesEveryEventType(t *testing.T) {
	s, col1, col2, card := seededState(t)
	newCol, newCard := uuid.New(), uuid.New()
	title, desc := "T2", "D2"
	steps := []struct {
		name  string
		env   protocol.Envelope
		check func(t *testing.T)
	}{
		{"column created", columnCreatedEvent(t, 11, nil, newCol, "New", "f"), func(t *testing.T) { assert.Equal(t, "New", s.Columns[newCol].Title) }},
		{"column renamed", event(t, 12, boardcontent.EventColumnRenamed, nil, boardcontent.ColumnRenamed{ColumnID: col1, Title: "Renamed", Version: 2}), func(t *testing.T) {
			assert.Equal(t, "Renamed", s.Columns[col1].Title)
			assert.Equal(t, int64(2), s.Columns[col1].Version)
		}},
		{"column moved", event(t, 13, boardcontent.EventColumnMoved, nil, boardcontent.ColumnMoved{ColumnID: col1, Rank: "z", Version: 3}), func(t *testing.T) {
			assert.Equal(t, "z", s.Columns[col1].Rank)
			assert.Equal(t, col2, s.OrderedColumns()[0].ID)
		}},
		{"card created", cardCreatedEvent(t, 14, nil, newCard, col2, "N", "a"), func(t *testing.T) { assert.Equal(t, col2, s.Cards[newCard].ColumnID) }},
		{"card edited", event(t, 15, boardcontent.EventCardEdited, nil, boardcontent.CardEdited{CardID: card, Title: &title, Description: &desc, Version: 2, DescriptionVersion: 2}), func(t *testing.T) {
			assert.Equal(t, "T2", s.Cards[card].Title)
			assert.Equal(t, "D2", s.Cards[card].Description)
			assert.Equal(t, int64(2), s.Cards[card].DescriptionVersion)
		}},
		{"card edited title only keeps description", event(t, 16, boardcontent.EventCardEdited, nil, boardcontent.CardEdited{CardID: card, Title: &desc, Version: 3, DescriptionVersion: 2}), func(t *testing.T) {
			assert.Equal(t, "D2", s.Cards[card].Title)
			assert.Equal(t, "D2", s.Cards[card].Description)
		}},
		{"card moved", event(t, 17, boardcontent.EventCardMoved, nil, boardcontent.CardMoved{CardID: card, FromColumnID: col1, ToColumnID: col2, Rank: "b", Version: 4}), func(t *testing.T) {
			assert.Equal(t, col2, s.Cards[card].ColumnID)
			ordered := s.OrderedCards(col2)
			require.Len(t, ordered, 2)
			assert.Equal(t, card, ordered[1].ID)
		}},
		{"card archived", event(t, 18, boardcontent.EventCardArchived, nil, boardcontent.CardArchived{CardID: newCard, ColumnID: col2, Version: 2}), func(t *testing.T) { assert.NotContains(t, s.Cards, newCard) }},
		{"column archived removes its cards", event(t, 19, boardcontent.EventColumnArchived, nil, boardcontent.ColumnArchived{ColumnID: col2, CardIDs: []uuid.UUID{card}, Version: 2}), func(t *testing.T) {
			assert.NotContains(t, s.Columns, col2)
			assert.Empty(t, s.Cards)
		}},
	}
	for _, step := range steps {
		require.NoError(t, s.Apply(step.env), step.name)
		assert.Equal(t, step.env.Seq, s.Seq, step.name)
		step.check(t)
	}
}

func TestStateRebalanceEventsReorderWithoutBumpingVersions(t *testing.T) {
	s, col1, col2, card := seededState(t)
	card2 := uuid.New()
	require.NoError(t, s.Apply(cardCreatedEvent(t, 11, nil, card2, col1, "K2", "n")))
	require.Equal(t, []uuid.UUID{card, card2}, []uuid.UUID{s.OrderedCards(col1)[0].ID, s.OrderedCards(col1)[1].ID})

	require.NoError(t, s.Apply(event(t, 12, boardcontent.EventColumnRebalanced, nil, boardcontent.ColumnRebalanced{
		ColumnID: col1,
		Cards:    []boardcontent.CardRank{{CardID: card, Rank: "q"}, {CardID: card2, Rank: "c"}},
	})))
	ordered := s.OrderedCards(col1)
	assert.Equal(t, card2, ordered[0].ID)
	assert.Equal(t, card, ordered[1].ID)
	assert.Equal(t, int64(1), s.Cards[card].Version)

	require.NoError(t, s.Apply(event(t, 13, boardcontent.EventColumnsRebalanced, nil, boardcontent.ColumnsRebalanced{
		Columns: []boardcontent.ColumnRank{{ColumnID: col1, Rank: "y"}, {ColumnID: col2, Rank: "e"}},
	})))
	cols := s.OrderedColumns()
	assert.Equal(t, col2, cols[0].ID)
	assert.Equal(t, col1, cols[1].ID)
	assert.Equal(t, int64(1), s.Columns[col1].Version)
	assert.Equal(t, int64(13), s.Seq)
}

func TestStateRejectsEventsThatDoNotFit(t *testing.T) {
	s, col1, _, card := seededState(t)
	missing := uuid.New()
	cases := map[string]protocol.Envelope{
		"duplicate column":          columnCreatedEvent(t, 11, nil, col1, "dup", "x"),
		"duplicate card":            cardCreatedEvent(t, 11, nil, card, col1, "dup", "x"),
		"unknown rename":            event(t, 11, boardcontent.EventColumnRenamed, nil, boardcontent.ColumnRenamed{ColumnID: missing}),
		"unknown column move":       event(t, 11, boardcontent.EventColumnMoved, nil, boardcontent.ColumnMoved{ColumnID: missing}),
		"unknown column archive":    event(t, 11, boardcontent.EventColumnArchived, nil, boardcontent.ColumnArchived{ColumnID: missing}),
		"unknown card edit":         event(t, 11, boardcontent.EventCardEdited, nil, boardcontent.CardEdited{CardID: missing}),
		"unknown card move":         event(t, 11, boardcontent.EventCardMoved, nil, boardcontent.CardMoved{CardID: missing}),
		"unknown card archive":      event(t, 11, boardcontent.EventCardArchived, nil, boardcontent.CardArchived{CardID: missing}),
		"unknown rebalanced card":   event(t, 11, boardcontent.EventColumnRebalanced, nil, boardcontent.ColumnRebalanced{Cards: []boardcontent.CardRank{{CardID: missing, Rank: "a"}}}),
		"unknown rebalanced column": event(t, 11, boardcontent.EventColumnsRebalanced, nil, boardcontent.ColumnsRebalanced{Columns: []boardcontent.ColumnRank{{ColumnID: missing, Rank: "a"}}}),
		"bad payload":               {V: protocol.Version, Type: boardcontent.EventCardCreated, Seq: 11, Payload: []byte(`[1]`)},
	}
	for name, env := range cases {
		err := s.Apply(env)
		assert.Error(t, err, name)
		assert.Equal(t, int64(10), s.Seq, name)
	}
}

func TestStateIgnoresEventTypesItDoesNotTrack(t *testing.T) {
	s, _, _, _ := seededState(t)
	require.NoError(t, s.Apply(event(t, 11, "board.renamed", nil, map[string]string{"title": "x"})))
	assert.Equal(t, int64(11), s.Seq)
}

func TestOrderingBreaksRankTiesByID(t *testing.T) {
	low := uuid.MustParse("00000000-0000-4000-8000-000000000001")
	high := uuid.MustParse("00000000-0000-4000-8000-000000000002")
	s := NewState()
	s.Load(protocol.Snapshot{Columns: []boardcontent.Column{
		{ID: high, Title: "h", Rank: "m"},
		{ID: low, Title: "l", Rank: "m"},
	}})
	cols := s.OrderedColumns()
	assert.Equal(t, low, cols[0].ID)
	assert.Equal(t, high, cols[1].ID)
}

func TestStateDiffDetectsEveryKindOfDivergence(t *testing.T) {
	s, col1, col2, card := seededState(t)
	same := func() protocol.Snapshot {
		return protocol.Snapshot{
			Seq: 10,
			Columns: []boardcontent.Column{
				{ID: col1, Title: "One", Rank: "b", Version: 1},
				{ID: col2, Title: "Two", Rank: "d", Version: 1},
			},
			Cards: []boardcontent.Card{{ID: card, ColumnID: col1, Title: "K", Description: "d", Rank: "m", Version: 1, DescriptionVersion: 1}},
		}
	}
	assert.Empty(t, s.Diff(same()))

	mutations := map[string]func(*protocol.Snapshot){
		"seq":            func(p *protocol.Snapshot) { p.Seq = 11 },
		"column title":   func(p *protocol.Snapshot) { p.Columns[0].Title = "x" },
		"column rank":    func(p *protocol.Snapshot) { p.Columns[0].Rank = "a" },
		"column version": func(p *protocol.Snapshot) { p.Columns[1].Version = 5 },
		"missing column": func(p *protocol.Snapshot) { p.Columns = p.Columns[:1] },
		"card title":     func(p *protocol.Snapshot) { p.Cards[0].Title = "x" },
		"card desc":      func(p *protocol.Snapshot) { p.Cards[0].Description = "x" },
		"card rank":      func(p *protocol.Snapshot) { p.Cards[0].Rank = "x" },
		"card version":   func(p *protocol.Snapshot) { p.Cards[0].Version = 9 },
		"card desc ver":  func(p *protocol.Snapshot) { p.Cards[0].DescriptionVersion = 9 },
		"missing card":   func(p *protocol.Snapshot) { p.Cards = nil },
		"extra card": func(p *protocol.Snapshot) {
			p.Cards = append(p.Cards, boardcontent.Card{ID: uuid.New(), ColumnID: col2})
		},
		"card in column": func(p *protocol.Snapshot) { p.Cards[0].ColumnID = col2 },
	}
	for name, mutate := range mutations {
		snap := same()
		mutate(&snap)
		assert.NotEmpty(t, s.Diff(snap), name)
	}
}

func TestStateDiffReportsCardsOutsideKnownColumns(t *testing.T) {
	s, col1, col2, card := seededState(t)
	snap := protocol.Snapshot{
		Seq: 10,
		Columns: []boardcontent.Column{
			{ID: col1, Title: "One", Rank: "b", Version: 1},
			{ID: col2, Title: "Two", Rank: "d", Version: 1},
		},
		Cards: []boardcontent.Card{{ID: card, ColumnID: col1, Title: "K", Description: "d", Rank: "m", Version: 1, DescriptionVersion: 1}},
	}
	s.Cards[uuid.New()] = &Card{ColumnID: uuid.New()}
	assert.NotEmpty(t, s.Diff(snap))
}
