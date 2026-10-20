package storage

import (
	"context"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

func (s *Store) BoardMember(ctx context.Context, boardID, userID uuid.UUID) (domain.Member, error) {
	m, err := scanMember(s.pool.QueryRow(ctx,
		`SELECT m.user_id, u.display_name, u.avatar_color, m.role, m.joined_at
		 FROM board_members m
		 JOIN users u ON u.id = m.user_id
		 JOIN boards b ON b.id = m.board_id
		 WHERE m.board_id = $1 AND m.user_id = $2 AND b.deleted_at IS NULL`,
		boardID, userID))
	if err != nil {
		return domain.Member{}, noRowsAsNotFound(err)
	}
	return m, nil
}
