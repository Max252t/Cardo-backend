package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const boardColumns = "b.id, b.title, b.owner_id, b.seq, b.created_at, b.updated_at, b.deleted_at"

func scanBoard(row pgx.Row, extra ...any) (domain.Board, error) {
	var b domain.Board
	dest := append([]any{&b.ID, &b.Title, &b.OwnerID, &b.Seq, &b.CreatedAt, &b.UpdatedAt, &b.DeletedAt}, extra...)
	err := row.Scan(dest...)
	return b, err
}

func scanBoardWithRole(row pgx.Row) (domain.BoardWithRole, error) {
	var role string
	b, err := scanBoard(row, &role)
	return domain.BoardWithRole{Board: b, Role: domain.Role(role)}, err
}

func (s *Store) CreateBoardWithOwner(ctx context.Context, b domain.Board) (domain.BoardWithRole, error) {
	var created domain.Board
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		created, err = scanBoard(tx.QueryRow(ctx,
			`INSERT INTO boards AS b (id, title, owner_id) VALUES ($1, $2, $3)
			 RETURNING `+boardColumns,
			b.ID, b.Title, b.OwnerID))
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, $3)`,
			created.ID, created.OwnerID, string(domain.RoleOwner))
		return err
	})
	if err != nil {
		return domain.BoardWithRole{}, err
	}
	return domain.BoardWithRole{Board: created, Role: domain.RoleOwner}, nil
}

func (s *Store) ListBoardsForUser(ctx context.Context, userID uuid.UUID) ([]domain.BoardWithRole, error) {
	return queryAll(ctx, s.pool, scanBoardWithRole,
		`SELECT `+boardColumns+`, m.role
		 FROM boards b
		 JOIN board_members m ON m.board_id = b.id
		 WHERE m.user_id = $1 AND b.deleted_at IS NULL
		 ORDER BY b.created_at DESC, b.id`,
		userID)
}
