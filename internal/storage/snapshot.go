package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

func (s *Store) Snapshot(ctx context.Context, boardID, userID uuid.UUID) (domain.BoardSnapshot, error) {
	var snap domain.BoardSnapshot
	err := s.inReadOnlySnapshot(ctx, func(tx pgx.Tx) error {
		board, err := memberBoard(ctx, tx, boardID, userID)
		if err != nil {
			return err
		}
		snap, err = loadSnapshot(ctx, tx, board)
		return err
	})
	if err != nil {
		return domain.BoardSnapshot{}, err
	}
	return snap, nil
}

func loadSnapshot(ctx context.Context, q Querier, board domain.BoardWithRole) (domain.BoardSnapshot, error) {
	columns, err := queryAll(ctx, q, scanColumn,
		`SELECT `+columnColumns+` FROM columns
		 WHERE board_id = $1 AND archived_at IS NULL
		 ORDER BY rank, id`,
		board.ID)
	if err != nil {
		return domain.BoardSnapshot{}, err
	}
	cards, err := queryAll(ctx, q, scanCard,
		`SELECT `+cardColumns+` FROM cards
		 WHERE board_id = $1 AND archived_at IS NULL
		 ORDER BY column_id, rank, id`,
		board.ID)
	if err != nil {
		return domain.BoardSnapshot{}, err
	}
	members, err := queryAll(ctx, q, scanMember,
		`SELECT m.user_id, u.display_name, u.avatar_color, m.role, m.joined_at
		 FROM board_members m JOIN users u ON u.id = m.user_id
		 WHERE m.board_id = $1
		 ORDER BY m.joined_at, m.user_id`,
		board.ID)
	if err != nil {
		return domain.BoardSnapshot{}, err
	}
	return domain.BoardSnapshot{Board: board, Columns: columns, Cards: cards, Members: members, Seq: board.Seq}, nil
}

func scanMember(row pgx.Row) (domain.Member, error) {
	var m domain.Member
	var role string
	err := row.Scan(&m.UserID, &m.DisplayName, &m.AvatarColor, &role, &m.JoinedAt)
	m.Role = domain.Role(role)
	return m, err
}
