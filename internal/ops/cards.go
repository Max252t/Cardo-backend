package ops

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

func (a *applier) cardCreate(ctx context.Context, p *protocol.CardCreate) error {
	if err := requireID("card_id", p.CardID); err != nil {
		return err
	}
	if err := requireID("column_id", p.ColumnID); err != nil {
		return err
	}
	title, err := domain.NormalizeCardTitle(p.Title)
	if err != nil {
		return err
	}
	description, err := optionalDescription(p.Description)
	if err != nil {
		return err
	}
	if err := requireUnusedID(ctx, "card_id", p.CardID, a.btx.CardIDExists); err != nil {
		return err
	}
	col, err := a.btx.Column(ctx, p.ColumnID)
	if err != nil {
		return missingAsNotFound(err, "column not found")
	}
	pl, err := placeIn(ctx, a.cardRanks(col.ID), p.CardID, p.AfterID, p.BeforeID)
	if err != nil {
		return err
	}
	card, err := a.btx.InsertCard(ctx, domain.Card{
		ID:          p.CardID,
		ColumnID:    col.ID,
		Title:       title,
		Description: description,
		Rank:        pl.rank,
		CreatedBy:   a.actor,
	})
	if err != nil {
		return fmt.Errorf("insert card: %w", err)
	}
	if err := a.emit(ctx, protocol.EventCardCreated, protocol.CardCreated{Card: protocol.FromCard(card), ActorID: a.actor}); err != nil {
		return err
	}
	return a.emitCardRebalance(ctx, col.ID, pl)
}

func optionalDescription(description *string) (string, error) {
	if description == nil {
		return "", nil
	}
	if err := domain.ValidateDescription(*description); err != nil {
		return "", err
	}
	return *description, nil
}

func (a *applier) cardEdit(ctx context.Context, p *protocol.CardEdit) error {
	if err := requireID("card_id", p.CardID); err != nil {
		return err
	}
	edit, err := cardEditFrom(p)
	if err != nil {
		return err
	}
	card, err := a.btx.Card(ctx, p.CardID)
	if err != nil {
		return missingAsNotFound(err, "card not found")
	}
	if edit.Description != nil && *p.BaseDescriptionVersion != card.DescriptionVersion {
		return conflict("description was changed by another user")
	}
	updated, err := a.btx.EditCard(ctx, card.ID, a.actor, edit)
	if err != nil {
		return missingAsNotFound(err, "card not found")
	}
	return a.emit(ctx, protocol.EventCardEdited, protocol.CardEdited{
		CardID:             updated.ID,
		Title:              edit.Title,
		Description:        edit.Description,
		Version:            updated.Version,
		DescriptionVersion: updated.DescriptionVersion,
		ActorID:            a.actor,
	})
}

func cardEditFrom(p *protocol.CardEdit) (storage.CardEdit, error) {
	if p.Title == nil && p.Description == nil {
		return storage.CardEdit{}, invalid("payload: title or description is required")
	}
	var edit storage.CardEdit
	if p.Title != nil {
		title, err := domain.NormalizeCardTitle(*p.Title)
		if err != nil {
			return storage.CardEdit{}, err
		}
		edit.Title = &title
	}
	if p.Description == nil {
		if p.BaseDescriptionVersion != nil {
			return storage.CardEdit{}, invalid("base_description_version: is only allowed with description")
		}
		return edit, nil
	}
	if err := domain.ValidateDescription(*p.Description); err != nil {
		return storage.CardEdit{}, err
	}
	if err := requireVersion("base_description_version", p.BaseDescriptionVersion); err != nil {
		return storage.CardEdit{}, err
	}
	edit.Description = p.Description
	return edit, nil
}

func (a *applier) cardMove(ctx context.Context, p *protocol.CardMove) error {
	if err := requireID("card_id", p.CardID); err != nil {
		return err
	}
	if err := requireID("to_column_id", p.ToColumnID); err != nil {
		return err
	}
	if err := requireVersion("base_version", p.BaseVersion); err != nil {
		return err
	}
	card, err := a.btx.Card(ctx, p.CardID)
	if err != nil {
		return missingAsNotFound(err, "card not found")
	}
	col, err := a.btx.Column(ctx, p.ToColumnID)
	if err != nil {
		return missingAsNotFound(err, "target column not found")
	}
	if *p.BaseVersion != card.Version {
		return conflict("card was changed by another user")
	}
	pl, err := placeIn(ctx, a.cardRanks(col.ID), card.ID, p.AfterID, p.BeforeID)
	if err != nil {
		return err
	}
	moved, err := a.btx.MoveCard(ctx, card.ID, a.actor, col.ID, pl.rank)
	if err != nil {
		return fmt.Errorf("move card: %w", err)
	}
	if err := a.emit(ctx, protocol.EventCardMoved, protocol.CardMoved{
		CardID:       moved.ID,
		FromColumnID: card.ColumnID,
		ToColumnID:   moved.ColumnID,
		Rank:         moved.Rank,
		Version:      moved.Version,
		ActorID:      a.actor,
	}); err != nil {
		return err
	}
	return a.emitCardRebalance(ctx, col.ID, pl)
}

func (a *applier) cardArchive(ctx context.Context, p *protocol.CardArchive) error {
	if err := requireID("card_id", p.CardID); err != nil {
		return err
	}
	card, err := a.btx.ArchiveCard(ctx, p.CardID, a.actor)
	if err != nil {
		return missingAsNotFound(err, "card not found")
	}
	return a.emit(ctx, protocol.EventCardArchived, protocol.CardArchived{
		CardID:   card.ID,
		ColumnID: card.ColumnID,
		Version:  card.Version,
		ActorID:  a.actor,
	})
}

func (a *applier) cardRanks(columnID uuid.UUID) rankScope {
	return rankScope{
		load: func(ctx context.Context) ([]storage.RankedItem, error) {
			return a.btx.CardRanks(ctx, columnID)
		},
		rerank: func(ctx context.Context, items []storage.RankedItem) error {
			return a.btx.RerankCards(ctx, columnID, items)
		},
	}
}

func (a *applier) emitCardRebalance(ctx context.Context, columnID uuid.UUID, pl placement) error {
	if pl.rebalance == nil {
		return nil
	}
	cards := make([]protocol.CardRank, len(pl.rebalance))
	for i, it := range pl.rebalance {
		cards[i] = protocol.CardRank{CardID: it.ID, Rank: it.Rank}
	}
	return a.emitExtra(ctx, protocol.EventColumnRebalanced, protocol.ColumnRebalanced{
		ColumnID: columnID,
		Cards:    cards,
		ActorID:  a.actor,
	})
}
