package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const limiterWarnEvery = 30 * time.Second

type RateLimiter interface {
	Allow(ctx context.Context, key string) (allowed bool, retryAfter time.Duration, err error)
}

type RateLimitMetrics interface {
	RateLimited()
	RateLimiterError()
}

type nopRateLimitMetrics struct{}

func (nopRateLimitMetrics) RateLimited()      {}
func (nopRateLimitMetrics) RateLimiterError() {}

func (h *handlers) rateLimitMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID, ok := userIDFrom(c)
		if !ok {
			c.Next()
			return
		}
		allowed, retryAfter, err := h.limiter.Allow(c.Request.Context(), userID.String())
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				h.limiterFailed(c, err)
			}
			c.Next()
			return
		}
		if allowed {
			c.Next()
			return
		}
		h.rateMetrics.RateLimited()
		abortWithAPIError(c, apiError{
			status:     http.StatusTooManyRequests,
			code:       CodeRateLimited,
			message:    "too many requests, try again later",
			retryAfter: retryAfterSeconds(max(retryAfter, time.Nanosecond)),
		})
	}
}

func (h *handlers) limiterFailed(c *gin.Context, err error) {
	h.rateMetrics.RateLimiterError()
	now := h.clock.Now().UnixNano()
	last := h.limiterWarnedAt.Load()
	if last != 0 && now-last < int64(limiterWarnEvery) {
		return
	}
	if !h.limiterWarnedAt.CompareAndSwap(last, now) {
		return
	}
	h.logger.WarnContext(c.Request.Context(), "rest rate limiter unavailable, allowing requests",
		slog.String("request_id", requestID(c)),
		slog.Any("error", err),
	)
}
