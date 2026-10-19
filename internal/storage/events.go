package storage

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const eventColumns = "board_id, seq, type, payload, op_id, actor_id, created_at"

func scanEvent(row pgx.Row) (domain.Event, error) {
	var ev domain.Event
	var payload []byte
	err := row.Scan(&ev.BoardID, &ev.Seq, &ev.Type, &payload, &ev.OpID, &ev.ActorID, &ev.CreatedAt)
	ev.Payload = payload
	return ev, err
}

func readOnlyRepeatable() pgx.TxOptions {
	return pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
}

func memberBoard(ctx context.Context, q Querier, boardID, userID uuid.UUID) (domain.BoardWithRole, error) {
	var role string
	b, err := scanBoard(q.QueryRow(ctx,
		`SELECT `+boardColumns+`, m.role
		 FROM boards b JOIN board_members m ON m.board_id = b.id
		 WHERE b.id = $1 AND m.user_id = $2 AND b.deleted_at IS NULL`,
		boardID, userID), &role)
	if err != nil {
		return domain.BoardWithRole{}, noRowsAsNotFound(err)
	}
	return domain.BoardWithRole{Board: b, Role: domain.Role(role)}, nil
}

func (s *Store) EventsAfter(ctx context.Context, boardID, userID uuid.UUID, afterSeq int64, limit int) (domain.EventPage, error) {
	var page domain.EventPage
	err := pgx.BeginTxFunc(ctx, s.pool, readOnlyRepeatable(), func(tx pgx.Tx) error {
		board, err := memberBoard(ctx, tx, boardID, userID)
		if err != nil {
			return err
		}
		page.Seq = board.Seq
		if afterSeq > board.Seq {
			return domain.NewValidationError("after_seq", "is greater than the current board seq")
		}
		if afterSeq == board.Seq {
			page.Events = []domain.Event{}
			return nil
		}
		var oldest *int64
		if err := tx.QueryRow(ctx,
			`SELECT min(seq) FROM board_events WHERE board_id = $1`, boardID).Scan(&oldest); err != nil {
			return err
		}
		if oldest == nil || *oldest > afterSeq+1 {
			return domain.ErrGone
		}
		rows, err := tx.Query(ctx,
			`SELECT `+eventColumns+` FROM board_events
			 WHERE board_id = $1 AND seq > $2
			 ORDER BY seq
			 LIMIT $3`,
			boardID, afterSeq, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		page.Events = make([]domain.Event, 0)
		for rows.Next() {
			ev, err := scanEvent(rows)
			if err != nil {
				return err
			}
			page.Events = append(page.Events, ev)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Events) > limit {
			page.Events = page.Events[:limit]
			page.HasMore = true
		}
		return nil
	})
	if err != nil {
		return domain.EventPage{}, err
	}
	return page, nil
}

func (s *Store) Snapshot(ctx context.Context, boardID, userID uuid.UUID) (domain.BoardSnapshot, error) {
	var snap domain.BoardSnapshot
	err := pgx.BeginTxFunc(ctx, s.pool, readOnlyRepeatable(), func(tx pgx.Tx) error {
		board, err := memberBoard(ctx, tx, boardID, userID)
		if err != nil {
			return err
		}
		snap.Board = board
		snap.Seq = board.Seq
		if snap.Columns, err = snapshotColumns(ctx, tx, boardID); err != nil {
			return err
		}
		if snap.Cards, err = snapshotCards(ctx, tx, boardID); err != nil {
			return err
		}
		snap.Members, err = boardMembers(ctx, tx, boardID)
		return err
	})
	if err != nil {
		return domain.BoardSnapshot{}, err
	}
	return snap, nil
}

func snapshotColumns(ctx context.Context, q Querier, boardID uuid.UUID) ([]domain.Column, error) {
	rows, err := q.Query(ctx,
		`SELECT `+columnColumns+` FROM columns
		 WHERE board_id = $1 AND archived_at IS NULL
		 ORDER BY rank, id`,
		boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Column, 0)
	for rows.Next() {
		c, err := scanColumn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func snapshotCards(ctx context.Context, q Querier, boardID uuid.UUID) ([]domain.Card, error) {
	rows, err := q.Query(ctx,
		`SELECT `+cardColumns+` FROM cards
		 WHERE board_id = $1 AND archived_at IS NULL
		 ORDER BY column_id, rank, id`,
		boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Card, 0)
	for rows.Next() {
		c, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func boardMembers(ctx context.Context, q Querier, boardID uuid.UUID) ([]domain.Member, error) {
	rows, err := q.Query(ctx,
		`SELECT m.user_id, u.display_name, u.avatar_color, m.role, m.joined_at
		 FROM board_members m JOIN users u ON u.id = m.user_id
		 WHERE m.board_id = $1
		 ORDER BY m.joined_at, m.user_id`,
		boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Member, 0)
	for rows.Next() {
		var m domain.Member
		var role string
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.AvatarColor, &role, &m.JoinedAt); err != nil {
			return nil, err
		}
		m.Role = domain.Role(role)
		out = append(out, m)
	}
	return out, rows.Err()
}
