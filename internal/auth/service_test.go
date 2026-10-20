package auth_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/ratelimit"
)

const refreshTTL = 7 * 24 * time.Hour

type fixture struct {
	svc     *auth.Service
	store   *fakeStore
	clock   *fakeClock
	limiter *ratelimit.MemoryLimiter
	jwt     *auth.JWTManager
	hasher  *auth.Argon2Hasher
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clock := newFakeClock()
	store := newFakeStore()
	limiter := ratelimit.NewMemoryLimiter(5, time.Minute, clock)
	jwtm := newJWT(clock)
	hasher := auth.NewArgon2Hasher(fastArgon2)
	svc := auth.NewService(auth.Deps{
		Store:      store,
		Hasher:     hasher,
		Access:     jwtm,
		Limiter:    limiter,
		Clock:      clock,
		Tokens:     &seqTokens{},
		RefreshTTL: refreshTTL,
	})
	return &fixture{svc: svc, store: store, clock: clock, limiter: limiter, jwt: jwtm, hasher: hasher}
}

func (f *fixture) register(t *testing.T, email, password string) auth.Session {
	t.Helper()
	s, err := f.svc.Register(context.Background(), auth.RegisterInput{Email: email, Password: password, DisplayName: "Alice"})
	require.NoError(t, err)
	return s
}

func login(f *fixture, email, password string) (auth.Session, error) {
	return f.svc.Login(context.Background(), auth.LoginInput{Email: email, Password: password, ClientIP: "10.0.0.1"})
}

func TestRegisterSuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s, err := f.svc.Register(context.Background(), auth.RegisterInput{
		Email:       "  Alice@Example.COM ",
		Password:    "s3cret-pass",
		DisplayName: "  Alice  ",
		AvatarColor: "#abcdef",
	})
	require.NoError(t, err)

	assert.NotEqual(t, uuid.Nil, s.User.ID)
	assert.Equal(t, "alice@example.com", s.User.Email)
	assert.Equal(t, "Alice", s.User.DisplayName)
	assert.Equal(t, "#ABCDEF", s.User.AvatarColor)
	assert.NotEqual(t, "s3cret-pass", s.User.PasswordHash)
	assert.Contains(t, s.User.PasswordHash, "$argon2id$")

	uid, err := f.jwt.Verify(s.Tokens.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, s.User.ID, uid)
	assert.Equal(t, testEpoch.Add(jwtTTL), s.Tokens.AccessExpiresAt)
	assert.Equal(t, "refresh-token-1", s.Tokens.RefreshToken)
	assert.Equal(t, testEpoch.Add(refreshTTL), s.Tokens.RefreshExpiresAt)

	stored, ok := f.store.token(s.Tokens.RefreshToken)
	require.True(t, ok)
	assert.Equal(t, s.User.ID, stored.UserID)
	assert.NotEqual(t, uuid.Nil, stored.FamilyID)
	assert.Equal(t, auth.HashToken(s.Tokens.RefreshToken), stored.TokenHash)
	assert.NotEqual(t, s.Tokens.RefreshToken, stored.TokenHash)
}

func TestRegisterDefaultAvatarColor(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.register(t, "bob@example.com", "password1")
	assert.Equal(t, domain.DefaultAvatarColor("bob@example.com"), s.User.AvatarColor)
}

func TestRegisterValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		in    auth.RegisterInput
		field string
	}{
		{"bad email", auth.RegisterInput{Email: "nope", Password: "password1", DisplayName: "A"}, "email"},
		{"empty email", auth.RegisterInput{Email: "", Password: "password1", DisplayName: "A"}, "email"},
		{"short password", auth.RegisterInput{Email: "a@b.co", Password: "short", DisplayName: "A"}, "password"},
		{"empty display name", auth.RegisterInput{Email: "a@b.co", Password: "password1", DisplayName: "  "}, "display_name"},
		{"bad color", auth.RegisterInput{Email: "a@b.co", Password: "password1", DisplayName: "A", AvatarColor: "red"}, "avatar_color"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			_, err := f.svc.Register(context.Background(), tt.in)
			var vErr *domain.ValidationError
			require.True(t, errors.As(err, &vErr), "got %v", err)
			assert.Equal(t, tt.field, vErr.Field)
			assert.Zero(t, f.store.createUserCalls)
		})
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "dup@example.com", "password1")
	_, err := f.svc.Register(context.Background(), auth.RegisterInput{Email: "DUP@example.com", Password: "password2", DisplayName: "B"})
	assert.ErrorIs(t, err, domain.ErrEmailTaken)
}

