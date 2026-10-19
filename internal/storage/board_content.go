package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	columnColumns = "id, board_id, title, rank, version, archived_at"
	cardColumns   = "id, board_id, column_id, title, description, rank, version, description_version, created_by, updated_by, created_at, updated_at, archived_at"
)

type RankedItem struct {
	ID   uuid.UUID
	Rank string
}

type ProcessedOp struct {
	OpID    uuid.UUID
	BoardID uuid.UUID
	UserID  uuid.UUID
	Outcome json.RawMessage
}

func scanColumn(row pgx.Row) (domain.Column, error) {
	var c domain.Column
	err := row.Scan(&c.ID, &c.BoardID, &c.Title, &c.Rank, &c.Version, &c.ArchivedAt)
	return c, err
}

func scanCard(row pgx.Row) (domain.Card, error) {
	var c domain.Card
	err := row.Scan(&c.ID, &c.BoardID, &c.ColumnID, &c.Title, &c.Description, &c.Rank, &c.Version,
		&c.DescriptionVersion, &c.CreatedBy, &c.UpdatedBy, &c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt)
	return c, err
}

func (b *BoardTx) ProcessedOp(ctx context.Context, opID uuid.UUID) (ProcessedOp, bool, error) {
	var p ProcessedOp
	var outcome []byte
	err := b.tx.QueryRow(ctx,
		`SELECT op_id, board_id, user_id, outcome FROM processed_ops WHERE op_id = $1`,
		opID).Scan(&p.OpID, &p.BoardID, &p.UserID, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProcessedOp{}, false, nil
	}
	if err != nil {
		return ProcessedOp{}, false, err
	}
	p.Outcome = outcome
	return p, true, nil
}

func (b *BoardTx) SaveProcessedOp(ctx context.Context, opID, userID uuid.UUID, outcome any) error {
	data, err := json.Marshal(outcome)
	if err != nil {
		return fmt.Errorf("marshal op outcome: %w", err)
	}
	_, err = b.tx.Exec(ctx,
		`INSERT INTO processed_ops (op_id, board_id, user_id, outcome) VALUES ($1, $2, $3, $4)`,
		opID, b.board.ID, userID, data)
	return err
}

func (b *BoardTx) EventBySeq(ctx context.Context, seq int64) (domain.Event, error) {
	ev, err := scanEvent(b.tx.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM board_events WHERE board_id = $1 AND seq = $2`,
		b.board.ID, seq))
	return ev, noRowsAsNotFound(err)
}

func (b *BoardTx) Column(ctx context.Context, id uuid.UUID) (domain.Column, error) {
	c, err := scanColumn(b.tx.QueryRow(ctx,
		`SELECT `+columnColumns+` FROM columns WHERE id = $1 AND board_id = $2 AND archived_at IS NULL`,
		id, b.board.ID))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) Card(ctx context.Context, id uuid.UUID) (domain.Card, error) {
	c, err := scanCard(b.tx.QueryRow(ctx,
		`SELECT `+cardColumns+` FROM cards WHERE id = $1 AND board_id = $2 AND archived_at IS NULL`,
		id, b.board.ID))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) ColumnIDExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := b.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM columns WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

func (b *BoardTx) CardIDExists(ctx context.Context, id uuid.UUID) (bool, error) {
	var exists bool
	err := b.tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cards WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

func (b *BoardTx) ColumnRanks(ctx context.Context) ([]RankedItem, error) {
	return queryRanks(ctx, b.tx,
		`SELECT id, rank FROM columns WHERE board_id = $1 AND archived_at IS NULL ORDER BY rank, id`,
		b.board.ID)
}

func (b *BoardTx) CardRanks(ctx context.Context, columnID uuid.UUID) ([]RankedItem, error) {
	return queryRanks(ctx, b.tx,
		`SELECT id, rank FROM cards WHERE column_id = $1 AND board_id = $2 AND archived_at IS NULL ORDER BY rank, id`,
		columnID, b.board.ID)
}

func queryRanks(ctx context.Context, q Querier, sql string, args ...any) ([]RankedItem, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RankedItem, 0)
	for rows.Next() {
		var it RankedItem
		if err := rows.Scan(&it.ID, &it.Rank); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
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
		 WHERE id = $1 AND board_id = $2 AND archived_at IS NULL
		 RETURNING `+columnColumns,
		id, b.board.ID, title))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) MoveColumn(ctx context.Context, id uuid.UUID, rank string) (domain.Column, error) {
	c, err := scanColumn(b.tx.QueryRow(ctx,
		`UPDATE columns SET rank = $3, version = version + 1
		 WHERE id = $1 AND board_id = $2 AND archived_at IS NULL
		 RETURNING `+columnColumns,
		id, b.board.ID, rank))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) ArchiveColumn(ctx context.Context, id, actorID uuid.UUID) (domain.Column, []uuid.UUID, error) {
	c, err := scanColumn(b.tx.QueryRow(ctx,
		`UPDATE columns SET archived_at = now(), version = version + 1
		 WHERE id = $1 AND board_id = $2 AND archived_at IS NULL
		 RETURNING `+columnColumns,
		id, b.board.ID))
	if err != nil {
		return domain.Column{}, nil, noRowsAsNotFound(err)
	}
	rows, err := b.tx.Query(ctx,
		`UPDATE cards SET archived_at = now(), updated_at = now(), updated_by = $3, version = version + 1
		 WHERE column_id = $1 AND board_id = $2 AND archived_at IS NULL
		 RETURNING id`,
		id, b.board.ID, actorID)
	if err != nil {
		return domain.Column{}, nil, err
	}
	defer rows.Close()
	cardIDs := make([]uuid.UUID, 0)
	for rows.Next() {
		var cid uuid.UUID
		if err := rows.Scan(&cid); err != nil {
			return domain.Column{}, nil, err
		}
		cardIDs = append(cardIDs, cid)
	}
	return c, cardIDs, rows.Err()
}

