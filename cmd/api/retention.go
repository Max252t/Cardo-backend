package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/retention"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

func startRetention(ctx context.Context, cfg config.Config, store *storage.Store, obs observability, logger *slog.Logger) (func(), error) {
	if !cfg.RetentionEnabled {
		logger.Info("retention jobs are disabled")
		return func() {}, nil
	}
	svc, err := retention.New(store, cfg.RetentionConfig(), retention.WithLogger(logger), retention.WithMetrics(obs.retention))
	if err != nil {
		return nil, fmt.Errorf("build retention: %w", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		svc.Run(runCtx)
	}()
	return func() {
		cancel()
		done.Wait()
	}, nil
}
