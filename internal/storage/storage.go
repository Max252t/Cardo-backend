package storage

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const DefaultLockTimeout = 5 * time.Second

type Store struct {
	pool        *pgxpool.Pool
	lockTimeout time.Duration
}

type Option func(*Store)

func WithLockTimeout(d time.Duration) Option {
	return func(s *Store) {
		if d >= 0 {
			s.lockTimeout = d
		}
	}
}

type PoolOption func(*pgxpool.Config)

func WithStatementTimeout(d time.Duration) PoolOption {
	return func(cfg *pgxpool.Config) {
		if d > 0 {
			cfg.ConnConfig.RuntimeParams["statement_timeout"] = durationMillis(d)
		}
	}
}

func durationMillis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
}

func NewPool(ctx context.Context, databaseURL string, maxConns int32, opts ...PoolOption) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	for _, opt := range opts {
		opt(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

func New(pool *pgxpool.Pool, opts ...Option) *Store {
	s := &Store{pool: pool, lockTimeout: DefaultLockTimeout}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) InTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{}, fn)
}

const (
	pgUniqueViolation  = "23505"
	pgLockNotAvailable = "55P03"
)

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation {
		return false
	}
	return constraint == "" || pgErr.ConstraintName == constraint
}

func noRowsAsNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}

func lockTimeoutAsBusy(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgLockNotAvailable {
		return fmt.Errorf("%w: %w", domain.ErrBusy, err)
	}
	return err
}
