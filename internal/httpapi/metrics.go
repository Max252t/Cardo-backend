package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const unmatchedRoute = "unmatched"

type Metrics interface {
	ObserveRequest(method, route string, status int, d time.Duration)
}

type nopMetrics struct{}

func (nopMetrics) ObserveRequest(string, string, int, time.Duration) {}

func metricsMiddleware(m Metrics) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if isUpgrade(c.Request) {
			return
		}
		route := c.FullPath()
		if route == "" {
			route = unmatchedRoute
		}
		m.ObserveRequest(c.Request.Method, route, c.Writer.Status(), time.Since(start))
	}
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
