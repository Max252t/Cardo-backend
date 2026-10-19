package storage_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func newToken(t *testing.T, userID, family uuid.UUID, raw string, expires time.Time) domain.RefreshToken {
	t.Helper()
	id, err := uuid.NewV7()
	require.NoError(t, err)
	return domain.RefreshToken{ID: id, UserID: userID, FamilyID: family, TokenHash: auth.HashToken(raw), ExpiresAt: expires}
}

func familyState(t *testing.T, pool *pgxpool.Pool, family uuid.UUID) (total, revoked int) {
	t.Helper()
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*), count(revoked_at) FROM refresh_tokens WHERE family_id = $1`, family).Scan(&total, &revoked))
	return total, revoked
}

func TestRefreshTokenCreateStoresOnlyHash(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	u := testutil.CreateUser(t, store, "rt@example.com")
	now := time.Now()

	family := uuid.New()
	tok := newToken(t, u.ID, family, "raw-secret-token", now.Add(time.Hour))
	created, err := store.CreateRefreshToken(context.Background(), tok)
	require.NoError(t, err)
	assert.Equal(t, tok.ID, created.ID)
	assert.Equal(t, u.ID, created.UserID)
	assert.Equal(t, family, created.FamilyID)
	assert.Nil(t, created.RevokedAt)

	var count int
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_tokens WHERE token_hash = 'raw-secret-token'`).Scan(&count))
	assert.Zero(t, count)
}

