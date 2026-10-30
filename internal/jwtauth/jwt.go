package jwtauth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

var _ auth.AccessTokens = (*Manager)(nil)

const ClockSkewLeeway = 30 * time.Second

type Config struct {
	Secret   []byte
	Issuer   string
	Audience string
	TTL      time.Duration
}

type Manager struct {
	cfg   Config
	clock auth.Clock
}

func New(cfg Config, clock auth.Clock) *Manager {
	if clock == nil {
		clock = auth.SystemClock{}
	}
	return &Manager{cfg: cfg, clock: clock}
}

func (m *Manager) Issue(userID uuid.UUID) (string, time.Time, error) {
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

func (m *Manager) Verify(token string) (uuid.UUID, error) {
	c, err := m.Parse(token)
	return c.UserID, err
}

func (m *Manager) Parse(token string) (domain.AccessClaims, error) {
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
			return domain.AccessClaims{}, domain.ErrTokenExpired
		}
		return domain.AccessClaims{}, domain.ErrTokenInvalid
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil || id == uuid.Nil || claims.ExpiresAt == nil {
		return domain.AccessClaims{}, domain.ErrTokenInvalid
	}
	return domain.AccessClaims{UserID: id, ExpiresAt: claims.ExpiresAt.Time}, nil
}
