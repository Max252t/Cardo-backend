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

type EventInput struct {
	Type    string
	Payload any
	OpID    *uuid.UUID
	ActorID *uuid.UUID
}

type BoardTxFunc func(ctx context.Context, btx *BoardTx) error

type BoardTx struct {
	tx    pgx.Tx
	board domain.Board
}

func (s *Store) WithBoardLock(ctx context.Context, boardID uuid.UUID, fn BoardTxFunc) error {
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, durationMillis(s.lockTimeout)); err != nil {
			return fmt.Errorf("set lock timeout: %w", err)
		}
		board, err := scanBoard(tx.QueryRow(ctx,
			`SELECT `+boardColumns+` FROM boards b WHERE b.id = $1 AND b.deleted_at IS NULL FOR UPDATE`,
			boardID))
		if err != nil {
			return noRowsAsNotFound(err)
		}
		return fn(ctx, &BoardTx{tx: tx, board: board})
	})
	return lockTimeoutAsBusy(err)
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

func (b *BoardTx) Tx() pgx.Tx {
	return b.tx
}

func (b *BoardTx) Board() domain.Board {
	return b.board
}

func (b *BoardTx) MemberRole(ctx context.Context, userID uuid.UUID) (domain.Role, error) {
	var role string
	err := b.tx.QueryRow(ctx,
		`SELECT role FROM board_members WHERE board_id = $1 AND user_id = $2`,
		b.board.ID, userID).Scan(&role)
	if err != nil {
		return "", noRowsAsNotFound(err)
	}
	return domain.Role(role), nil
}

func (b *BoardTx) RequireRole(ctx context.Context, userID uuid.UUID, min domain.Role) (domain.Role, error) {
	role, err := b.MemberRole(ctx, userID)
	if err != nil {
		return "", err
	}
	if !role.AtLeast(min) {
		return role, domain.ErrForbidden
	}
	return role, nil
}

func (b *BoardTx) AppendEvent(ctx context.Context, in EventInput) (domain.Event, error) {
	if in.Type == "" {
		return domain.Event{}, errors.New("event type is required")
	}
	payload, err := marshalPayload(in.Payload)
	if err != nil {
		return domain.Event{}, err
	}
	seq, err := b.nextSeq(ctx)
	if err != nil {
		return domain.Event{}, err
	}
	ev := domain.Event{
		BoardID: b.board.ID,
		Seq:     seq,
		Type:    in.Type,
		Payload: payload,
		OpID:    in.OpID,
		ActorID: in.ActorID,
	}
	if err := b.tx.QueryRow(ctx,
		`INSERT INTO board_events (board_id, seq, type, payload, op_id, actor_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING created_at`,
		ev.BoardID, ev.Seq, ev.Type, []byte(ev.Payload), ev.OpID, ev.ActorID).Scan(&ev.CreatedAt); err != nil {
		return domain.Event{}, fmt.Errorf("insert board event: %w", err)
	}
	b.board.Seq = seq
	return ev, nil
}

func (b *BoardTx) nextSeq(ctx context.Context) (int64, error) {
	var seq int64
	if err := b.tx.QueryRow(ctx,
		`UPDATE boards SET seq = seq + 1, updated_at = now() WHERE id = $1 RETURNING seq, updated_at`,
		b.board.ID).Scan(&seq, &b.board.UpdatedAt); err != nil {
		return 0, fmt.Errorf("increment board seq: %w", err)
	}
	return seq, nil
}

func (b *BoardTx) SetTitle(ctx context.Context, title string) error {
	return b.tx.QueryRow(ctx,
		`UPDATE boards SET title = $2, updated_at = now() WHERE id = $1 RETURNING title, updated_at`,
		b.board.ID, title).Scan(&b.board.Title, &b.board.UpdatedAt)
}

func (b *BoardTx) MarkDeleted(ctx context.Context) error {
	return b.tx.QueryRow(ctx,
		`UPDATE boards SET deleted_at = now(), updated_at = now() WHERE id = $1 RETURNING deleted_at, updated_at`,
		b.board.ID).Scan(&b.board.DeletedAt, &b.board.UpdatedAt)
}

func marshalPayload(p any) (json.RawMessage, error) {
	switch v := p.(type) {
	case nil:
		return json.RawMessage("{}"), nil
	case json.RawMessage:
		if len(v) == 0 {
			return json.RawMessage("{}"), nil
		}
		return v, nil
	}
	data, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("marshal event payload: %w", err)
	}
	return data, nil
}
