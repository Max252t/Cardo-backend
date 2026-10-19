package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type result struct {
	primary domain.Event
	extra   []domain.Event
}

type applier struct {
	btx   *storage.BoardTx
	actor uuid.UUID
	opID  uuid.UUID
	res   result
}

func (a *applier) authorize(ctx context.Context) error {
	role, err := a.btx.MemberRole(ctx, a.actor)
	if errors.Is(err, domain.ErrNotFound) {
		return forbidden("not a member of this board")
	}
	if err != nil {
		return err
	}
	if !role.AtLeast(domain.RoleEditor) {
		return forbidden("editor role is required")
	}
	return nil
}

func (a *applier) run(ctx context.Context, op protocol.Op) error {
	payload, err := protocol.DecodePayload(op)
	if err != nil {
		return asRejection(err)
	}
	switch p := payload.(type) {
	case *protocol.CardCreate:
		err = a.cardCreate(ctx, p)
	case *protocol.CardEdit:
		err = a.cardEdit(ctx, p)
	case *protocol.CardMove:
		err = a.cardMove(ctx, p)
	case *protocol.CardArchive:
		err = a.cardArchive(ctx, p)
	case *protocol.ColumnCreate:
		err = a.columnCreate(ctx, p)
	case *protocol.ColumnRename:
		err = a.columnRename(ctx, p)
	case *protocol.ColumnMove:
		err = a.columnMove(ctx, p)
	case *protocol.ColumnArchive:
		err = a.columnArchive(ctx, p)
	default:
		return invalid("unsupported operation type")
	}
	return asRejection(err)
}

func (a *applier) emit(ctx context.Context, eventType string, payload any) error {
	ev, err := a.btx.AppendEvent(ctx, storage.EventInput{
		Type:    eventType,
		Payload: payload,
		OpID:    &a.opID,
		ActorID: &a.actor,
	})
	if err != nil {
		return err
	}
	a.res.primary = ev
	return nil
}

func (a *applier) emitExtra(ctx context.Context, eventType string, payload any) error {
	ev, err := a.btx.AppendEvent(ctx, storage.EventInput{
		Type:    eventType,
		Payload: payload,
		ActorID: &a.actor,
	})
	if err != nil {
		return err
	}
	a.res.extra = append(a.res.extra, ev)
	return nil
}

func requireID(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return invalid(field + ": is required")
	}
	return nil
}

func requireVersion(field string, v *int64) error {
	if v == nil {
		return invalid(field + ": is required")
	}
	if *v < 1 {
		return invalid(field + ": must be positive")
	}
	return nil
}

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
	description := ""
	if p.Description != nil {
		if err := domain.ValidateDescription(*p.Description); err != nil {
			return err
		}
		description = *p.Description
	}
	exists, err := a.btx.CardIDExists(ctx, p.CardID)
	if err != nil {
		return err
	}
	if exists {
		return invalid("card_id: is already in use")
	}
	col, err := a.btx.Column(ctx, p.ColumnID)
	if err != nil {
		return missingAsNotFound(err, "column not found")
	}
	pl, err := a.placeCard(ctx, col.ID, p.CardID, p.AfterID, p.BeforeID)
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

