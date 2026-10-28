package metrics

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Max252t/Cardo-backend/internal/boardevents"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const otherLabel = "other"

var knownMethods = map[string]struct{}{
	http.MethodGet:     {},
	http.MethodHead:    {},
	http.MethodPost:    {},
	http.MethodPut:     {},
	http.MethodPatch:   {},
	http.MethodDelete:  {},
	http.MethodOptions: {},
}

var knownCloseCodes = map[int]struct{}{
	protocol.CloseNormal:        {},
	protocol.CloseGoingAway:     {},
	protocol.CloseWSProtocol:    {},
	protocol.CloseNoStatus:      {},
	protocol.CloseAbnormal:      {},
	protocol.CloseMessageTooBig: {},
	protocol.CloseInternalError: {},
	protocol.CloseTryAgainLater: {},
	protocol.CloseInvalidToken:  {},
	protocol.CloseAccessRevoked: {},
	protocol.CloseBoardDeleted:  {},
	protocol.CloseRateLimited:   {},
	protocol.CloseSlowClient:    {},
	protocol.CloseProtocolError: {},
}

type Collector struct {
	registry      *prometheus.Registry
	wsConnections prometheus.Gauge
	wsClosed      *prometheus.CounterVec
	resyncs       *prometheus.CounterVec
	ops           *prometheus.CounterVec
	opDuration    *prometheus.HistogramVec
	publishErrors prometheus.Counter
	httpDuration  *prometheus.HistogramVec
	retained      *prometheus.CounterVec
	restLimited   prometheus.Counter
	restLimitErrs prometheus.Counter
}

func New() *Collector {
	reg := prometheus.NewRegistry()
	c := &Collector{
		registry: reg,
		wsConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "ws_connections",
			Help: "Currently open WebSocket connections.",
		}),
		wsClosed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ws_closed_total",
			Help: "Closed WebSocket connections by close code.",
		}, []string{"code"}),
		resyncs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "resync_total",
			Help: "WebSocket handshakes by catch-up kind.",
		}, []string{"kind"}),
		ops: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "ops_total",
			Help: "Processed board operations by type and result.",
		}, []string{"type", "result"}),
		opDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "op_apply_duration_seconds",
			Help:    "Time to apply a board operation by type.",
			Buckets: prometheus.DefBuckets,
		}, []string{"type"}),
		publishErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "event_publish_errors_total",
			Help: "Failed board event publish batches.",
		}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration by method, route template and status.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "status"}),
		retained: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "retention_deleted_total",
			Help: "Rows removed by the retention jobs by table.",
		}, []string{"table"}),
		restLimited: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rest_rate_limited_total",
			Help: "REST requests rejected by the per-user rate limit.",
		}),
		restLimitErrs: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "rest_rate_limiter_errors_total",
			Help: "REST rate limiter backend failures, requests are allowed through.",
		}),
	}
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		c.wsConnections, c.wsClosed, c.resyncs, c.ops, c.opDuration, c.publishErrors, c.httpDuration, c.retained, c.restLimited, c.restLimitErrs,
	)
	return c
}

func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{Registry: c.registry})
}

func (c *Collector) ObserveOp(opType, result string, d time.Duration) {
	c.ops.WithLabelValues(opType, result).Inc()
	c.opDuration.WithLabelValues(opType).Observe(d.Seconds())
}

func (c *Collector) ConnectionOpened() {
	c.wsConnections.Inc()
}

func (c *Collector) ConnectionClosed(code int) {
	c.wsConnections.Dec()
	c.wsClosed.WithLabelValues(closeCodeLabel(code)).Inc()
}

func (c *Collector) ConnectionRefused(code int) {
	c.wsClosed.WithLabelValues(closeCodeLabel(code)).Inc()
}

func (c *Collector) OpRejected(opType, reason string) {
	c.ops.WithLabelValues(opType, reason).Inc()
}

func (c *Collector) Resync(kind string) {
	c.resyncs.WithLabelValues(kind).Inc()
}

func (c *Collector) ObserveRequest(method, route string, status int, d time.Duration) {
	if _, ok := knownMethods[method]; !ok {
		method = otherLabel
	}
	c.httpDuration.WithLabelValues(method, route, strconv.Itoa(status)).Observe(d.Seconds())
}

func (c *Collector) Deleted(table string, n int64) {
	c.retained.WithLabelValues(table).Add(float64(n))
}

func (c *Collector) RateLimited() {
	c.restLimited.Inc()
}

func (c *Collector) RateLimiterError() {
	c.restLimitErrs.Inc()
}

func (c *Collector) InstrumentPublisher(p boardevents.Publisher) boardevents.Publisher {
	return boardevents.PublisherFunc(func(ctx context.Context, events []domain.Event) error {
		err := p.Publish(ctx, events)
		if err != nil {
			c.publishErrors.Inc()
		}
		return err
	})
}

func closeCodeLabel(code int) string {
	if _, ok := knownCloseCodes[code]; !ok {
		return otherLabel
	}
	return strconv.Itoa(code)
}
