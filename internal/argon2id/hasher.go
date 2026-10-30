package argon2id

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

var _ auth.PasswordHasher = (*Hasher)(nil)

type Params struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

var DefaultParams = Params{
	Memory:      19 * 1024,
	Iterations:  2,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}

var ErrInvalidHash = errors.New("invalid password hash format")

const DefaultAcquireTimeout = 2 * time.Second

type Hasher struct {
	params         Params
	slots          chan struct{}
	acquireTimeout time.Duration
}

type Option func(*Hasher)

func WithMaxConcurrent(n int) Option {
	return func(h *Hasher) {
		if n > 0 {
			h.slots = make(chan struct{}, n)
		}
	}
}

func WithAcquireTimeout(d time.Duration) Option {
	return func(h *Hasher) {
		if d > 0 {
			h.acquireTimeout = d
		}
	}
}

func New(params Params, opts ...Option) *Hasher {
	h := &Hasher{
		params:         params,
		slots:          make(chan struct{}, runtime.GOMAXPROCS(0)),
		acquireTimeout: DefaultAcquireTimeout,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

func (h *Hasher) acquire(ctx context.Context) (func(), error) {
	release := func() { <-h.slots }
	select {
	case h.slots <- struct{}{}:
		return release, nil
	default:
	}
	timer := time.NewTimer(h.acquireTimeout)
	defer timer.Stop()
	select {
	case h.slots <- struct{}{}:
		return release, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, domain.ErrOverloaded
	}
}

func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	release, err := h.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	salt := make([]byte, h.params.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Iterations, h.params.Memory, h.params.Parallelism, h.params.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		h.params.Memory, h.params.Iterations, h.params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

func (h *Hasher) Verify(ctx context.Context, password, encoded string) (bool, error) {
	params, salt, key, err := decode(encoded)
	if err != nil {
		return false, err
	}
	keyLen := len(key)
	if keyLen == 0 || keyLen > 1024 {
		return false, ErrInvalidHash
	}
	release, err := h.acquire(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	other := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, uint32(keyLen))
	return subtle.ConstantTimeCompare(key, other) == 1, nil
}

func decode(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, ErrInvalidHash
	}
	var p Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	if p.Memory == 0 || p.Iterations == 0 || p.Parallelism == 0 {
		return Params{}, nil, nil, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, ErrInvalidHash
	}
	return p, salt, key, nil
}
