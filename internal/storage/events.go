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

func (b *BoardTx) EventBySeq(ctx context.Context, seq int64) (domain.Event, error) {
	ev, err := scanEvent(b.tx.QueryRow(ctx,
		`SELECT `+eventColumns+` FROM board_events WHERE board_id = $1 AND seq = $2`,
		b.board.ID, seq))
	return ev, noRowsAsNotFound(err)
}

func (s *Store) inReadOnlySnapshot(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

func memberBoard(ctx context.Context, q Querier, boardID, userID uuid.UUID) (domain.BoardWithRole, error) {
	b, err := scanBoardWithRole(q.QueryRow(ctx,
		`SELECT `+boardColumns+`, m.role
		 FROM boards b JOIN board_members m ON m.board_id = b.id
		 WHERE b.id = $1 AND m.user_id = $2 AND b.deleted_at IS NULL`,
		boardID, userID))
	if err != nil {
		return domain.BoardWithRole{}, noRowsAsNotFound(err)
	}
	return b, nil
}

func (s *Store) EventsAfter(ctx context.Context, boardID, userID uuid.UUID, afterSeq int64, limit int) (domain.EventPage, error) {
	var page domain.EventPage
	err := s.inReadOnlySnapshot(ctx, func(tx pgx.Tx) error {
		board, err := memberBoard(ctx, tx, boardID, userID)
		if err != nil {
			return err
		}
		page, err = eventPage(ctx, tx, board.Board, afterSeq, limit)
		return err
	})
	if err != nil {
		return domain.EventPage{}, err
	}
	return page, nil
}

func eventPage(ctx context.Context, q Querier, board domain.Board, afterSeq int64, limit int) (domain.EventPage, error) {
	if afterSeq > board.Seq {
		return domain.EventPage{}, domain.NewValidationError("after_seq", "is greater than the current board seq")
	}
	if afterSeq == board.Seq {
		return domain.EventPage{Events: []domain.Event{}, Seq: board.Seq}, nil
	}
	if err := ensureEventsRetained(ctx, q, board.ID, afterSeq); err != nil {
		return domain.EventPage{}, err
	}
	events, err := queryAll(ctx, q, scanEvent,
		`SELECT `+eventColumns+` FROM board_events
		 WHERE board_id = $1 AND seq > $2
		 ORDER BY seq
		 LIMIT $3`,
		board.ID, afterSeq, limit+1)
	if err != nil {
		return domain.EventPage{}, err
	}
	page := domain.EventPage{Events: events, Seq: board.Seq}
	if len(events) > limit {
		page.Events = events[:limit]
		page.HasMore = true
	}
	return page, nil
}

func ensureEventsRetained(ctx context.Context, q Querier, boardID uuid.UUID, afterSeq int64) error {
	var oldest *int64
	if err := q.QueryRow(ctx,
		`SELECT min(seq) FROM board_events WHERE board_id = $1`, boardID).Scan(&oldest); err != nil {
		return err
	}
	if oldest == nil || *oldest > afterSeq+1 {
		return domain.ErrGone
	}
	return nil
}
