package boardcontent

import (
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	EventCardCreated       = "card.created"
	EventCardEdited        = "card.edited"
	EventCardMoved         = "card.moved"
	EventCardArchived      = "card.archived"
	EventColumnCreated     = "column.created"
	EventColumnRenamed     = "column.renamed"
	EventColumnMoved       = "column.moved"
	EventColumnArchived    = "column.archived"
	EventColumnRebalanced  = "column.rebalanced"
	EventColumnsRebalanced = "columns.rebalanced"
	EventBoardRenamed      = domain.EventBoardRenamed
	EventBoardDeleted      = domain.EventBoardDeleted
)

type Card struct {
	ID                 uuid.UUID `json:"id"`
	ColumnID           uuid.UUID `json:"column_id"`
	Title              string    `json:"title"`
	Description        string    `json:"description"`
	Rank               string    `json:"rank"`
	Version            int64     `json:"version"`
	DescriptionVersion int64     `json:"description_version"`
	CreatedBy          uuid.UUID `json:"created_by"`
	UpdatedBy          uuid.UUID `json:"updated_by"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func FromCard(c domain.Card) Card {
	return Card{
		ID:                 c.ID,
		ColumnID:           c.ColumnID,
		Title:              c.Title,
		Description:        c.Description,
		Rank:               c.Rank,
		Version:            c.Version,
		DescriptionVersion: c.DescriptionVersion,
		CreatedBy:          c.CreatedBy,
		UpdatedBy:          c.UpdatedBy,
		CreatedAt:          c.CreatedAt.UTC(),
		UpdatedAt:          c.UpdatedAt.UTC(),
	}
}

type Column struct {
	ID      uuid.UUID `json:"id"`
	Title   string    `json:"title"`
	Rank    string    `json:"rank"`
	Version int64     `json:"version"`
}

func FromColumn(c domain.Column) Column {
	return Column{ID: c.ID, Title: c.Title, Rank: c.Rank, Version: c.Version}
}

type CardCreated struct {
	Card
	ActorID uuid.UUID `json:"actor_id"`
}

type CardEdited struct {
	CardID             uuid.UUID `json:"card_id"`
	Title              *string   `json:"title,omitempty"`
	Description        *string   `json:"description,omitempty"`
	Version            int64     `json:"version"`
	DescriptionVersion int64     `json:"description_version"`
	ActorID            uuid.UUID `json:"actor_id"`
}

type CardMoved struct {
	CardID       uuid.UUID `json:"card_id"`
	FromColumnID uuid.UUID `json:"from_column_id"`
	ToColumnID   uuid.UUID `json:"to_column_id"`
	Rank         string    `json:"rank"`
	Version      int64     `json:"version"`
	ActorID      uuid.UUID `json:"actor_id"`
}

type CardArchived struct {
	CardID   uuid.UUID `json:"card_id"`
	ColumnID uuid.UUID `json:"column_id"`
	Version  int64     `json:"version"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type ColumnCreated struct {
	Column
	ActorID uuid.UUID `json:"actor_id"`
}

type ColumnRenamed struct {
	ColumnID uuid.UUID `json:"column_id"`
	Title    string    `json:"title"`
	Version  int64     `json:"version"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type ColumnMoved struct {
	ColumnID uuid.UUID `json:"column_id"`
	Rank     string    `json:"rank"`
	Version  int64     `json:"version"`
	ActorID  uuid.UUID `json:"actor_id"`
}

type ColumnArchived struct {
	ColumnID uuid.UUID   `json:"column_id"`
	CardIDs  []uuid.UUID `json:"card_ids"`
	Version  int64       `json:"version"`
	ActorID  uuid.UUID   `json:"actor_id"`
}

type CardRank struct {
	CardID uuid.UUID `json:"card_id"`
	Rank   string    `json:"rank"`
}

type ColumnRebalanced struct {
	ColumnID uuid.UUID  `json:"column_id"`
	Cards    []CardRank `json:"cards"`
	ActorID  uuid.UUID  `json:"actor_id"`
}

type ColumnRank struct {
	ColumnID uuid.UUID `json:"column_id"`
	Rank     string    `json:"rank"`
}

type ColumnsRebalanced struct {
	Columns []ColumnRank `json:"columns"`
	ActorID uuid.UUID    `json:"actor_id"`
}

type BoardRenamed struct {
	Title   string    `json:"title"`
	ActorID uuid.UUID `json:"actor_id"`
}

type BoardDeleted struct {
	ActorID uuid.UUID `json:"actor_id"`
}
