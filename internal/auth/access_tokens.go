package auth

import (
	"time"

	"github.com/google/uuid"
)

type AccessTokens interface {
	Issue(userID uuid.UUID) (string, time.Time, error)
	Verify(token string) (uuid.UUID, error)
}
