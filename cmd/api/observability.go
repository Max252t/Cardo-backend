package main

import (
	"net/http"

	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/metrics"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/realtime"
)

type observability struct {
	ops      ops.Metrics
	realtime realtime.Metrics
	http     httpapi.Metrics
	handler  http.Handler
	wrap     func(boardevents.Publisher) boardevents.Publisher
}

func newObservability(cfg config.Config) observability {
	if !cfg.MetricsEnabled {
		return observability{wrap: func(p boardevents.Publisher) boardevents.Publisher { return p }}
	}
	c := metrics.New()
	return observability{
		ops:      c,
		realtime: c,
		http:     c,
		handler:  c.Handler(),
		wrap:     c.InstrumentPublisher,
	}
}
