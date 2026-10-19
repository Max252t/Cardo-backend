package auth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	maxLoginPasswordBytes = 4 * domain.MaxPasswordLength
	dummyPassword         = "cardo-dummy-password"
)

type Store interface {
	CreateUser(ctx context.Context, u domain.User) (domain.User, error)
	GetUserByEmail(ctx context.Context, email string) (domain.User, error)
	CreateRefreshToken(ctx context.Context, t domain.RefreshToken) (domain.RefreshToken, error)
	RotateRefreshToken(ctx context.Context, presentedHash string, next domain.RefreshToken, now time.Time) (domain.RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, now time.Time) (bool, error)
}

type Deps struct {
	Store      Store
	Hasher     PasswordHasher
	Access     AccessTokens
	Limiter    LoginLimiter
	Clock      Clock
	Tokens     TokenGenerator
	RefreshTTL time.Duration
}

type Service struct {
	store      Store
	hasher     PasswordHasher
	access     AccessTokens
	limiter    LoginLimiter
	clock      Clock
	tokens     TokenGenerator
	refreshTTL time.Duration

	dummyMu   sync.Mutex
	dummyHash string
}

func NewService(d Deps) *Service {
	if d.Clock == nil {
		d.Clock = SystemClock{}
	}
	if d.Tokens == nil {
		d.Tokens = RandomTokenGenerator{}
	}
	return &Service{
		store:      d.Store,
		hasher:     d.Hasher,
		access:     d.Access,
		limiter:    d.Limiter,
		clock:      d.Clock,
		tokens:     d.Tokens,
		refreshTTL: d.RefreshTTL,
	}
}

type TokenPair struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

type Session struct {
	User   domain.User
	Tokens TokenPair
}

type RegisterInput struct {
	Email       string
	Password    string
	DisplayName string
	AvatarColor string
}

type LoginInput struct {
	Email    string
	Password string
	ClientIP string
}

func (s *Service) Register(ctx context.Context, in RegisterInput) (Session, error) {
	user, err := s.newUser(ctx, in)
	if err != nil {
		return Session{}, err
	}
	created, err := s.store.CreateUser(ctx, user)
	if err != nil {
		return Session{}, err
	}
	return s.startSession(ctx, created)
}

func (s *Service) newUser(ctx context.Context, in RegisterInput) (domain.User, error) {
	email, err := domain.NormalizeEmail(in.Email)
	if err != nil {
		return domain.User{}, err
	}
	if err := domain.ValidatePassword(in.Password); err != nil {
		return domain.User{}, err
	}
	name, err := domain.NormalizeDisplayName(in.DisplayName)
	if err != nil {
		return domain.User{}, err
	}
	color, err := avatarColorFor(email, in.AvatarColor)
	if err != nil {
		return domain.User{}, err
	}
	hash, err := s.hasher.Hash(ctx, in.Password)
	if err != nil {
		return domain.User{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return domain.User{}, err
	}
	return domain.User{
		ID:           id,
		Email:        email,
		PasswordHash: hash,
		DisplayName:  name,
		AvatarColor:  color,
	}, nil
}

func avatarColorFor(email, requested string) (string, error) {
	if requested == "" {
		return domain.DefaultAvatarColor(email), nil
	}
	return domain.NormalizeAvatarColor(requested)
}

func (s *Service) startSession(ctx context.Context, user domain.User) (Session, error) {
	tokens, err := s.issueNewFamily(ctx, user.ID)
	if err != nil {
		return Session{}, err
	}
	return Session{User: user, Tokens: tokens}, nil
}

func (s *Service) Login(ctx context.Context, in LoginInput) (Session, error) {
	key := LoginKey(in.ClientIP, in.Email)
	res, err := s.limiter.Reserve(ctx, key)
	if err != nil {
		return Session{}, err
	}
	if !res.Allowed {
		return Session{}, &domain.RateLimitError{RetryAfter: res.RetryAfter}
	}

	user, ok, err := s.authenticate(ctx, in.Email, in.Password)
	if err != nil {
		return Session{}, err
	}
	if !ok {
		return Session{}, domain.ErrInvalidCredential
	}
	if err := s.limiter.Reset(ctx, key); err != nil {
		return Session{}, err
	}
	return s.startSession(ctx, user)
}

func (s *Service) authenticate(ctx context.Context, rawEmail, password string) (domain.User, bool, error) {
	email, err := domain.NormalizeEmail(rawEmail)
	if err != nil || password == "" || len(password) > maxLoginPasswordBytes {
		return domain.User{}, false, s.burnHash(ctx, password)
	}
	user, err := s.store.GetUserByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		return domain.User{}, false, s.burnHash(ctx, password)
	}
	if err != nil {
		return domain.User{}, false, err
	}
	ok, err := s.hasher.Verify(ctx, password, user.PasswordHash)
	if err != nil {
		return domain.User{}, false, fmt.Errorf("verify password: %w", err)
	}
	return user, ok, nil
}

