package ops

import (
	"context"
	"errors"
	"fmt"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boardtx"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/google/uuid"
)

func (a *applier) columnCreate(ctx context.Context, p *boardcontent.ColumnCreate) error {
	if err := requireID("column_id", p.ColumnID); err != nil {
		return err
	}
	title, err := domain.NormalizeColumnTitle(p.Title)
	if err != nil {
		return err
	}
	if err := requireUnusedID(ctx, "column_id", p.ColumnID, a.btx.ColumnIDExists); err != nil {
		return err
	}
	pl, err := placeIn(ctx, a.columnRanks(), p.ColumnID, p.AfterID, p.BeforeID)
	if err != nil {
		return err
	}
	col, err := a.btx.InsertColumn(ctx, domain.Column{ID: p.ColumnID, Title: title, Rank: pl.rank})
	if errors.Is(err, domain.ErrDuplicateID) {
		return idUnavailable("column_id")
	}
	if err != nil {
		return fmt.Errorf("insert column: %w", err)
	}
	return a.emit(ctx, boardcontent.EventColumnCreated, boardcontent.ColumnCreated{Column: boardcontent.FromColumn(col), ActorID: a.actor})
}

func (a *applier) columnRename(ctx context.Context, p *boardcontent.ColumnRename) error {
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
	return a.emit(ctx, boardcontent.EventColumnRenamed, boardcontent.ColumnRenamed{
		ColumnID: col.ID,
		Title:    col.Title,
		Version:  col.Version,
		ActorID:  a.actor,
	})
}

func (a *applier) columnMove(ctx context.Context, p *boardcontent.ColumnMove) error {
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
	pl, err := placeIn(ctx, a.columnRanks(), col.ID, p.AfterID, p.BeforeID)
	if err != nil {
		return err
	}
	moved, err := a.btx.MoveColumn(ctx, col.ID, pl.rank)
	if err != nil {
		return fmt.Errorf("move column: %w", err)
	}
	return a.emit(ctx, boardcontent.EventColumnMoved, boardcontent.ColumnMoved{
		ColumnID: moved.ID,
		Rank:     moved.Rank,
		Version:  moved.Version,
		ActorID:  a.actor,
	})
}

func (a *applier) columnArchive(ctx context.Context, p *boardcontent.ColumnArchive) error {
	if err := requireID("column_id", p.ColumnID); err != nil {
		return err
	}
	col, cardIDs, err := a.btx.ArchiveColumn(ctx, p.ColumnID, a.actor)
	if err != nil {
		return missingAsNotFound(err, "column not found")
	}
	return a.emit(ctx, boardcontent.EventColumnArchived, boardcontent.ColumnArchived{
		ColumnID: col.ID,
		CardIDs:  cardIDs,
		Version:  col.Version,
		ActorID:  a.actor,
	})
}

func (a *applier) columnRanks() rankScope {
	return rankScope{
		probe:    columnProbe{btx: a.btx},
		load:     a.btx.ColumnRanks,
		rerank:   a.btx.RerankColumns,
		announce: a.emitColumnRebalance,
	}
}

func (a *applier) emitColumnRebalance(ctx context.Context, pl placement) error {
	if pl.rebalance == nil {
		return nil
	}
	cols := make([]boardcontent.ColumnRank, len(pl.rebalance))
	for i, it := range pl.rebalance {
		cols[i] = boardcontent.ColumnRank{ColumnID: it.ID, Rank: it.Rank}
	}
	return a.emitExtra(ctx, boardcontent.EventColumnsRebalanced, boardcontent.ColumnsRebalanced{
		Columns: cols,
		ActorID: a.actor,
	})
}

type columnProbe struct {
	btx boardtx.Tx
}

func (p columnProbe) window(ctx context.Context, anchor uuid.UUID, dir boardtx.RankDirection, exclude uuid.UUID) (string, string, bool, error) {
	return p.btx.ColumnRankWindow(ctx, anchor, dir, exclude)
}

func (p columnProbe) last(ctx context.Context, exclude uuid.UUID) (string, bool, error) {
	return p.btx.ColumnLastRank(ctx, exclude)
}
