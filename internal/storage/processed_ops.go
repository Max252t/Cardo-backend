package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (b *BoardTx) ProcessedOp(ctx context.Context, opID uuid.UUID) (domain.ProcessedOp, bool, error) {
	var p domain.ProcessedOp
	var outcome []byte
	err := b.tx.QueryRow(ctx,
		`SELECT op_id, board_id, user_id, outcome FROM processed_ops WHERE op_id = $1`,
		opID).Scan(&p.OpID, &p.BoardID, &p.UserID, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ProcessedOp{}, false, nil
	}
	if err != nil {
		return domain.ProcessedOp{}, false, err
	}
	p.Outcome = outcome
	return p, true, nil
}

func (b *BoardTx) SaveProcessedOp(ctx context.Context, opID, userID uuid.UUID, outcome any) (bool, error) {
	data, err := json.Marshal(outcome)
	if err != nil {
		return false, fmt.Errorf("marshal op outcome: %w", err)
	}
	tag, err := b.tx.Exec(ctx,
		`INSERT INTO processed_ops (op_id, board_id, user_id, outcome) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (op_id) DO NOTHING`,
		opID, b.board.ID, userID, data)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
