package main

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/jwtauth"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/presencestore"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type realtimeStack struct {
	hub    *realtime.Hub
	boards *boards.Service
	stop   func()
}

func newRealtimeStack(cfg config.Config, store *storage.Store, rdb *redis.Client, jwt *jwtauth.Manager, logger *slog.Logger) (*realtimeStack, error) {
	broker, presenceStore := newTransport(rdb, logger)
	publisher := pubsub.NewEventPublisher(broker)
	boardService := boards.NewService(store, boards.WithPublisher(publisher), boards.WithLogger(logger))
	processor := ops.NewProcessor(store, ops.WithPublisher(publisher), ops.WithLogger(logger))
	clock := realtime.SystemClock{}
	presenceService := presence.New(presenceStore, broker, clock, uuid.NewString())

	hub, err := realtime.NewHub(realtime.Deps{
		Bus:      broker,
		Presence: presenceService,
		Ops:      processor,
		Sync:     boardService,
		Members:  store,
		Tokens:   jwt,
		Clock:    clock,
		Logger:   logger,
	}, realtime.Config{AllowedOrigins: cfg.WSAllowedOrigins})
	if err != nil {
		_ = broker.Close()
		return nil, err
	}

	runCtx, cancel := context.WithCancel(context.Background())
	go pubsub.Pump(broker, hub.Dispatch)
	go hub.Run(runCtx)
	return &realtimeStack{
		hub:    hub,
		boards: boardService,
		stop: func() {
			cancel()
			if err := broker.Close(); err != nil {
				logger.Warn("close broker", slog.Any("error", err))
			}
		},
	}, nil
}

func newTransport(rdb *redis.Client, logger *slog.Logger) (pubsub.Broker, presence.Store) {
	if rdb != nil {
		return pubsub.NewRedisBroker(context.Background(), rdb), presencestore.NewRedisStore(rdb)
	}
	logger.Warn("SINGLE_INSTANCE is enabled: using in-process pub/sub and presence")
	return pubsub.NewMemoryNetwork().NewBroker(), presencestore.NewMemoryStore()
}
