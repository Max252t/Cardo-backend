package storage

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const cardColumns = "id, board_id, column_id, title, description, rank, version, description_version, created_by, updated_by, created_at, updated_at, archived_at"

func scanCard(row pgx.Row) (domain.Card, error) {
	var c domain.Card
	err := row.Scan(&c.ID, &c.BoardID, &c.ColumnID, &c.Title, &c.Description, &c.Rank, &c.Version,
		&c.DescriptionVersion, &c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt)
	return c, err
}

type CardEdit struct {
	Title       *string
	Description *string
}

func (b *BoardTx) Card(ctx context.Context, id uuid.UUID) (domain.Card, error) {
	c, err := scanCard(b.tx.QueryRow(ctx,
		`SELECT `+cardColumns+` FROM cards WHERE `+activeByIDInBoard,
		id, b.board.ID))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) CardIDExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := b.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cards WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

func (b *BoardTx) CardRanks(ctx context.Context, columnID uuid.UUID) ([]RankedItem, error) {
	return queryAll(ctx, b.tx, scanRankedItem,
		`SELECT id, rank FROM cards WHERE column_id = $1 AND board_id = $2 AND archived_at IS NULL ORDER BY rank, id`,
		columnID, b.board.ID)
}

func (b *BoardTx) InsertCard(ctx context.Context, c domain.Card) (domain.Card, error) {
	return scanCard(b.tx.QueryRow(ctx,
		`INSERT INTO cards (id, board_id, column_id, title, description, rank, created_by, updated_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		 RETURNING `+cardColumns,
		c.ID, b.board.ID, c.ColumnID, c.Title, c.Description, c.Rank, c.CreatedBy))
}

func (b *BoardTx) EditCard(ctx context.Context, id, actorID uuid.UUID, edit CardEdit) (domain.Card, error) {
	c, err := scanCard(b.tx.QueryRow(ctx,
		`UPDATE cards SET
		   title = COALESCE($4, title),
		   description = COALESCE($5, description),
		   description_version = description_version + CASE WHEN $5::text IS NULL THEN 0 ELSE 1 END,
		   version = version + 1,
		   updated_by = $3,
		   updated_at = now()
		 WHERE `+activeByIDInBoard+`
		 RETURNING `+cardColumns,
		id, b.board.ID, actorID, edit.Title, edit.Description))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) MoveCard(ctx context.Context, id, actorID, toColumnID uuid.UUID, rank string) (domain.Card, error) {
	c, err := scanCard(b.tx.QueryRow(ctx,
		`UPDATE cards SET column_id = $4, rank = $5, version = version + 1, updated_by = $3, updated_at = now()
		 WHERE `+activeByIDInBoard+`
		 RETURNING `+cardColumns,
		id, b.board.ID, actorID, toColumnID, rank))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) ArchiveCard(ctx context.Context, id, actorID uuid.UUID) (domain.Card, error) {
	c, err := scanCard(b.tx.QueryRow(ctx,
		`UPDATE cards SET archived_at = now(), version = version + 1, updated_by = $3, updated_at = now()
		 WHERE `+activeByIDInBoard+`
		 RETURNING `+cardColumns,
		id, b.board.ID, actorID))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) RerankCards(ctx context.Context, columnID uuid.UUID, items []RankedItem) error {
	if len(items) == 0 {
		return nil
	}
	ids, ranks := splitRanked(items)
	if _, err := b.tx.Exec(ctx,
		`UPDATE cards SET rank = '~' || id::text
		 WHERE id = ANY($1) AND column_id = $2 AND board_id = $3 AND archived_at IS NULL`,
		ids, columnID, b.board.ID); err != nil {
		return fmt.Errorf("rerank cards: %w", err)
	}
	tag, err := b.tx.Exec(ctx,
		`UPDATE cards c SET rank = v.rank
		 FROM unnest($1::uuid[], $4::text[]) AS v(id, rank)
		 WHERE c.id = v.id AND c.column_id = $2 AND c.board_id = $3 AND c.archived_at IS NULL`,
		ids, columnID, b.board.ID, ranks)
	return checkReranked("cards", tag.RowsAffected(), len(items), err)
}
