package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type AccessTokens interface {
	Issue(userID uuid.UUID) (string, time.Time, error)
	Verify(token string) (uuid.UUID, error)
}

const ClockSkewLeeway = 30 * time.Second

type JWTConfig struct {
	Secret   []byte
	Issuer   string
	Audience string
	TTL      time.Duration
}

type JWTManager struct {
	cfg   JWTConfig
	clock Clock
}

func NewJWTManager(cfg JWTConfig, clock Clock) *JWTManager {
	if clock == nil {
		clock = SystemClock{}
	}
	return &JWTManager{cfg: cfg, clock: clock}
}

func (m *JWTManager) Issue(userID uuid.UUID) (string, time.Time, error) {
	now := m.clock.Now()
	exp := now.Add(m.cfg.TTL)
	claims := jwt.RegisteredClaims{
		Subject:   userID.String(),
		Issuer:    m.cfg.Issuer,
		Audience:  jwt.ClaimStrings{m.cfg.Audience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(exp),
		ID:        uuid.NewString(),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.cfg.Secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign access token: %w", err)
	}
	return signed, exp, nil
}

func (m *JWTManager) Verify(token string) (uuid.UUID, error) {
	claims := &jwt.RegisteredClaims{}
	_, err := jwt.ParseWithClaims(token, claims,
		func(*jwt.Token) (any, error) { return m.cfg.Secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(m.cfg.Issuer),
		jwt.WithAudience(m.cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(ClockSkewLeeway),
		jwt.WithTimeFunc(m.clock.Now),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return uuid.Nil, domain.ErrTokenExpired
		}
		return uuid.Nil, domain.ErrTokenInvalid
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, domain.ErrTokenInvalid
	}
	return id, nil
}
