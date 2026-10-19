package storage

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const refreshColumns = "id, user_id, family_id, token_hash, expires_at, revoked_at, created_at"

func scanRefreshToken(row pgx.Row) (domain.RefreshToken, error) {
	var t domain.RefreshToken
	err := row.Scan(&t.ID, &t.UserID, &t.FamilyID, &t.TokenHash, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt)
	return t, err
}

func insertRefreshToken(ctx context.Context, q Querier, t domain.RefreshToken) (domain.RefreshToken, error) {
	return scanRefreshToken(q.QueryRow(ctx,
		`INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+refreshColumns,
		t.ID, t.UserID, t.FamilyID, t.TokenHash, t.ExpiresAt))
}

func (s *Store) CreateRefreshToken(ctx context.Context, t domain.RefreshToken) (domain.RefreshToken, error) {
	return insertRefreshToken(ctx, s.pool, t)
}

func (s *Store) RotateRefreshToken(ctx context.Context, presentedHash string, next domain.RefreshToken, now time.Time) (domain.RefreshToken, error) {
	var (
		issued domain.RefreshToken
		reused bool
	)
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		current, err := scanRefreshToken(tx.QueryRow(ctx,
			`SELECT `+refreshColumns+` FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`,
			presentedHash))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrTokenInvalid
		}
		if err != nil {
			return err
		}
		if current.RevokedAt != nil {
			reused = true
			_, err := tx.Exec(ctx,
				`UPDATE refresh_tokens SET revoked_at = $2 WHERE family_id = $1 AND revoked_at IS NULL`,
				current.FamilyID, now)
			return err
		}
		if !now.Before(current.ExpiresAt) {
			return domain.ErrTokenExpired
		}
		if _, err := tx.Exec(ctx,
			`UPDATE refresh_tokens SET revoked_at = $2 WHERE id = $1`,
			current.ID, now); err != nil {
			return err
		}
		next.UserID = current.UserID
		next.FamilyID = current.FamilyID
		issued, err = insertRefreshToken(ctx, tx, next)
		return err
	})
	if err != nil {
		return domain.RefreshToken{}, err
	}
	if reused {
		return domain.RefreshToken{}, domain.ErrTokenReused
	}
	return issued, nil
}

func (s *Store) RevokeRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, now time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = $3
		 WHERE token_hash = $1 AND user_id = $2 AND revoked_at IS NULL`,
		tokenHash, userID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
