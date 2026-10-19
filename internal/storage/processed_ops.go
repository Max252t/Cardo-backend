package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ProcessedOp struct {
	OpID    uuid.UUID
	BoardID uuid.UUID
	UserID  uuid.UUID
	Outcome json.RawMessage
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