func TestRegisterStoreErrorsPropagate(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	f := newFixture(t)
	f.store.createUserErr = boom
	_, err := f.svc.Register(context.Background(), auth.RegisterInput{Email: "a@b.co", Password: "password1", DisplayName: "A"})
	assert.ErrorIs(t, err, boom)

	f = newFixture(t)
	f.store.createRefreshEr = boom
	_, err = f.svc.Register(context.Background(), auth.RegisterInput{Email: "a@b.co", Password: "password1", DisplayName: "A"})
	assert.ErrorIs(t, err, boom)
}

func TestLoginSuccess(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")

	s, err := login(f, " Alice@Example.com", "password1")
	require.NoError(t, err)
	assert.Equal(t, reg.User.ID, s.User.ID)

	uid, err := f.jwt.Verify(s.Tokens.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, reg.User.ID, uid)
	assert.NotEqual(t, reg.Tokens.RefreshToken, s.Tokens.RefreshToken)

	first, ok := f.store.token(reg.Tokens.RefreshToken)
	require.True(t, ok)
	second, ok := f.store.token(s.Tokens.RefreshToken)
	require.True(t, ok)
	assert.NotEqual(t, first.FamilyID, second.FamilyID)
	assert.Nil(t, first.RevokedAt)
}

func TestLoginFailuresReturnSameError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		email    string
		password string
	}{
		{"wrong password", "alice@example.com", "wrong-password"},
		{"unknown user", "nobody@example.com", "password1"},
		{"invalid email", "not-an-email", "password1"},
		{"empty email", "", "password1"},
		{"empty password", "alice@example.com", ""},
		{"oversized password", "alice@example.com", string(make([]byte, 4*domain.MaxPasswordLength+1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.register(t, "alice@example.com", "password1")
			s, err := login(f, tt.email, tt.password)
			assert.ErrorIs(t, err, domain.ErrInvalidCredential)
			assert.Equal(t, auth.Session{}, s)
		})
	}
}

func TestLoginUnknownUserStillBurnsHash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	_, err := login(f, "ghost@example.com", "password1")
	assert.ErrorIs(t, err, domain.ErrInvalidCredential)
	_, err = login(f, "ghost@example.com", "password1")
	assert.ErrorIs(t, err, domain.ErrInvalidCredential)
}

func TestLoginRateLimitSixthAttempt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")

	for i := 1; i <= 5; i++ {
		_, err := login(f, "alice@example.com", "wrong-password")
		require.ErrorIs(t, err, domain.ErrInvalidCredential, "attempt %d", i)
	}
	_, err := login(f, "alice@example.com", "wrong-password")
	assert.ErrorIs(t, err, domain.ErrRateLimited)

	_, err = login(f, "alice@example.com", "password1")
	assert.ErrorIs(t, err, domain.ErrRateLimited, "correct password must not bypass the limiter")

	calls := f.store.getUserCalls
	_, err = login(f, "alice@example.com", "password1")
	assert.ErrorIs(t, err, domain.ErrRateLimited)
	assert.Equal(t, calls, f.store.getUserCalls, "blocked attempts must not reach the store")
}

func TestLoginRateLimitIsPerIPAndEmail(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")
	f.register(t, "bob@example.com", "password1")

	for range 5 {
		_, err := login(f, "alice@example.com", "bad-password")
		require.ErrorIs(t, err, domain.ErrInvalidCredential)
	}
	_, err := login(f, "alice@example.com", "password1")
	assert.ErrorIs(t, err, domain.ErrRateLimited)

	_, err = login(f, "bob@example.com", "password1")
	assert.NoError(t, err)

	_, err = f.svc.Login(context.Background(), auth.LoginInput{Email: "alice@example.com", Password: "password1", ClientIP: "10.0.0.2"})
	assert.NoError(t, err)
}