func (s *Service) burnHash(ctx context.Context, password string) error {
	dummy, err := s.dummy(ctx)
	if err != nil {
		return err
	}
	if _, err := s.hasher.Verify(ctx, password, dummy); err != nil {
		return fmt.Errorf("verify password: %w", err)
	}
	return nil
}

func (s *Service) dummy(ctx context.Context) (string, error) {
	s.dummyMu.Lock()
	defer s.dummyMu.Unlock()
	if s.dummyHash != "" {
		return s.dummyHash, nil
	}
	hash, err := s.hasher.Hash(ctx, dummyPassword)
	if err != nil {
		return "", fmt.Errorf("hash dummy password: %w", err)
	}
	s.dummyHash = hash
	return hash, nil
}

func (s *Service) Refresh(ctx context.Context, refreshToken string) (TokenPair, error) {
	if refreshToken == "" {
		return TokenPair{}, domain.ErrTokenInvalid
	}
	now := s.clock.Now()
	raw, next, err := s.newRefreshToken(uuid.Nil, uuid.Nil, now)
	if err != nil {
		return TokenPair{}, err
	}
	issued, err := s.store.RotateRefreshToken(ctx, HashToken(refreshToken), next, now)
	if err != nil {
		return TokenPair{}, err
	}
	return s.pair(issued, raw)
}

func (s *Service) Logout(ctx context.Context, userID uuid.UUID, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	_, err := s.store.RevokeRefreshToken(ctx, userID, HashToken(refreshToken), s.clock.Now())
	return err
}

func (s *Service) issueNewFamily(ctx context.Context, userID uuid.UUID) (TokenPair, error) {
	family, err := uuid.NewV7()
	if err != nil {
		return TokenPair{}, err
	}
	raw, t, err := s.newRefreshToken(userID, family, s.clock.Now())
	if err != nil {
		return TokenPair{}, err
	}
	stored, err := s.store.CreateRefreshToken(ctx, t)
	if err != nil {
		return TokenPair{}, err
	}
	return s.pair(stored, raw)
}

func (s *Service) newRefreshToken(userID, family uuid.UUID, now time.Time) (string, domain.RefreshToken, error) {
	raw, err := s.tokens.NewToken()
	if err != nil {
		return "", domain.RefreshToken{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", domain.RefreshToken{}, err
	}
	return raw, domain.RefreshToken{
		ID:        id,
		UserID:    userID,
		FamilyID:  family,
		TokenHash: HashToken(raw),
		ExpiresAt: now.Add(s.refreshTTL),
	}, nil
}

func (s *Service) pair(refresh domain.RefreshToken, rawRefresh string) (TokenPair, error) {
	access, accessExp, err := s.access.Issue(refresh.UserID)
	if err != nil {
		return TokenPair{}, err
	}
	return TokenPair{
		AccessToken:      access,
		AccessExpiresAt:  accessExp,
		RefreshToken:     rawRefresh,
		RefreshExpiresAt: refresh.ExpiresAt,
	}, nil
}