func (b *BoardTx) InsertCard(ctx context.Context, c domain.Card) (domain.Card, error) {
	return scanCard(b.tx.QueryRow(ctx,
		`INSERT INTO cards (id, board_id, column_id, title, description, rank, created_by, updated_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
		 RETURNING `+cardColumns,
		c.ID, b.board.ID, c.ColumnID, c.Title, c.Description, c.Rank, c.CreatedBy))
}

type CardEdit struct {
	Title       *string
	Description *string
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
		 WHERE id = $1 AND board_id = $2 AND archived_at IS NULL
		 RETURNING `+cardColumns,
		id, b.board.ID, actorID, edit.Title, edit.Description))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) MoveCard(ctx context.Context, id, actorID, toColumnID uuid.UUID, rank string) (domain.Card, error) {
	c, err := scanCard(b.tx.QueryRow(ctx,
		`UPDATE cards SET column_id = $4, rank = $5, version = version + 1, updated_by = $3, updated_at = now()
		 WHERE id = $1 AND board_id = $2 AND archived_at IS NULL
		 RETURNING `+cardColumns,
		id, b.board.ID, actorID, toColumnID, rank))
	return c, noRowsAsNotFound(err)
}

func (b *BoardTx) ArchiveCard(ctx context.Context, id, actorID uuid.UUID) (domain.Card, error) {
	c, err := scanCard(b.tx.QueryRow(ctx,
		`UPDATE cards SET archived_at = now(), version = version + 1, updated_by = $3, updated_at = now()
		 WHERE id = $1 AND board_id = $2 AND archived_at IS NULL
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

func splitRanked(items []RankedItem) ([]uuid.UUID, []string) {
	ids := make([]uuid.UUID, len(items))
	ranks := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
		ranks[i] = it.Rank
	}
	return ids, ranks
}

func checkReranked(table string, affected int64, want int, err error) error {
	if err != nil {
		return fmt.Errorf("rerank %s: %w", table, err)
	}
	if affected != int64(want) {
		return fmt.Errorf("rerank %s: updated %d of %d rows", table, affected, want)
	}
	return nil
}

func (b *BoardTx) Savepoint(ctx context.Context, fn BoardTxFunc) error {
	sp, err := b.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin savepoint: %w", err)
	}
	child := &BoardTx{tx: sp, board: b.board}
	if err := fn(ctx, child); err != nil {
		if rbErr := sp.Rollback(ctx); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	b.board = child.board
	return nil
}
