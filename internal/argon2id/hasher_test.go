package argon2id_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/argon2id"
)

func TestArgon2HashFormat(t *testing.T) {
	t.Parallel()
	h := argon2id.New(fastArgon2)
	encoded, err := h.Hash(context.Background(), "correct horse")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(encoded, "$argon2id$v=19$m=8,t=1,p=1$"), encoded)
	assert.Len(t, strings.Split(encoded, "$"), 6)
	assert.NotContains(t, encoded, "correct horse")
}

func TestArgon2HashVerify(t *testing.T) {
	t.Parallel()
	h := argon2id.New(fastArgon2)
	encoded, err := h.Hash(context.Background(), "correct horse")
	require.NoError(t, err)

	tests := []struct {
		name     string
		password string
		want     bool
	}{
		{"correct", "correct horse", true},
		{"wrong", "wrong horse", false},
		{"empty", "", false},
		{"case differs", "Correct horse", false},
		{"prefix", "correct hors", false},
		{"suffix", "correct horse ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok, err := h.Verify(context.Background(), tt.password, encoded)
			require.NoError(t, err)
			assert.Equal(t, tt.want, ok)
		})
	}
}

func TestArgon2HashIsSalted(t *testing.T) {
	t.Parallel()
	h := argon2id.New(fastArgon2)
	a, err := h.Hash(context.Background(), "same-password")
	require.NoError(t, err)
	b, err := h.Hash(context.Background(), "same-password")
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

func TestArgon2VerifyUsesParamsFromEncodedHash(t *testing.T) {
	t.Parallel()
	encoded, err := argon2id.New(fastArgon2).Hash(context.Background(), "pw-pw-pw-pw")
	require.NoError(t, err)

	other := argon2id.New(argon2id.Params{Memory: 16, Iterations: 3, Parallelism: 2, SaltLength: 16, KeyLength: 32})
	ok, err := other.Verify(context.Background(), "pw-pw-pw-pw", encoded)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestArgon2VerifyRejectsMalformedHashes(t *testing.T) {
	t.Parallel()
	h := argon2id.New(fastArgon2)
	valid, err := h.Hash(context.Background(), "password")
	require.NoError(t, err)
	parts := strings.Split(valid, "$")

	join := func(p []string) string { return strings.Join(p, "$") }
	with := func(i int, v string) string {
		c := append([]string(nil), parts...)
		c[i] = v
		return join(c)
	}

	tests := []struct {
		name    string
		encoded string
	}{
		{"empty", ""},
		{"garbage", "not-a-hash"},
		{"too few parts", "$argon2id$v=19$m=8,t=1,p=1$c2FsdA"},
		{"too many parts", valid + "$extra"},
		{"wrong algorithm", with(1, "argon2i")},
		{"bcrypt", "$2a$12$abcdefghijklmnopqrstuuabcdefghijklmnopqrstuvwxyz0123"},
		{"wrong version", with(2, "v=18")},
		{"bad version", with(2, "version")},
		{"bad params", with(3, "m=x,t=1,p=1")},
		{"zero memory", with(3, "m=0,t=1,p=1")},
		{"zero iterations", with(3, "m=8,t=0,p=1")},
		{"zero parallelism", with(3, "m=8,t=1,p=0")},
		{"bad salt base64", with(4, "!!!")},
		{"bad key base64", with(5, "!!!")},
		{"empty key", with(5, "")},
		{"oversized key", with(5, strings.Repeat("A", 2000))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ok, err := h.Verify(context.Background(), "password", tt.encoded)
			assert.ErrorIs(t, err, argon2id.ErrInvalidHash)
			assert.False(t, ok)
		})
	}
}

func TestArgon2DefaultParamsAreOWASPMinimum(t *testing.T) {
	t.Parallel()
	p := argon2id.DefaultParams
	assert.GreaterOrEqual(t, p.Memory, uint32(19*1024))
	assert.GreaterOrEqual(t, p.Iterations, uint32(2))
	assert.GreaterOrEqual(t, p.Parallelism, uint8(1))
	assert.GreaterOrEqual(t, p.SaltLength, uint32(16))
	assert.GreaterOrEqual(t, p.KeyLength, uint32(32))
}

var fastArgon2 = argon2id.Params{
	Memory:      8,
	Iterations:  1,
	Parallelism: 1,
	SaltLength:  8,
	KeyLength:   16,
}
