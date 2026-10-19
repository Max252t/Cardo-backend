package auth_test

import (
	"encoding/base64"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
)

func TestHashToken(t *testing.T) {
	t.Parallel()
	h := auth.HashToken("abc")
	assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", h)
	assert.Equal(t, h, auth.HashToken("abc"))
	assert.NotEqual(t, h, auth.HashToken("abd"))
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), auth.HashToken(""))
}

func TestRandomTokenGenerator(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		bytes    int
		wantSize int
	}{
		{"default size", 0, 32},
		{"negative falls back to default", -1, 32},
		{"custom size", 48, 48},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := auth.RandomTokenGenerator{Bytes: tt.bytes}
			tok, err := g.NewToken()
			require.NoError(t, err)
			raw, err := base64.RawURLEncoding.DecodeString(tok)
			require.NoError(t, err)
			assert.Len(t, raw, tt.wantSize)
		})
	}

	g := auth.RandomTokenGenerator{}
	seen := map[string]struct{}{}
	for range 200 {
		tok, err := g.NewToken()
		require.NoError(t, err)
		_, dup := seen[tok]
		require.False(t, dup, "duplicate token generated")
		seen[tok] = struct{}{}
	}
}

func TestLoginKey(t *testing.T) {
	t.Parallel()
	base := auth.LoginKey("1.2.3.4", "user@example.com")
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), base)
	assert.Equal(t, base, auth.LoginKey("1.2.3.4", "user@example.com"))
	assert.Equal(t, base, auth.LoginKey("1.2.3.4", "  USER@Example.com "))
	assert.NotEqual(t, base, auth.LoginKey("1.2.3.5", "user@example.com"))
	assert.NotEqual(t, base, auth.LoginKey("1.2.3.4", "other@example.com"))
	assert.NotContains(t, base, "user")
}

func TestSystemClock(t *testing.T) {
	t.Parallel()
	assert.WithinDuration(t, time.Now(), auth.SystemClock{}.Now(), 5*time.Second)
}
