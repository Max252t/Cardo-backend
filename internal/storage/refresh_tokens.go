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

func lockRefreshTokenByHash(ctx context.Context, q Querier, tokenHash string) (domain.RefreshToken, error) {
	t, err := scanRefreshToken(q.QueryRow(ctx,
		`SELECT `+refreshColumns+` FROM refresh_tokens WHERE token_hash = $1 FOR UPDATE`,
		tokenHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.RefreshToken{}, domain.ErrTokenInvalid
	}
	return t, err
}

func revokeRefreshTokenByID(ctx context.Context, q Querier, id uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = $2 WHERE id = $1`,
		id, now)
	return err
}

func revokeRefreshFamily(ctx context.Context, q Querier, familyID uuid.UUID, now time.Time) error {
	_, err := q.Exec(ctx,
		`UPDATE refresh_tokens SET revoked_at = $2 WHERE family_id = $1 AND revoked_at IS NULL`,
		familyID, now)
	return err
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
		current, err := lockRefreshTokenByHash(ctx, tx, presentedHash)
		if err != nil {
			return err
		}
		if current.RevokedAt != nil {
			reused = true
			return revokeRefreshFamily(ctx, tx, current.FamilyID, now)
		}
		if !now.Before(current.ExpiresAt) {
			return domain.ErrTokenExpired
		}
		if err := revokeRefreshTokenByID(ctx, tx, current.ID, now); err != nil {
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
		 WHERE revoked_at IS NULL
		   AND family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1 AND user_id = $2)`,
		tokenHash, userID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
