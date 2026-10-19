package storage

import (
	"context"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const userColumns = "id, email, password_hash, display_name, avatar_color, created_at"

func scanUser(row interface{ Scan(dest ...any) error }) (domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.AvatarColor, &u.CreatedAt)
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, u domain.User) (domain.User, error) {
	row := s.pool.QueryRow(ctx,
		`INSERT INTO users (id, email, password_hash, display_name, avatar_color)
		 VALUES ($1, $2, $3, $4, $5)
		 RETURNING `+userColumns,
		u.ID, u.Email, u.PasswordHash, u.DisplayName, u.AvatarColor)
	created, err := scanUser(row)
	if err != nil {
		if isUniqueViolation(err, "users_email_key") {
			return domain.User{}, domain.ErrEmailTaken
		}
		return domain.User{}, err
	}
	return created, nil
}

func (s *Store) GetUserByID(ctx context.Context, id uuid.UUID) (domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
	return u, notFound(err)
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE email = $1`, email))
	return u, notFound(err)
}

func (s *Store) UpdateUserProfile(ctx context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx,
		`UPDATE users
		 SET display_name = COALESCE($2, display_name),
		     avatar_color = COALESCE($3, avatar_color)
		 WHERE id = $1
		 RETURNING `+userColumns,
		id, upd.DisplayName, upd.AvatarColor))
	return u, notFound(err)
}
