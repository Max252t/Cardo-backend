package auth_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

var testEpoch = time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: testEpoch}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

var fastArgon2 = auth.Argon2Params{
	Memory:      8,
	Iterations:  1,
	Parallelism: 1,
	SaltLength:  8,
	KeyLength:   16,
}

type seqTokens struct {
	mu sync.Mutex
	n  int
}

func (s *seqTokens) NewToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return fmt.Sprintf("refresh-token-%d", s.n), nil
}

type fakeStore struct {
	mu     sync.Mutex
	users  map[string]domain.User
	tokens map[string]domain.RefreshToken

	createUserErr   error
	getUserErr      error
	createRefreshEr error
	rotateErr       error
	revokeErr       error

	createUserCalls int
	getUserCalls    int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		users:  map[string]domain.User{},
		tokens: map[string]domain.RefreshToken{},
	}
}

func (s *fakeStore) CreateUser(_ context.Context, u domain.User) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createUserCalls++
	if s.createUserErr != nil {
		return domain.User{}, s.createUserErr
	}
	if _, ok := s.users[u.Email]; ok {
		return domain.User{}, domain.ErrEmailTaken
	}
	u.CreatedAt = testEpoch
	s.users[u.Email] = u
	return u, nil
}

func (s *fakeStore) GetUserByEmail(_ context.Context, email string) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getUserCalls++
	if s.getUserErr != nil {
		return domain.User{}, s.getUserErr
	}
	u, ok := s.users[email]
	if !ok {
		return domain.User{}, domain.ErrNotFound
	}
	return u, nil
}

func (s *fakeStore) CreateRefreshToken(_ context.Context, t domain.RefreshToken) (domain.RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createRefreshEr != nil {
		return domain.RefreshToken{}, s.createRefreshEr
	}
	t.CreatedAt = testEpoch
	s.tokens[t.TokenHash] = t
	return t, nil
}

func (s *fakeStore) RotateRefreshToken(_ context.Context, presentedHash string, next domain.RefreshToken, now time.Time) (domain.RefreshToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rotateErr != nil {
		return domain.RefreshToken{}, s.rotateErr
	}
	cur, ok := s.tokens[presentedHash]
	if !ok {
		return domain.RefreshToken{}, domain.ErrTokenInvalid
	}
	if cur.RevokedAt != nil {
		for h, t := range s.tokens {
			if t.FamilyID == cur.FamilyID && t.RevokedAt == nil {
				t.RevokedAt = &now
				s.tokens[h] = t
			}
		}
		return domain.RefreshToken{}, domain.ErrTokenReused
	}
	if !now.Before(cur.ExpiresAt) {
		return domain.RefreshToken{}, domain.ErrTokenExpired
	}
	cur.RevokedAt = &now
	s.tokens[presentedHash] = cur
	next.UserID = cur.UserID
	next.FamilyID = cur.FamilyID
	next.CreatedAt = now
	s.tokens[next.TokenHash] = next
	return next, nil
}

func (s *fakeStore) RevokeRefreshToken(_ context.Context, userID uuid.UUID, tokenHash string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revokeErr != nil {
		return false, s.revokeErr
	}
	t, ok := s.tokens[tokenHash]
	if !ok || t.UserID != userID || t.RevokedAt != nil {
		return false, nil
	}
	t.RevokedAt = &now
	s.tokens[tokenHash] = t
	return true, nil
}

func (s *fakeStore) token(raw string) (domain.RefreshToken, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[auth.HashToken(raw)]
	return t, ok
}

type errLimiter struct {
	allowErr  error
	recordErr error
	resetErr  error
}

func (l errLimiter) Allow(context.Context, string) (bool, error) {
	if l.allowErr != nil {
		return false, l.allowErr
	}
	return true, nil
}

func (l errLimiter) RecordFailure(context.Context, string) error { return l.recordErr }

func (l errLimiter) Reset(context.Context, string) error { return l.resetErr }
