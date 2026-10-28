package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Max252t/Cardo-backend/internal/argon2id"
	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/jwtauth"
	"github.com/Max252t/Cardo-backend/internal/ratelimit"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/users"
)

func main() {
	exitOnHealthcheck()
	cfg, cfgErr := config.Load()
	logger := newLogger(cfg)
	if cfgErr != nil {
		logger.Error("fatal", slog.Any("error", fmt.Errorf("load config: %w", cfgErr)))
		os.Exit(1)
	}
	if err := run(cfg, logger); err != nil {
		logger.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
}

func run(cfg config.Config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openDatabase(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := storage.New(pool, storage.WithLockTimeout(cfg.DBLockTimeout))
	readiness := map[string]httpapi.Checker{"postgres": store}

	rdb, err := openRedis(cfg)
	if err != nil {
		return err
	}
	if rdb != nil {
		defer closeRedis(rdb, logger)
		readiness["redis"] = httpapi.CheckerFunc(func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		})
	}

	jwtManager := jwtauth.New(jwtauth.Config{
		Secret:   cfg.JWTSecret,
		Issuer:   cfg.JWTIssuer,
		Audience: cfg.JWTAudience,
		TTL:      cfg.AccessTokenTTL,
	}, nil)
	authService := auth.NewService(auth.Deps{
		Store: store,
		Hasher: argon2id.New(argon2id.DefaultParams,
			argon2id.WithMaxConcurrent(cfg.HashMaxConcurrent),
			argon2id.WithAcquireTimeout(cfg.HashAcquireTimeout),
		),
		Access:     jwtManager,
		Limiter:    newLoginLimiter(cfg, rdb, logger),
		RefreshTTL: cfg.RefreshTokenTTL,
	})

	obs := newObservability(cfg)
	rt, err := newRealtimeStack(cfg, store, rdb, jwtManager, obs, logger)
	if err != nil {
		return fmt.Errorf("build realtime: %w", err)
	}
	defer rt.stop()

	stopRetention, err := startRetention(ctx, cfg, store, obs, logger)
	if err != nil {
		return err
	}
	defer stopRetention()

	lc := newLifecycle()
	lc.closeSockets = rt.hub.Shutdown
	defer lc.cancelBase()

	restLimiter, err := newRESTLimiter(cfg, rdb, logger)
	if err != nil {
		return err
	}

	gin.SetMode(gin.ReleaseMode)
	router, err := httpapi.NewRouter(httpapi.Deps{
		Logger:         logger,
		Auth:           authService,
		Tokens:         jwtManager,
		Users:          users.NewService(store),
		Boards:         rt.boards,
		Claims:         jwtManager,
		Sockets:        rt.hub,
		Members:        rt.members,
		Readiness:      readiness,
		ShuttingDown:   lc.draining.Load,
		TrustedProxies: cfg.TrustedProxies,
		MaxBodyBytes:   cfg.MaxBodyBytes,
		Metrics:        obs.http,
		RateLimiter:    restLimiter,
		RateMetrics:    obs.rest,
	})
	if err != nil {
		return err
	}

	lc.metricsSrv = newMetricsServer(cfg, obs, logger)
	return serve(ctx, stop, newHTTPServer(cfg, router, logger, lc.baseCtx), lc, cfg, logger)
}

func openDatabase(ctx context.Context, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	pool, err := storage.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns, storage.WithStatementTimeout(cfg.DBStatementTimeout))
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if cfg.MigrateOnStart {
		if err := storage.Migrate(ctx, pool, logger); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return pool, nil
}

func openRedis(cfg config.Config) (*redis.Client, error) {
	if cfg.SingleInstance {
		return nil, nil
	}
	redisURL := cfg.RedisURL
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	return redis.NewClient(opts), nil
}

func closeRedis(rdb *redis.Client, logger *slog.Logger) {
	if err := rdb.Close(); err != nil {
		logger.Warn("close redis", slog.Any("error", err))
	}
}

func newLoginLimiter(cfg config.Config, rdb *redis.Client, logger *slog.Logger) auth.LoginLimiter {
	if rdb == nil {
		logger.Warn("SINGLE_INSTANCE is enabled: using the in-memory login rate limiter")
		return ratelimit.NewMemoryLimiter(cfg.LoginMaxFailures, cfg.LoginFailureWindow, nil)
	}
	return ratelimit.NewRedisLimiter(rdb, cfg.LoginMaxFailures, cfg.LoginFailureWindow)
}

func newRESTLimiter(cfg config.Config, rdb *redis.Client, logger *slog.Logger) (httpapi.RateLimiter, error) {
	if cfg.RESTRateLimit == 0 {
		logger.Warn("REST_RATE_LIMIT is 0: the per-user REST rate limit is disabled")
		return nil, nil
	}
	if rdb == nil {
		logger.Warn("SINGLE_INSTANCE is enabled: using the in-memory REST rate limiter")
		l, err := ratelimit.NewMemoryRESTLimiter(cfg.RESTRateLimit, cfg.RESTRateBurst, nil)
		if err != nil {
			return nil, fmt.Errorf("build rest limiter: %w", err)
		}
		return l, nil
	}
	l, err := ratelimit.NewRedisRESTLimiter(rdb, cfg.RESTRateLimit, cfg.RESTRateBurst)
	if err != nil {
		return nil, fmt.Errorf("build rest limiter: %w", err)
	}
	return l, nil
}

type lifecycle struct {
	draining     atomic.Bool
	closeSockets func(ctx context.Context) error
	metricsSrv   *http.Server
	baseCtx      context.Context
	cancelBase   context.CancelFunc
}

func newLifecycle() *lifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &lifecycle{baseCtx: ctx, cancelBase: cancel}
}

