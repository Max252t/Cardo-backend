package main

import (
	"net/http"

	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/config"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/metrics"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/retention"
)

type observability struct {
	ops       ops.Metrics
	realtime  realtime.Metrics
	http      httpapi.Metrics
	retention retention.Metrics
	rest      httpapi.RateLimitMetrics
	handler   http.Handler
	collector *metrics.Collector
}

func newObservability(cfg config.Config) observability {
	if cfg.MetricsAddr == "" {
		return observability{}
	}
	c := metrics.New()
	return observability{
		ops:       c,
		realtime:  c,
		http:      c,
		retention: c,
		rest:      c,
		handler:   c.Handler(),
		collector: c,
	}
}

func (o observability) instrument(p boardevents.Publisher) boardevents.Publisher {
	if o.collector == nil {
		return p
	}
	return o.collector.InstrumentPublisher(p)
}