func TestLoginRateLimitNormalizesEmailCase(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")
	emails := []string{"alice@example.com", "ALICE@example.com", " Alice@Example.com", "alice@EXAMPLE.com", "aLiCe@example.com"}
	for _, e := range emails {
		_, err := login(f, e, "bad-password")
		require.ErrorIs(t, err, domain.ErrInvalidCredential)
	}
	_, err := login(f, "alice@example.com", "password1")
	assert.ErrorIs(t, err, domain.ErrRateLimited)
}

func TestLoginRateLimitWindowRecovers(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")
	for range 5 {
		_, _ = login(f, "alice@example.com", "bad-password")
	}
	_, err := login(f, "alice@example.com", "password1")
	require.ErrorIs(t, err, domain.ErrRateLimited)

	f.clock.Advance(time.Minute + time.Second)
	_, err = login(f, "alice@example.com", "password1")
	assert.NoError(t, err)
}

func TestLoginSuccessResetsFailureCounter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")

	for range 4 {
		_, err := login(f, "alice@example.com", "bad-password")
		require.ErrorIs(t, err, domain.ErrInvalidCredential)
	}
	_, err := login(f, "alice@example.com", "password1")
	require.NoError(t, err)

	for i := 1; i <= 5; i++ {
		_, err := login(f, "alice@example.com", "bad-password")
		require.ErrorIs(t, err, domain.ErrInvalidCredential, "attempt %d after reset", i)
	}
	_, err = login(f, "alice@example.com", "bad-password")
	assert.ErrorIs(t, err, domain.ErrRateLimited)
}

func TestLoginInfrastructureErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")

	t.Run("limiter reserve", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		svc := auth.NewService(auth.Deps{Store: f.store, Hasher: auth.NewArgon2Hasher(fastArgon2), Access: f.jwt, Limiter: errLimiter{reserveErr: boom}, Clock: f.clock, RefreshTTL: refreshTTL})
		_, err := svc.Login(context.Background(), auth.LoginInput{Email: "a@b.co", Password: "password1"})
		assert.ErrorIs(t, err, boom)
	})

	t.Run("limiter reset", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.register(t, "a@b.co", "password1")
		svc := auth.NewService(auth.Deps{Store: f.store, Hasher: auth.NewArgon2Hasher(fastArgon2), Access: f.jwt, Limiter: errLimiter{resetErr: boom}, Clock: f.clock, RefreshTTL: refreshTTL})
		_, err := svc.Login(context.Background(), auth.LoginInput{Email: "a@b.co", Password: "password1"})
		assert.ErrorIs(t, err, boom)
	})

	t.Run("store lookup", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.store.getUserErr = boom
		_, err := login(f, "a@b.co", "password1")
		assert.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, domain.ErrInvalidCredential)
	})

	t.Run("corrupt stored hash", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.store.users["a@b.co"] = domain.User{ID: uuid.New(), Email: "a@b.co", PasswordHash: "garbage"}
		_, err := login(f, "a@b.co", "password1")
		assert.ErrorIs(t, err, auth.ErrInvalidHash)
	})

	t.Run("refresh token creation", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t)
		f.register(t, "a@b.co", "password1")
		f.store.createRefreshEr = boom
		_, err := login(f, "a@b.co", "password1")
		assert.ErrorIs(t, err, boom)
	})
}

