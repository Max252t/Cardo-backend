package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const columnColumns = "id, board_id, title, rank, version, archived_at"

func scanColumn(row pgx.Row) (domain.Column, error) {
	var c domain.Column
	err := row.Scan(&c.ID, &c.BoardID, &c.Title, &c.Rank, &c.Version, &c.ArchivedAt)
	return c, err
}

func (b *BoardTx) Column(ctx context.Context, id uuid.UUID) (domain.Column, error) {
	c, err := scanColumn(b.tx.QueryRow(ctx,
		`SELECT `+columnColumns+` FROM columns WHERE `+activeByIDInBoard,
		id, b.board.ID))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) ColumnIDExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := b.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM columns WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

func (b *BoardTx) ColumnRanks(ctx context.Context) ([]RankedItem, error) {
	return queryAll(ctx, b.tx, scanRankedItem,
		`SELECT id, rank FROM columns WHERE board_id = $1 AND archived_at IS NULL ORDER BY rank, id`,
		b.board.ID)
}

func (b *BoardTx) InsertColumn(ctx context.Context, c domain.Column) (domain.Column, error) {
	return scanColumn(b.tx.QueryRow(ctx,
		`INSERT INTO columns (id, board_id, title, rank) VALUES ($1, $2, $3, $4)
		 RETURNING `+columnColumns,
		c.ID, b.board.ID, c.Title, c.Rank))
}

func (b *BoardTx) RenameColumn(ctx context.Context, id uuid.UUID, title string) (domain.Column, error) {
	c, err := scanColumn(b.tx.QueryRow(ctx,
		`UPDATE columns SET title = $3, version = version + 1
		 WHERE `+activeByIDInBoard+`
		 RETURNING `+columnColumns,
		id, b.board.ID, title))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) MoveColumn(ctx context.Context, id uuid.UUID, rank string) (domain.Column, error) {
	c, err := scanColumn(b.tx.QueryRow(ctx,
		`UPDATE columns SET rank = $3, version = version + 1
		 WHERE `+activeByIDInBoard+`
		 RETURNING `+columnColumns,
		id, b.board.ID, rank))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) ArchiveColumn(ctx context.Context, id, actorID uuid.UUID) (domain.Column, []uuid.UUID, error) {
	c, err := scanColumn(b.tx.QueryRow(ctx,
		`UPDATE columns SET archived_at = now(), version = version + 1
		 WHERE `+activeByIDInBoard+`
		 RETURNING `+columnColumns,
		id, b.board.ID))
	if err != nil {
		return domain.Column{}, nil, noRowsAsNotFound(err)
	}
	cardIDs, err := b.archiveColumnCards(ctx, id, actorID)
	if err != nil {
		return domain.Column{}, nil, err
	}
	return c, cardIDs, nil
}

func (b *BoardTx) archiveColumnCards(ctx context.Context, columnID, actorID uuid.UUID) ([]uuid.UUID, error) {
	return queryAll(ctx, b.tx, scanUUID,
		`UPDATE cards SET archived_at = now(), updated_at = now(), updated_by = $3, version = version + 1
		 WHERE column_id = $1 AND board_id = $2 AND archived_at IS NULL
		 RETURNING id`,
		columnID, b.board.ID, actorID)
}

func (b *BoardTx) RerankColumns(ctx context.Context, items []RankedItem) error {
	if len(items) == 0 {
		return nil
	}
	ids, ranks := splitRanked(items)
	if _, err := b.tx.Exec(ctx,
		`UPDATE columns SET rank = '~' || id::text
		 WHERE id = ANY($1) AND board_id = $2 AND archived_at IS NULL`,
		ids, b.board.ID); err != nil {
		return fmt.Errorf("rerank columns: %w", err)
	}
	tag, err := b.tx.Exec(ctx,
		`UPDATE columns c SET rank = v.rank
		 FROM unnest($1::uuid[], $3::text[]) AS v(id, rank)
		 WHERE c.id = v.id AND c.board_id = $2 AND c.archived_at IS NULL`,
		ids, b.board.ID, ranks)
	return checkReranked("columns", tag.RowsAffected(), len(items), err)
}
