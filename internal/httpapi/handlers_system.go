package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	readinessTimeout = 2 * time.Second
	statusOK         = "ok"
	checkUnavailable = "unavailable"
)

func (h *handlers) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": statusOK})
}

func (h *handlers) readyz(c *gin.Context) {
	if h.shuttingDown != nil && h.shuttingDown() {
		c.Header("Retry-After", unavailableRetryAfter)
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, readinessResponse{
			Error:  &ErrorBody{Code: CodeShuttingDown, Message: "server is shutting down"},
			Checks: map[string]string{},
		})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), readinessTimeout)
	defer cancel()
	checks, failed := h.runReadinessChecks(ctx)
	if len(failed) > 0 {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, readinessResponse{
			Error:  &ErrorBody{Code: CodeNotReady, Message: "dependencies unavailable: " + strings.Join(failed, ", ")},
			Checks: checks,
		})
		return
	}
	c.JSON(http.StatusOK, readinessResponse{Status: statusOK, Checks: checks})
}

func (h *handlers) runReadinessChecks(ctx context.Context) (checks map[string]string, failed []string) {
	names := make([]string, 0, len(h.readiness))
	for name := range h.readiness {
		names = append(names, name)
	}
	sort.Strings(names)
	checks = make(map[string]string, len(names))
	for _, name := range names {
		if err := h.readiness[name].Ping(ctx); err != nil {
			checks[name] = checkUnavailable
			failed = append(failed, name)
			h.logger.WarnContext(ctx, "readiness check failed", slog.String("check", name), slog.Any("error", err))
			continue
		}
		checks[name] = statusOK
	}
	return checks, failed
}