func TestRefreshRotation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")
	f.clock.Advance(time.Minute)

	pair, err := f.svc.Refresh(context.Background(), reg.Tokens.RefreshToken)
	require.NoError(t, err)

	assert.NotEqual(t, reg.Tokens.RefreshToken, pair.RefreshToken)
	assert.NotEqual(t, reg.Tokens.AccessToken, pair.AccessToken)
	assert.Equal(t, f.clock.Now().Add(refreshTTL), pair.RefreshExpiresAt)
	assert.Equal(t, f.clock.Now().Add(jwtTTL), pair.AccessExpiresAt)

	uid, err := f.jwt.Verify(pair.AccessToken)
	require.NoError(t, err)
	assert.Equal(t, reg.User.ID, uid)

	old, _ := f.store.token(reg.Tokens.RefreshToken)
	cur, ok := f.store.token(pair.RefreshToken)
	require.True(t, ok)
	require.NotNil(t, old.RevokedAt)
	assert.Nil(t, cur.RevokedAt)
	assert.Equal(t, old.FamilyID, cur.FamilyID)
	assert.Equal(t, old.UserID, cur.UserID)
}

func TestRefreshChainKeepsWorking(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")
	token := reg.Tokens.RefreshToken
	for i := range 5 {
		pair, err := f.svc.Refresh(context.Background(), token)
		require.NoError(t, err, "rotation %d", i)
		token = pair.RefreshToken
	}
}

func TestRefreshReuseRevokesFamily(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")
	other, err := login(f, "alice@example.com", "password1")
	require.NoError(t, err)

	rotated, err := f.svc.Refresh(context.Background(), reg.Tokens.RefreshToken)
	require.NoError(t, err)

	_, err = f.svc.Refresh(context.Background(), reg.Tokens.RefreshToken)
	assert.ErrorIs(t, err, domain.ErrTokenReused)

	_, err = f.svc.Refresh(context.Background(), rotated.RefreshToken)
	assert.ErrorIs(t, err, domain.ErrTokenReused, "newest token of the family must be revoked too")

	_, err = f.svc.Refresh(context.Background(), other.Tokens.RefreshToken)
	assert.NoError(t, err, "a different family must stay valid")
}

func TestRefreshRejections(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")

	_, err := f.svc.Refresh(context.Background(), "")
	assert.ErrorIs(t, err, domain.ErrTokenInvalid)

	_, err = f.svc.Refresh(context.Background(), "never-issued")
	assert.ErrorIs(t, err, domain.ErrTokenInvalid)

	f.clock.Advance(refreshTTL + time.Second)
	_, err = f.svc.Refresh(context.Background(), reg.Tokens.RefreshToken)
	assert.ErrorIs(t, err, domain.ErrTokenExpired)
}

func TestRefreshStoreErrorPropagates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")
	boom := errors.New("boom")
	f.store.rotateErr = boom
	_, err := f.svc.Refresh(context.Background(), reg.Tokens.RefreshToken)
	assert.ErrorIs(t, err, boom)
}

func TestLogout(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")

	require.NoError(t, f.svc.Logout(context.Background(), reg.User.ID, reg.Tokens.RefreshToken))
	stored, _ := f.store.token(reg.Tokens.RefreshToken)
	assert.NotNil(t, stored.RevokedAt)

	_, err := f.svc.Refresh(context.Background(), reg.Tokens.RefreshToken)
	assert.ErrorIs(t, err, domain.ErrTokenReused)

	require.NoError(t, f.svc.Logout(context.Background(), reg.User.ID, reg.Tokens.RefreshToken), "second logout is idempotent")
	require.NoError(t, f.svc.Logout(context.Background(), reg.User.ID, ""), "empty token is a no-op")
	require.NoError(t, f.svc.Logout(context.Background(), reg.User.ID, "unknown"))
}

func TestLogoutOnlyAffectsOwnTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	alice := f.register(t, "alice@example.com", "password1")
	bob := f.register(t, "bob@example.com", "password1")

	require.NoError(t, f.svc.Logout(context.Background(), bob.User.ID, alice.Tokens.RefreshToken))
	stored, _ := f.store.token(alice.Tokens.RefreshToken)
	assert.Nil(t, stored.RevokedAt)

	_, err := f.svc.Refresh(context.Background(), alice.Tokens.RefreshToken)
	assert.NoError(t, err)
}

func TestLogoutStoreError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")
	boom := errors.New("boom")
	f.store.revokeErr = boom
	assert.ErrorIs(t, f.svc.Logout(context.Background(), reg.User.ID, reg.Tokens.RefreshToken), boom)
}

