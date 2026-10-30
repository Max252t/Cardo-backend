package auth

import (
	"github.com/Max252t/Cardo-backend/internal/clock"
	"github.com/Max252t/Cardo-backend/internal/secrets"
)

type (
	Clock                = clock.Clock
	SystemClock          = clock.System
	RandomTokenGenerator = secrets.Generator
)

type TokenGenerator interface {
	NewToken() (string, error)
}

func HashToken(raw string) string {
	return secrets.Hash(raw)
}
