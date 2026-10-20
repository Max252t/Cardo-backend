package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

type Reservation struct {
	Allowed    bool
	Attempts   int
	RetryAfter time.Duration
}

type LoginLimiter interface {
	Reserve(ctx context.Context, key string) (Reservation, error)
	Reset(ctx context.Context, key string) error
}

func LoginKey(ip, email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email)) + "|" + ip))
	return hex.EncodeToString(sum[:])
}
