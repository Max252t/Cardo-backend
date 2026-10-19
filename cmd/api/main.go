package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
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

	pool, err := storage.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}
	if cfg.MigrateOnStart {
		if err := storage.Migrate(ctx, pool, logger); err != nil {
			return err
		}
	}
	store := storage.New(pool)

	readiness := map[string]httpapi.Checker{"postgres": store}
	var limiter auth.LoginLimiter
	if cfg.RedisURL != "" {
		opts, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return fmt.Errorf("parse redis url: %w", err)
		}
		rdb := redis.NewClient(opts)
		defer func() {
			if err := rdb.Close(); err != nil {
				logger.Warn("close redis", slog.Any("error", err))
			}
		}()
		readiness["redis"] = httpapi.CheckerFunc(func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		})
		limiter = auth.NewRedisLimiter(rdb, cfg.LoginMaxFailures, cfg.LoginFailureWindow)
	} else {
		logger.Warn("REDIS_URL is not set, using in-memory login rate limiter")
		limiter = auth.NewMemoryLimiter(cfg.LoginMaxFailures, cfg.LoginFailureWindow, nil)
	}

	jwtManager := auth.NewJWTManager(auth.JWTConfig{
		Secret:   cfg.JWTSecret,
		Issuer:   cfg.JWTIssuer,
		Audience: cfg.JWTAudience,
		TTL:      cfg.AccessTokenTTL,
	}, nil)
	authService := auth.NewService(auth.Deps{
		Store:      store,
		Hasher:     auth.NewArgon2Hasher(auth.DefaultArgon2Params),
		Access:     jwtManager,
		Limiter:    limiter,
		RefreshTTL: cfg.RefreshTokenTTL,
	})

	gin.SetMode(gin.ReleaseMode)
	router, err := httpapi.NewRouter(httpapi.Deps{
		Logger:         logger,
		Auth:           authService,
		Tokens:         jwtManager,
		Users:          users.NewService(store),
		Boards:         boards.NewService(store),
		Readiness:      readiness,
		TrustedProxies: cfg.TrustedProxies,
		MaxBodyBytes:   cfg.MaxBodyBytes,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http server listening", slog.String("addr", cfg.HTTPAddr))
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
	stop()

	logger.Info("shutting down", slog.Duration("timeout", cfg.ShutdownTimeout))
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("server stopped")
	return nil
}
