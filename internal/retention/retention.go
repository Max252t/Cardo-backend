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

const (
	ResultOK          = "ok"
	ResultSkipped     = "skipped"
	ResultFailed      = "failed"
	ResultInterrupted = "interrupted"

	MaxBatchSize = 50000
)

type Metrics interface {
	Deleted(table string, n int64)
	Ran(table, result string)
}

type nopMetrics struct{}

func (nopMetrics) Deleted(string, int64) {}
func (nopMetrics) Ran(string, string)    {}

type Config struct {
	Interval              time.Duration
	EventsRetention       time.Duration
	EventsKeepMin         int
	ProcessedOpsRetention time.Duration
	BatchSize             int
}

const (
	FieldInterval              = "interval"
	FieldEventsRetention       = "events_retention"
	FieldProcessedOpsRetention = "processed_ops_retention"
	FieldEventsKeepMin         = "events_keep_min"
	FieldBatchSize             = "batch_size"
)

type Problem struct {
	Field   string
	Message string
}

func (c Config) WindowProblems() []Problem {
	if c.EventsRetention < c.ProcessedOpsRetention {
		return []Problem{{FieldEventsRetention, "must not be shorter than the processed ops retention"}}
	}
	return nil
}

func (c Config) Problems() []Problem {
	var out []Problem
	if c.Interval <= 0 {
		out = append(out, Problem{FieldInterval, "must be positive"})
	}
	if c.EventsRetention <= 0 {
		out = append(out, Problem{FieldEventsRetention, "must be positive"})
	}
	if c.ProcessedOpsRetention <= 0 {
		out = append(out, Problem{FieldProcessedOpsRetention, "must be positive"})
	}
	out = append(out, c.WindowProblems()...)
	if c.EventsKeepMin < 0 {
		out = append(out, Problem{FieldEventsKeepMin, "must not be negative"})
	}
	if c.BatchSize <= 0 || c.BatchSize > MaxBatchSize {
		out = append(out, Problem{FieldBatchSize, fmt.Sprintf("must be between 1 and %d", MaxBatchSize)})
	}
	return out
}

func (c Config) Validate() error {
	var errs []error
	for _, p := range c.Problems() {
		errs = append(errs, fmt.Errorf("%s %s", p.Field, p.Message))
	}
	return errors.Join(errs...)
}

type Service struct {
	store   Store
	clock   clock.Clock
	waiter  clock.Waiter
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

func WithWaiter(w clock.Waiter) Option {
	return func(s *Service) {
		if w != nil {
			s.waiter = w
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
		waiter:  clock.System{},
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
	for ctx.Err() == nil {
		s.RunOnce(ctx)
		if err := s.waiter.Wait(ctx, s.cfg.Interval); err != nil {
			return
		}
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
		s.metrics.Ran(table, ResultInterrupted)
		s.logger.Info("retention job interrupted", slog.String("table", table), slog.Int64("deleted", res.Deleted))
	case err != nil:
		s.metrics.Ran(table, ResultFailed)
		s.logger.Error("retention job failed", slog.String("table", table), slog.Int64("deleted", res.Deleted), slog.Any("error", err))
	case res.Skipped:
		s.metrics.Ran(table, ResultSkipped)
		s.logger.Debug("retention job skipped, another instance holds the lock", slog.String("table", table))
	default:
		s.metrics.Ran(table, ResultOK)
		s.logger.Info("retention job finished", slog.String("table", table), slog.Int64("deleted", res.Deleted))
	}
}