func TestNewServiceDefaults(t *testing.T) {
	t.Parallel()
	store := newFakeStore()
	svc := auth.NewService(auth.Deps{
		Store:      store,
		Hasher:     auth.NewArgon2Hasher(fastArgon2),
		Access:     newJWT(nil),
		Limiter:    ratelimit.NewMemoryLimiter(5, time.Minute, nil),
		RefreshTTL: time.Hour,
	})
	s, err := svc.Register(context.Background(), auth.RegisterInput{Email: "a@b.co", Password: "password1", DisplayName: "A"})
	require.NoError(t, err)
	assert.NotEmpty(t, s.Tokens.RefreshToken)
	assert.WithinDuration(t, time.Now().Add(time.Hour), s.Tokens.RefreshExpiresAt, 5*time.Second)
}

func TestLoginConcurrentAttemptsCannotBypassLimiter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")
	hasher := newSlowHasher(f.hasher, 20*time.Millisecond)
	svc := auth.NewService(auth.Deps{
		Store:      f.store,
		Hasher:     hasher,
		Access:     f.jwt,
		Limiter:    ratelimit.NewMemoryLimiter(5, time.Minute, f.clock),
		Clock:      f.clock,
		RefreshTTL: refreshTTL,
	})

	results := concurrentLogins(svc, 50, auth.LoginInput{Email: "alice@example.com", Password: "wrong-password", ClientIP: "10.0.0.9"})
	assert.LessOrEqual(t, hasher.verifies.Load(), int32(5))
	assert.Equal(t, 5, results[domain.ErrInvalidCredential])
	assert.Equal(t, 45, results[domain.ErrRateLimited])
}

func TestLoginRateLimitCarriesRetryAfter(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")
	for range 5 {
		_, _ = login(f, "alice@example.com", "bad-password")
	}
	f.clock.Advance(15 * time.Second)
	_, err := login(f, "alice@example.com", "bad-password")
	var rl *domain.RateLimitError
	require.ErrorAs(t, err, &rl)
	assert.Equal(t, 45*time.Second, rl.RetryAfter)
}

func TestLoginHasherOverloadPropagates(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.register(t, "alice@example.com", "password1")
	svc := auth.NewService(auth.Deps{
		Store:      f.store,
		Hasher:     overloadedHasher{},
		Access:     f.jwt,
		Limiter:    ratelimit.NewMemoryLimiter(5, time.Minute, f.clock),
		Clock:      f.clock,
		RefreshTTL: refreshTTL,
	})
	_, err := svc.Login(context.Background(), auth.LoginInput{Email: "alice@example.com", Password: "password1"})
	assert.ErrorIs(t, err, domain.ErrOverloaded)
	_, err = svc.Login(context.Background(), auth.LoginInput{Email: "ghost@example.com", Password: "password1"})
	assert.ErrorIs(t, err, domain.ErrOverloaded)
	_, err = svc.Register(context.Background(), auth.RegisterInput{Email: "new@example.com", Password: "password1", DisplayName: "N"})
	assert.ErrorIs(t, err, domain.ErrOverloaded)
}

type overloadedHasher struct{}

func (overloadedHasher) Hash(context.Context, string) (string, error) {
	return "", domain.ErrOverloaded
}

func (overloadedHasher) Verify(context.Context, string, string) (bool, error) {
	return false, domain.ErrOverloaded
}

func TestLogoutWithRotatedTokenRevokesCurrentSession(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	reg := f.register(t, "alice@example.com", "password1")
	other, err := login(f, "alice@example.com", "password1")
	require.NoError(t, err)
	rotated, err := f.svc.Refresh(context.Background(), reg.Tokens.RefreshToken)
	require.NoError(t, err)

	require.NoError(t, f.svc.Logout(context.Background(), reg.User.ID, reg.Tokens.RefreshToken))

	current, _ := f.store.token(rotated.RefreshToken)
	assert.NotNil(t, current.RevokedAt, "the family's current token must be revoked")
	untouched, _ := f.store.token(other.Tokens.RefreshToken)
	assert.Nil(t, untouched.RevokedAt, "other sessions stay active")
}
