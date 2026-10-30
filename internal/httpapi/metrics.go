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
		status := c.Writer.Status()
		if isUpgrade(c.Request) && status == http.StatusOK {
			return
		}
		m.ObserveRequest(c.Request.Method, routeLabel(c), status, time.Since(start))
	}
}

func routeLabel(c *gin.Context) string {
	if route := c.FullPath(); route != "" {
		return route
	}
	return unmatchedRoute
}

func isUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}
