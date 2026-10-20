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
	if err := run(); err != nil {
		slog.Error("fatal", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openDatabase(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()
	store := storage.New(pool, storage.WithLockTimeout(cfg.DBLockTimeout))
	readiness := map[string]httpapi.Checker{"postgres": store}

	rdb, err := openRedis(cfg.RedisURL)
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

	rt, err := newRealtimeStack(cfg, store, rdb, jwtManager, logger)
	if err != nil {
		return fmt.Errorf("build realtime: %w", err)
	}
	defer rt.stop()

	lc := newLifecycle()
	lc.closeSockets = rt.hub.Shutdown
	defer lc.cancelBase()

	gin.SetMode(gin.ReleaseMode)
	router, err := httpapi.NewRouter(httpapi.Deps{
		Logger:         logger,
		Auth:           authService,
		Tokens:         jwtManager,
		Users:          users.NewService(store),
		Boards:         rt.boards,
		Claims:         jwtManager,
		Sockets:        rt.hub,
		Readiness:      readiness,
		ShuttingDown:   lc.draining.Load,
		TrustedProxies: cfg.TrustedProxies,
		MaxBodyBytes:   cfg.MaxBodyBytes,
	})
	if err != nil {
		return err
	}

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

func openRedis(redisURL string) (*redis.Client, error) {
	if redisURL == "" {
		return nil, nil
	}
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
	if cfg.LoginLimiter == config.LoginLimiterMemory || rdb == nil {
		logger.Warn("using per-instance in-memory login rate limiter, not suitable for multiple instances")
		return ratelimit.NewMemoryLimiter(cfg.LoginMaxFailures, cfg.LoginFailureWindow, nil)
	}
	return ratelimit.NewRedisLimiter(rdb, cfg.LoginMaxFailures, cfg.LoginFailureWindow)
}

type lifecycle struct {
	draining     atomic.Bool
	closeSockets func(ctx context.Context) error
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

func serve(ctx context.Context, stopSignals context.CancelFunc, srv *http.Server, lc *lifecycle, cfg config.Config, logger *slog.Logger) error {
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
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
	lc.cancelBase()
	if err != nil {
		logger.Warn("graceful shutdown timed out, cancelling in-flight requests", slog.Any("error", err))
		if closeErr := srv.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return nil
}
