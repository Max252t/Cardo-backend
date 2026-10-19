package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

var (
	jwtSecret = []byte("0123456789abcdef0123456789abcdef")
	jwtTTL    = 15 * time.Minute
)

func newJWT(clock auth.Clock) *auth.JWTManager {
	return auth.NewJWTManager(auth.JWTConfig{
		Secret:   jwtSecret,
		Issuer:   "cardo",
		Audience: "cardo-api",
		TTL:      jwtTTL,
	}, clock)
}

func signClaims(t *testing.T, method jwt.SigningMethod, key any, claims jwt.Claims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(method, claims).SignedString(key)
	require.NoError(t, err)
	return s
}

func validClaims(userID uuid.UUID) jwt.RegisteredClaims {
	return jwt.RegisteredClaims{
		Subject:   userID.String(),
		Issuer:    "cardo",
		Audience:  jwt.ClaimStrings{"cardo-api"},
		IssuedAt:  jwt.NewNumericDate(testEpoch),
		NotBefore: jwt.NewNumericDate(testEpoch),
		ExpiresAt: jwt.NewNumericDate(testEpoch.Add(jwtTTL)),
		ID:        uuid.NewString(),
	}
}

func TestJWTIssueVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	m := newJWT(clock)
	userID := uuid.New()

	token, exp, err := m.Issue(userID)
	require.NoError(t, err)
	assert.Equal(t, testEpoch.Add(jwtTTL), exp)

	got, err := m.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, userID, got)
}

func TestJWTIssueClaims(t *testing.T) {
	t.Parallel()
	m := newJWT(newFakeClock())
	userID := uuid.New()
	token, _, err := m.Issue(userID)
	require.NoError(t, err)

	claims := jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) { return jwtSecret, nil },
		jwt.WithTimeFunc(func() time.Time { return testEpoch }))
	require.NoError(t, err)
	assert.Equal(t, "HS256", parsed.Method.Alg())
	assert.Equal(t, userID.String(), claims.Subject)
	assert.Equal(t, "cardo", claims.Issuer)
	assert.Equal(t, jwt.ClaimStrings{"cardo-api"}, claims.Audience)
	assert.Equal(t, testEpoch.Add(jwtTTL).Unix(), claims.ExpiresAt.Unix())
	assert.Equal(t, testEpoch.Unix(), claims.IssuedAt.Unix())
	assert.NotEmpty(t, claims.ID)
}

func TestJWTIssueUniqueIDs(t *testing.T) {
	t.Parallel()
	m := newJWT(newFakeClock())
	userID := uuid.New()
	a, _, err := m.Issue(userID)
	require.NoError(t, err)
	b, _, err := m.Issue(userID)
	require.NoError(t, err)
	assert.NotEqual(t, a, b)
}

func TestJWTExpiry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		advance time.Duration
		wantErr error
	}{
		{"fresh", 0, nil},
		{"one second before expiry", jwtTTL - time.Second, nil},
		{"one second after expiry", jwtTTL + time.Second, domain.ErrTokenExpired},
		{"long after expiry", 24 * time.Hour, domain.ErrTokenExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			clock := newFakeClock()
			m := newJWT(clock)
			userID := uuid.New()
			token, _, err := m.Issue(userID)
			require.NoError(t, err)

			clock.Advance(tt.advance)
			got, err := m.Verify(token)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				assert.Equal(t, uuid.Nil, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, userID, got)
		})
	}
}

func TestJWTNotYetValid(t *testing.T) {
	t.Parallel()
	issuerClock := newFakeClock()
	issuerClock.Advance(time.Hour)
	token, _, err := newJWT(issuerClock).Issue(uuid.New())
	require.NoError(t, err)

	got, err := newJWT(newFakeClock()).Verify(token)
	assert.ErrorIs(t, err, domain.ErrTokenInvalid)
	assert.Equal(t, uuid.Nil, got)
}

func TestJWTRejectsInvalidTokens(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	valid := validClaims(userID)

	withMut := func(mut func(*jwt.RegisteredClaims)) jwt.RegisteredClaims {
		c := valid
		mut(&c)
		return c
	}

	goodToken := signClaims(t, jwt.SigningMethodHS256, jwtSecret, valid)
	parts := strings.Split(goodToken, ".")
	require.Len(t, parts, 3)

	forgedPayload := signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) {
		c.Subject = uuid.NewString()
	}))
	forgedParts := strings.Split(forgedPayload, ".")

	noneToken := signClaims(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid)

	tests := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not.a.jwt"},
		{"single segment", "abc"},
		{"wrong secret", signClaims(t, jwt.SigningMethodHS256, []byte("another-secret-another-secret-12345"), valid)},
		{"wrong algorithm HS384", signClaims(t, jwt.SigningMethodHS384, jwtSecret, valid)},
		{"wrong algorithm HS512", signClaims(t, jwt.SigningMethodHS512, jwtSecret, valid)},
		{"alg none", noneToken},
		{"alg none with trailing dot stripped", strings.TrimSuffix(noneToken, ".")},
		{"wrong issuer", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.Issuer = "evil" }))},
		{"missing issuer", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.Issuer = "" }))},
		{"wrong audience", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.Audience = jwt.ClaimStrings{"other"} }))},
		{"missing audience", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.Audience = nil }))},
		{"missing expiry", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.ExpiresAt = nil }))},
		{"subject not a uuid", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.Subject = "alice" }))},
		{"empty subject", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.Subject = "" }))},
		{"nil uuid subject", signClaims(t, jwt.SigningMethodHS256, jwtSecret, withMut(func(c *jwt.RegisteredClaims) { c.Subject = uuid.Nil.String() }))},
		{"tampered signature", parts[0] + "." + parts[1] + "." + flipFirst(parts[2])},
		{"stripped signature", parts[0] + "." + parts[1] + "."},
		{"payload swapped keeping old signature", forgedParts[0] + "." + forgedParts[1] + "." + parts[2]},
		{"extra segment", goodToken + ".extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := newJWT(newFakeClock()).Verify(tt.token)
			assert.ErrorIs(t, err, domain.ErrTokenInvalid)
			assert.Equal(t, uuid.Nil, got)
		})
	}

	got, err := newJWT(newFakeClock()).Verify(goodToken)
	require.NoError(t, err)
	assert.Equal(t, userID, got)
}

func TestJWTAcceptsMultiValueAudienceContainingExpected(t *testing.T) {
	t.Parallel()
	userID := uuid.New()
	c := validClaims(userID)
	c.Audience = jwt.ClaimStrings{"someone-else", "cardo-api"}
	token := signClaims(t, jwt.SigningMethodHS256, jwtSecret, c)

	got, err := newJWT(newFakeClock()).Verify(token)
	require.NoError(t, err)
	assert.Equal(t, userID, got)
}

func TestJWTNilClockUsesSystemClock(t *testing.T) {
	t.Parallel()
	m := newJWT(nil)
	userID := uuid.New()
	token, exp, err := m.Issue(userID)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now().Add(jwtTTL), exp, 5*time.Second)
	got, err := m.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, userID, got)
}

func flipFirst(s string) string {
	if s == "" {
		return s
	}
	repl := byte('A')
	if s[0] == 'A' {
		repl = 'B'
	}
	return string(repl) + s[1:]
}