func newHTTPServer(cfg config.Config, handler http.Handler, logger *slog.Logger, baseCtx context.Context) *http.Server {
	return &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
}

func newMetricsServer(cfg config.Config, obs observability, logger *slog.Logger) *http.Server {
	if obs.handler == nil {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", obs.handler)
	return &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

func serve(ctx context.Context, stopSignals context.CancelFunc, srv *http.Server, lc *lifecycle, cfg config.Config, logger *slog.Logger) error {
	serveErr := make(chan error, 2)
	var servers sync.WaitGroup
	servers.Add(1)
	go func() {
		defer servers.Done()
		logger.Info("http server listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("http server: %w", err)
		}
	}()
	if lc.metricsSrv != nil {
		servers.Add(1)
		go func() {
			defer servers.Done()
			logger.Info("metrics server listening", slog.String("addr", lc.metricsSrv.Addr))
			if err := lc.metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("metrics server: %w", err)
			}
		}()
	}
	go func() {
		servers.Wait()
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	stopSignals()
	return shutdown(srv, lc, cfg.ShutdownDrain, cfg.ShutdownTimeout, logger)
}

func shutdown(srv *http.Server, lc *lifecycle, drain, timeout time.Duration, logger *slog.Logger) error {
	lc.draining.Store(true)
	logger.Info("draining before shutdown", slog.Duration("delay", drain))
	time.Sleep(drain)

	logger.Info("shutting down", slog.Duration("timeout", timeout))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if lc.closeSockets != nil {
		if drainErr := lc.closeSockets(shutdownCtx); drainErr != nil {
			logger.Warn("closing websocket connections timed out", slog.Any("error", drainErr))
		}
	}
	err := srv.Shutdown(shutdownCtx)
	if lc.metricsSrv != nil {
		err = errors.Join(err, lc.metricsSrv.Shutdown(shutdownCtx))
	}
	lc.cancelBase()
	if err != nil {
		logger.Warn("graceful shutdown timed out, cancelling in-flight requests", slog.Any("error", err))
		err = errors.Join(err, srv.Close())
		if lc.metricsSrv != nil {
			err = errors.Join(err, lc.metricsSrv.Close())
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return nil
}
