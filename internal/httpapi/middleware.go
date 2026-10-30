package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	ctxRequestID  = "request_id"
	ctxUserID     = "user_id"
	headerReqID   = "X-Request-ID"
	authChallenge = `Bearer realm="cardo"`
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader(headerReqID)
		if !requestIDPattern.MatchString(id) {
			id = uuid.NewString()
		}
		c.Set(ctxRequestID, id)
		c.Header(headerReqID, id)
		c.Next()
	}
}

func requestID(c *gin.Context) string {
	return c.GetString(ctxRequestID)
}

func (h *handlers) logMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := routeLabel(c)
		status := c.Writer.Status()
		level := slog.LevelInfo
		switch {
		case route == "/healthz" || route == "/readyz":
			level = slog.LevelDebug
		case status >= http.StatusInternalServerError:
			level = slog.LevelError
		}
		attrs := []slog.Attr{
			slog.String("request_id", requestID(c)),
			slog.String("method", c.Request.Method),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("client_ip", c.ClientIP()),
		}
		if uid, ok := userIDFrom(c); ok {
			attrs = append(attrs, slog.String("user_id", uid.String()))
		}
		h.logger.LogAttrs(c.Request.Context(), level, "http request", attrs...)
	}
}

func (h *handlers) recoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(rec)
			}
			h.logger.ErrorContext(c.Request.Context(), "panic recovered",
				slog.String("request_id", requestID(c)),
				slog.String("route", c.FullPath()),
				slog.Any("panic", rec),
				slog.String("stack", string(debug.Stack())),
			)
			if !c.Writer.Written() {
				abortWithError(c, http.StatusInternalServerError, CodeInternal, "internal server error")
				return
			}
			c.Abort()
		}()
		c.Next()
	}
}

func bodyLimitMiddleware(limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
		}
		c.Next()
	}
}

func (h *handlers) bearerToken(c *gin.Context) (string, bool) {
	header := c.GetHeader("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "Bearer") || token == "" {
		c.Header("WWW-Authenticate", authChallenge)
		abortWithError(c, http.StatusUnauthorized, CodeUnauthorized, "missing bearer token")
		return "", false
	}
	return token, true
}

func (h *handlers) rejectToken(c *gin.Context, err error) {
	c.Header("WWW-Authenticate", authChallenge+`, error="invalid_token"`)
	h.fail(c, err)
}

func (h *handlers) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		token, ok := h.bearerToken(c)
		if !ok {
			return
		}
		userID, err := h.tokens.Verify(token)
		if err != nil {
			h.rejectToken(c, err)
			return
		}
		c.Set(ctxUserID, userID)
		c.Next()
	}
}

func userIDFrom(c *gin.Context) (uuid.UUID, bool) {
	v, ok := c.Get(ctxUserID)
	if !ok {
		return uuid.Nil, false
	}
	id, ok := v.(uuid.UUID)
	return id, ok
}

func currentUserID(c *gin.Context) uuid.UUID {
	id, _ := userIDFrom(c)
	return id
}