func (a *applier) cardEdit(ctx context.Context, p *protocol.CardEdit) error {
	if err := requireID("card_id", p.CardID); err != nil {
		return err
	}
	if p.Title == nil && p.Description == nil {
		return invalid("payload: title or description is required")
	}
	var edit storage.CardEdit
	if p.Title != nil {
		title, err := domain.NormalizeCardTitle(*p.Title)
		if err != nil {
			return err
		}
		edit.Title = &title
	}
	if p.Description != nil {
		if err := domain.ValidateDescription(*p.Description); err != nil {
			return err
		}
		if err := requireVersion("base_description_version", p.BaseDescriptionVersion); err != nil {
			return err
		}
		edit.Description = p.Description
	} else if p.BaseDescriptionVersion != nil {
		return invalid("base_description_version: is only allowed with description")
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
	pl, err := a.placeCard(ctx, col.ID, card.ID, p.AfterID, p.BeforeID)
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

func (a *applier) columnCreate(ctx context.Context, p *protocol.ColumnCreate) error {
	if err := requireID("column_id", p.ColumnID); err != nil {
		return err
	}
	title, err := domain.NormalizeColumnTitle(p.Title)
	if err != nil {
		return err
	}
	exists, err := a.btx.ColumnIDExists(ctx, p.ColumnID)
	if err != nil {
		return err
	}
	if exists {
		return invalid("column_id: is already in use")
	}
	pl, err := a.placeColumn(ctx, p.ColumnID, p.AfterID, p.BeforeID)
	if err != nil {
		return err
	}
	col, err := a.btx.InsertColumn(ctx, domain.Column{ID: p.ColumnID, Title: title, Rank: pl.rank})
	if err != nil {
		return fmt.Errorf("insert column: %w", err)
	}
	if err := a.emit(ctx, protocol.EventColumnCreated, protocol.ColumnCreated{Column: protocol.FromColumn(col), ActorID: a.actor}); err != nil {
		return err
	}
	return a.emitColumnRebalance(ctx, pl)
}

func (a *applier) columnRename(ctx context.Context, p *protocol.ColumnRename) error {
	if err := requireID("column_id", p.ColumnID); err != nil {
		return err
	}
	title, err := domain.NormalizeColumnTitle(p.Title)
	if err != nil {
		return err
	}
	col, err := a.btx.RenameColumn(ctx, p.ColumnID, title)
	if err != nil {
		return missingAsNotFound(err, "column not found")
	}
	return a.emit(ctx, protocol.EventColumnRenamed, protocol.ColumnRenamed{
		ColumnID: col.ID,
		Title:    col.Title,
		Version:  col.Version,
		ActorID:  a.actor,
	})
}

func (a *applier) columnMove(ctx context.Context, p *protocol.ColumnMove) error {
	if err := requireID("column_id", p.ColumnID); err != nil {
		return err
	}
	if err := requireVersion("base_version", p.BaseVersion); err != nil {
		return err
	}
	col, err := a.btx.Column(ctx, p.ColumnID)
	if err != nil {
		return missingAsNotFound(err, "column not found")
	}
	if *p.BaseVersion != col.Version {
		return conflict("column was changed by another user")
	}
	pl, err := a.placeColumn(ctx, col.ID, p.AfterID, p.BeforeID)
	if err != nil {
		return err
	}
	moved, err := a.btx.MoveColumn(ctx, col.ID, pl.rank)
	if err != nil {
		return fmt.Errorf("move column: %w", err)
	}
	if err := a.emit(ctx, protocol.EventColumnMoved, protocol.ColumnMoved{
		ColumnID: moved.ID,
		Rank:     moved.Rank,
		Version:  moved.Version,
		ActorID:  a.actor,
	}); err != nil {
		return err
	}
	return a.emitColumnRebalance(ctx, pl)
}

func (a *applier) columnArchive(ctx context.Context, p *protocol.ColumnArchive) error {
	if err := requireID("column_id", p.ColumnID); err != nil {
		return err
	}
	col, cardIDs, err := a.btx.ArchiveColumn(ctx, p.ColumnID, a.actor)
	if err != nil {
		return missingAsNotFound(err, "column not found")
	}
	return a.emit(ctx, protocol.EventColumnArchived, protocol.ColumnArchived{
		ColumnID: col.ID,
		CardIDs:  cardIDs,
		Version:  col.Version,
		ActorID:  a.actor,
	})
}

func (a *applier) placeCard(ctx context.Context, columnID, cardID uuid.UUID, after, before *uuid.UUID) (placement, error) {
	current, err := a.btx.CardRanks(ctx, columnID)
	if err != nil {
		return placement{}, err
	}
	pl, err := place(current, cardID, after, before)
	if err != nil {
		return placement{}, err
	}
	if err := a.btx.RerankCards(ctx, columnID, pl.rerank); err != nil {
		return placement{}, err
	}
	return pl, nil
}

func (a *applier) placeColumn(ctx context.Context, columnID uuid.UUID, after, before *uuid.UUID) (placement, error) {
	current, err := a.btx.ColumnRanks(ctx)
	if err != nil {
		return placement{}, err
	}
	pl, err := place(current, columnID, after, before)
	if err != nil {
		return placement{}, err
	}
	if err := a.btx.RerankColumns(ctx, pl.rerank); err != nil {
		return placement{}, err
	}
	return pl, nil
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

func (a *applier) emitColumnRebalance(ctx context.Context, pl placement) error {
	if pl.rebalance == nil {
		return nil
	}
	cols := make([]protocol.ColumnRank, len(pl.rebalance))
	for i, it := range pl.rebalance {
		cols[i] = protocol.ColumnRank{ColumnID: it.ID, Rank: it.Rank}
	}
	return a.emitExtra(ctx, protocol.EventColumnsRebalanced, protocol.ColumnsRebalanced{
		Columns: cols,
		ActorID: a.actor,
	})
}
