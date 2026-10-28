package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Max252t/Cardo-backend/internal/clock"
)

const (
	TableBoardEvents  = "board_events"
	TableProcessedOps = "processed_ops"
)

type Result struct {
	Deleted int64
	Skipped bool
}

type Store interface {
	PruneEvents(ctx context.Context, olderThan time.Time, keepMin, batch int) (Result, error)
	PruneProcessedOps(ctx context.Context, olderThan time.Time, batch int) (Result, error)
}

type Metrics interface {
	Deleted(table string, n int64)
}

type nopMetrics struct{}

func (nopMetrics) Deleted(string, int64) {}

type Config struct {
	Interval              time.Duration
	EventsRetention       time.Duration
	EventsKeepMin         int
	ProcessedOpsRetention time.Duration
	BatchSize             int
}

func (c Config) Validate() error {
	var errs []error
	if c.Interval <= 0 {
		errs = append(errs, errors.New("interval must be positive"))
	}
	if c.EventsRetention <= 0 {
		errs = append(errs, errors.New("events retention must be positive"))
	}
	if c.ProcessedOpsRetention <= 0 {
		errs = append(errs, errors.New("processed ops retention must be positive"))
	}
	if c.EventsRetention < c.ProcessedOpsRetention {
		errs = append(errs, errors.New("events retention must not be shorter than processed ops retention"))
	}
	if c.EventsKeepMin < 0 {
		errs = append(errs, errors.New("events keep minimum must not be negative"))
	}
	if c.BatchSize <= 0 {
		errs = append(errs, errors.New("batch size must be positive"))
	}
	return errors.Join(errs...)
}

type Service struct {
	store   Store
	clock   clock.Clock
	logger  *slog.Logger
	metrics Metrics
	cfg     Config
}

type Option func(*Service)

func WithClock(c clock.Clock) Option {
	return func(s *Service) {
		if c != nil {
			s.clock = c
		}
	}
}

func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

func WithMetrics(m Metrics) Option {
	return func(s *Service) {
		if m != nil {
			s.metrics = m
		}
	}
}

func New(store Store, cfg Config, opts ...Option) (*Service, error) {
	if store == nil {
		return nil, errors.New("retention: store is required")
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("retention: %w", err)
	}
	s := &Service{
		store:   store,
		clock:   clock.System{},
		logger:  slog.New(slog.DiscardHandler),
		metrics: nopMetrics{},
		cfg:     cfg,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

func (s *Service) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.RunOnce(ctx)
		timer.Reset(s.cfg.Interval)
	}
}

func (s *Service) RunOnce(ctx context.Context) {
	now := s.clock.Now()
	s.job(ctx, TableBoardEvents, func() (Result, error) {
		return s.store.PruneEvents(ctx, now.Add(-s.cfg.EventsRetention), s.cfg.EventsKeepMin, s.cfg.BatchSize)
	})
	s.job(ctx, TableProcessedOps, func() (Result, error) {
		return s.store.PruneProcessedOps(ctx, now.Add(-s.cfg.ProcessedOpsRetention), s.cfg.BatchSize)
	})
}

func (s *Service) job(ctx context.Context, table string, run func() (Result, error)) {
	res, err := run()
	if res.Deleted > 0 {
		s.metrics.Deleted(table, res.Deleted)
	}
	switch {
	case err != nil && ctx.Err() != nil:
		s.logger.Info("retention job interrupted", slog.String("table", table), slog.Int64("deleted", res.Deleted))
	case err != nil:
		s.logger.Error("retention job failed", slog.String("table", table), slog.Int64("deleted", res.Deleted), slog.Any("error", err))
	case res.Skipped:
		s.logger.Debug("retention job skipped, another instance holds the lock", slog.String("table", table))
	default:
		s.logger.Info("retention job finished", slog.String("table", table), slog.Int64("deleted", res.Deleted))
	}
}