func TestRefreshTokenRotation(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	u := testutil.CreateUser(t, store, "rotate@example.com")
	now := time.Now()
	family := uuid.New()

	_, err := store.CreateRefreshToken(ctx, newToken(t, u.ID, family, "t1", now.Add(time.Hour)))
	require.NoError(t, err)

	next := newToken(t, uuid.Nil, uuid.Nil, "t2", now.Add(2*time.Hour))
	issued, err := store.RotateRefreshToken(ctx, auth.HashToken("t1"), next, now)
	require.NoError(t, err)
	assert.Equal(t, next.ID, issued.ID)
	assert.Equal(t, u.ID, issued.UserID, "rotation must inherit the user from the presented token")
	assert.Equal(t, family, issued.FamilyID, "rotation must inherit the family from the presented token")
	assert.Nil(t, issued.RevokedAt)

	total, revoked := familyState(t, pool, family)
	assert.Equal(t, 2, total)
	assert.Equal(t, 1, revoked)

	var revokedAt *time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT revoked_at FROM refresh_tokens WHERE token_hash = $1`, auth.HashToken("t1")).Scan(&revokedAt))
	require.NotNil(t, revokedAt)
	assert.WithinDuration(t, now, *revokedAt, time.Second)

	third, err := store.RotateRefreshToken(ctx, auth.HashToken("t2"), newToken(t, uuid.Nil, uuid.Nil, "t3", now.Add(3*time.Hour)), now)
	require.NoError(t, err)
	assert.Equal(t, family, third.FamilyID)
}

func TestRefreshTokenReuseRevokesWholeFamily(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	u := testutil.CreateUser(t, store, "reuse@example.com")
	now := time.Now()
	family, otherFamily := uuid.New(), uuid.New()

	_, err := store.CreateRefreshToken(ctx, newToken(t, u.ID, family, "a1", now.Add(time.Hour)))
	require.NoError(t, err)
	_, err = store.CreateRefreshToken(ctx, newToken(t, u.ID, otherFamily, "other1", now.Add(time.Hour)))
	require.NoError(t, err)

	_, err = store.RotateRefreshToken(ctx, auth.HashToken("a1"), newToken(t, uuid.Nil, uuid.Nil, "a2", now.Add(time.Hour)), now)
	require.NoError(t, err)
	_, err = store.RotateRefreshToken(ctx, auth.HashToken("a2"), newToken(t, uuid.Nil, uuid.Nil, "a3", now.Add(time.Hour)), now)
	require.NoError(t, err)

	total, revoked := familyState(t, pool, family)
	require.Equal(t, 3, total)
	require.Equal(t, 2, revoked, "only the newest token is live before reuse")

	replayAt := now.Add(time.Minute)
	_, err = store.RotateRefreshToken(ctx, auth.HashToken("a1"), newToken(t, uuid.Nil, uuid.Nil, "a-evil", now.Add(time.Hour)), replayAt)
	assert.ErrorIs(t, err, domain.ErrTokenReused)

	total, revoked = familyState(t, pool, family)
	assert.Equal(t, 3, total, "a replayed token must not mint a new one")
	assert.Equal(t, 3, revoked, "the whole family must be revoked after reuse")

	var evil int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, auth.HashToken("a-evil")).Scan(&evil))
	assert.Zero(t, evil)
}

func TestRefreshTokenReuseKeepsOtherFamiliesAlive(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	u := testutil.CreateUser(t, store, "families@example.com")
	now := time.Now()
	family, other := uuid.New(), uuid.New()

	_, err := store.CreateRefreshToken(ctx, newToken(t, u.ID, family, "f1", now.Add(time.Hour)))
	require.NoError(t, err)
	_, err = store.CreateRefreshToken(ctx, newToken(t, u.ID, other, "o1", now.Add(time.Hour)))
	require.NoError(t, err)

	_, err = store.RotateRefreshToken(ctx, auth.HashToken("f1"), newToken(t, uuid.Nil, uuid.Nil, "f2", now.Add(time.Hour)), now)
	require.NoError(t, err)
	_, err = store.RotateRefreshToken(ctx, auth.HashToken("f1"), newToken(t, uuid.Nil, uuid.Nil, "x", now.Add(time.Hour)), now)
	require.ErrorIs(t, err, domain.ErrTokenReused)

	total, revoked := familyState(t, pool, other)
	assert.Equal(t, 1, total)
	assert.Zero(t, revoked)

	_, err = store.RotateRefreshToken(ctx, auth.HashToken("o1"), newToken(t, uuid.Nil, uuid.Nil, "o2", now.Add(time.Hour)), now)
	assert.NoError(t, err)
}

func TestRefreshTokenRejections(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	u := testutil.CreateUser(t, store, "reject@example.com")
	now := time.Now()

	_, err := store.RotateRefreshToken(ctx, auth.HashToken("never-issued"), newToken(t, uuid.Nil, uuid.Nil, "n", now.Add(time.Hour)), now)
	assert.ErrorIs(t, err, domain.ErrTokenInvalid)

	family := uuid.New()
	_, err = store.CreateRefreshToken(ctx, newToken(t, u.ID, family, "exp1", now.Add(time.Hour)))
	require.NoError(t, err)

	_, err = store.RotateRefreshToken(ctx, auth.HashToken("exp1"), newToken(t, uuid.Nil, uuid.Nil, "exp2", now.Add(time.Hour)), now.Add(time.Hour))
	assert.ErrorIs(t, err, domain.ErrTokenExpired, "a token is expired exactly at expires_at")

	_, err = store.RotateRefreshToken(ctx, auth.HashToken("exp1"), newToken(t, uuid.Nil, uuid.Nil, "exp3", now.Add(2*time.Hour)), now.Add(2*time.Hour))
	assert.ErrorIs(t, err, domain.ErrTokenExpired)

	total, revoked := familyState(t, pool, family)
	assert.Equal(t, 1, total, "expired rotation must not insert a new token")
	assert.Zero(t, revoked, "expired rotation must not revoke anything")

	_, err = store.RotateRefreshToken(ctx, auth.HashToken("exp1"), newToken(t, uuid.Nil, uuid.Nil, "ok", now.Add(2*time.Hour)), now.Add(30*time.Minute))
	assert.NoError(t, err, "the token is still usable before expiry")
}

func TestRefreshTokenConcurrentRotationOfSameToken(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	u := testutil.CreateUser(t, store, "concurrent-rt@example.com")
	now := time.Now()
	family := uuid.New()

	_, err := store.CreateRefreshToken(ctx, newToken(t, u.ID, family, "shared", now.Add(time.Hour)))
	require.NoError(t, err)

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	start := make(chan struct{})
	nexts := make([]domain.RefreshToken, workers)
	for i := range nexts {
		nexts[i] = newToken(t, uuid.Nil, uuid.Nil, fmt.Sprintf("next-%d", i), now.Add(time.Hour))
	}
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = store.RotateRefreshToken(ctx, auth.HashToken("shared"), nexts[i], now)
		}()
	}
	close(start)
	wg.Wait()

	var success, reused int
	for _, err := range errs {
		switch {
		case err == nil:
			success++
		case assert.ErrorIs(t, err, domain.ErrTokenReused):
			reused++
		}
	}
	assert.Equal(t, 1, success, "exactly one concurrent rotation may win")
	assert.Equal(t, workers-1, reused)

	total, revoked := familyState(t, pool, family)
	assert.Equal(t, 2, total)
	assert.Equal(t, 2, revoked, "losing the race is treated as reuse and revokes the family")
}

func TestRefreshTokenRevoke(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	alice := testutil.CreateUser(t, store, "alice-revoke@example.com")
	bob := testutil.CreateUser(t, store, "bob-revoke@example.com")
	now := time.Now()
	family := uuid.New()

	_, err := store.CreateRefreshToken(ctx, newToken(t, alice.ID, family, "alice-token", now.Add(time.Hour)))
	require.NoError(t, err)

	revoked, err := store.RevokeRefreshToken(ctx, bob.ID, auth.HashToken("alice-token"), now)
	require.NoError(t, err)
	assert.False(t, revoked, "another user cannot revoke the token")

	revoked, err = store.RevokeRefreshToken(ctx, alice.ID, auth.HashToken("missing"), now)
	require.NoError(t, err)
	assert.False(t, revoked)

	revoked, err = store.RevokeRefreshToken(ctx, alice.ID, auth.HashToken("alice-token"), now)
	require.NoError(t, err)
	assert.True(t, revoked)

	revoked, err = store.RevokeRefreshToken(ctx, alice.ID, auth.HashToken("alice-token"), now)
	require.NoError(t, err)
	assert.False(t, revoked, "revoking twice is a no-op")

	_, err = store.RotateRefreshToken(ctx, auth.HashToken("alice-token"), newToken(t, uuid.Nil, uuid.Nil, "after", now.Add(time.Hour)), now)
	assert.ErrorIs(t, err, domain.ErrTokenReused)
}

func TestRefreshTokensCascadeOnUserDeletion(t *testing.T) {
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	ctx := context.Background()
	u := testutil.CreateUser(t, store, "cascade@example.com")
	family := uuid.New()
	_, err := store.CreateRefreshToken(ctx, newToken(t, u.ID, family, "c1", time.Now().Add(time.Hour)))
	require.NoError(t, err)

	_, err = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u.ID)
	require.NoError(t, err)
	total, _ := familyState(t, pool, family)
	assert.Zero(t, total)
}
