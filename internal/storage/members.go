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

func (s *Store) BoardMembers(ctx context.Context, boardID, userID uuid.UUID) ([]domain.Member, error) {
	if _, err := s.BoardMember(ctx, boardID, userID); err != nil {
		return nil, err
	}
	return queryAll(ctx, s.pool, scanMember,
		`SELECT m.user_id, u.display_name, u.avatar_color, m.role, m.joined_at
		 FROM board_members m JOIN users u ON u.id = m.user_id
		 WHERE m.board_id = $1
		 ORDER BY m.joined_at, m.user_id`,
		boardID)
}

func (s *Store) BoardIDByInvite(ctx context.Context, tokenHash string) (uuid.UUID, error) {
	id, err := scanUUID(s.pool.QueryRow(ctx,
		`SELECT i.board_id FROM invites i JOIN boards b ON b.id = i.board_id
		 WHERE i.token_hash = $1 AND b.deleted_at IS NULL`,
		tokenHash))
	return id, noRowsAsNotFound(err)
}
