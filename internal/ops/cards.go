package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

func (a *applier) cardCreate(ctx context.Context, p *boardcontent.CardCreate) error {
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
	if errors.Is(err, domain.ErrDuplicateID) {
		return idUnavailable("card_id")
	}
	if err != nil {
		return fmt.Errorf("insert card: %w", err)
	}
	return a.emit(ctx, boardcontent.EventCardCreated, boardcontent.CardCreated{Card: boardcontent.FromCard(card), ActorID: a.actor})
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

func (a *applier) cardEdit(ctx context.Context, p *boardcontent.CardEdit) error {
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
	return a.emit(ctx, boardcontent.EventCardEdited, boardcontent.CardEdited{
		CardID:             updated.ID,
		Title:              edit.Title,
		Description:        edit.Description,
		Version:            updated.Version,
		DescriptionVersion: updated.DescriptionVersion,
		ActorID:            a.actor,
	})
}

func cardEditFrom(p *boardcontent.CardEdit) (domain.CardEdit, error) {
	if p.Title == nil && p.Description == nil {
		return domain.CardEdit{}, invalid("payload: title or description is required")
	}
	var edit domain.CardEdit
	if p.Title != nil {
		title, err := domain.NormalizeCardTitle(*p.Title)
		if err != nil {
			return domain.CardEdit{}, err
		}
		edit.Title = &title
	}
	if p.Description == nil {
		if p.BaseDescriptionVersion != nil {
			return domain.CardEdit{}, invalid("base_description_version: is only allowed with description")
		}
		return edit, nil
	}
	if err := domain.ValidateDescription(*p.Description); err != nil {
		return domain.CardEdit{}, err
	}
	if err := requireVersion("base_description_version", p.BaseDescriptionVersion); err != nil {
		return domain.CardEdit{}, err
	}
	edit.Description = p.Description
	return edit, nil
}

func (a *applier) cardMove(ctx context.Context, p *boardcontent.CardMove) error {
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
	return a.emit(ctx, boardcontent.EventCardMoved, boardcontent.CardMoved{
		CardID:       moved.ID,
		FromColumnID: card.ColumnID,
		ToColumnID:   moved.ColumnID,
		Rank:         moved.Rank,
		Version:      moved.Version,
		ActorID:      a.actor,
	})
}

func (a *applier) cardArchive(ctx context.Context, p *boardcontent.CardArchive) error {
	if err := requireID("card_id", p.CardID); err != nil {
		return err
	}
	card, err := a.btx.ArchiveCard(ctx, p.CardID, a.actor)
	if err != nil {
		return missingAsNotFound(err, "card not found")
	}
	return a.emit(ctx, boardcontent.EventCardArchived, boardcontent.CardArchived{
		CardID:   card.ID,
		ColumnID: card.ColumnID,
		Version:  card.Version,
		ActorID:  a.actor,
	})
}

func (a *applier) cardRanks(columnID uuid.UUID) rankScope {
	return rankScope{
		probe: cardProbe{btx: a.btx, columnID: columnID},
		load: func(ctx context.Context) ([]domain.RankedItem, error) {
			return a.btx.CardRanks(ctx, columnID)
		},
		rerank: func(ctx context.Context, items []domain.RankedItem) error {
			return a.btx.RerankCards(ctx, columnID, items)
		},
		announce: func(ctx context.Context, pl placement) error {
			return a.emitCardRebalance(ctx, columnID, pl)
		},
	}
}

func (a *applier) emitCardRebalance(ctx context.Context, columnID uuid.UUID, pl placement) error {
	if pl.rebalance == nil {
		return nil
	}
	cards := make([]boardcontent.CardRank, len(pl.rebalance))
	for i, it := range pl.rebalance {
		cards[i] = boardcontent.CardRank{CardID: it.ID, Rank: it.Rank}
	}
	return a.emitExtra(ctx, boardcontent.EventColumnRebalanced, boardcontent.ColumnRebalanced{
		ColumnID: columnID,
		Cards:    cards,
		ActorID:  a.actor,
	})
}

type cardProbe struct {
	btx      boardtx.Tx
	columnID uuid.UUID
}

func (p cardProbe) window(ctx context.Context, anchor uuid.UUID, dir boardtx.RankDirection, exclude uuid.UUID) (string, string, bool, error) {
	return p.btx.CardRankWindow(ctx, p.columnID, anchor, dir, exclude)
}

func (p cardProbe) last(ctx context.Context, exclude uuid.UUID) (string, bool, error) {
	return p.btx.CardLastRank(ctx, p.columnID, exclude)
}
