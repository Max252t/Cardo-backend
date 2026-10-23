package storage

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const memberFromCTE = `SELECT c.user_id, u.display_name, u.avatar_color, c.role, c.joined_at
	 FROM changed c JOIN users u ON u.id = c.user_id`

const inviteColumns = "id, board_id, token_hash, role, created_by, expires_at, max_uses, uses, revoked_at"

func scanInvite(row pgx.Row) (domain.Invite, error) {
	var inv domain.Invite
	var role string
	err := row.Scan(&inv.ID, &inv.BoardID, &inv.TokenHash, &role, &inv.CreatedBy, &inv.ExpiresAt, &inv.MaxUses, &inv.Uses, &inv.RevokedAt)
	inv.Role = domain.Role(role)
	return inv, err
}

func (b *BoardTx) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := scanUser(b.tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email))
	return u, noRowsAsNotFound(err)
}

func (b *BoardTx) Member(ctx context.Context, userID uuid.UUID) (domain.Member, error) {
	m, err := scanMember(b.tx.QueryRow(ctx,
		`SELECT m.user_id, u.display_name, u.avatar_color, m.role, m.joined_at
		 FROM board_members m JOIN users u ON u.id = m.user_id
		 WHERE m.board_id = $1 AND m.user_id = $2`,
		b.board.ID, userID))
	return m, noRowsAsNotFound(err)
}

func (b *BoardTx) AddMember(ctx context.Context, userID uuid.UUID, role domain.Role) (domain.Member, error) {
	m, err := scanMember(b.tx.QueryRow(ctx,
		`WITH changed AS (
		   INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, $3)
		   RETURNING user_id, role, joined_at
		 ) `+memberFromCTE,
		b.board.ID, userID, string(role)))
	if err != nil {
		if isUniqueViolation(err, "board_members_pkey") {
			return domain.Member{}, domain.ErrAlreadyMember
		}
		return domain.Member{}, err
	}
	return m, nil
}

func (b *BoardTx) SetMemberRole(ctx context.Context, userID uuid.UUID, role domain.Role) (domain.Member, error) {
	m, err := scanMember(b.tx.QueryRow(ctx,
		`WITH changed AS (
		   UPDATE board_members SET role = $3 WHERE board_id = $1 AND user_id = $2
		   RETURNING user_id, role, joined_at
		 ) `+memberFromCTE,
		b.board.ID, userID, string(role)))
	return m, noRowsAsNotFound(err)
}

func (b *BoardTx) RemoveMember(ctx context.Context, userID uuid.UUID) error {
	tag, err := b.tx.Exec(ctx, `DELETE FROM board_members WHERE board_id = $1 AND user_id = $2`, b.board.ID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (b *BoardTx) InsertInvite(ctx context.Context, inv domain.Invite) error {
	_, err := b.tx.Exec(ctx,
		`INSERT INTO invites (id, board_id, token_hash, role, created_by, expires_at, max_uses)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		inv.ID, b.board.ID, inv.TokenHash, string(inv.Role), inv.CreatedBy, inv.ExpiresAt, inv.MaxUses)
	return err
}

func (b *BoardTx) InviteByHash(ctx context.Context, tokenHash string) (domain.Invite, error) {
	inv, err := scanInvite(b.tx.QueryRow(ctx,
		`SELECT `+inviteColumns+` FROM invites WHERE token_hash = $1 AND board_id = $2`,
		tokenHash, b.board.ID))
	return inv, noRowsAsNotFound(err)
}

func (b *BoardTx) ActiveInvites(ctx context.Context, now time.Time) ([]domain.Invite, error) {
	return queryAll(ctx, b.tx, scanInvite,
		`SELECT `+inviteColumns+` FROM invites
		 WHERE board_id = $1 AND revoked_at IS NULL AND expires_at > $2 AND uses < max_uses
		 ORDER BY expires_at, id`,
		b.board.ID, now)
}

func (b *BoardTx) RevokeInvite(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := b.tx.Exec(ctx,
		`UPDATE invites SET revoked_at = COALESCE(revoked_at, $3) WHERE id = $1 AND board_id = $2`,
		id, b.board.ID, at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (b *BoardTx) ConsumeInvite(ctx context.Context, id uuid.UUID) error {
	tag, err := b.tx.Exec(ctx,
		`UPDATE invites SET uses = uses + 1
		 WHERE id = $1 AND board_id = $2 AND uses < max_uses AND revoked_at IS NULL`,
		id, b.board.ID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrInviteUnavailable
	}
	return nil
}
