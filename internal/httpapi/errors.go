package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	CodeBadRequest       = "bad_request"
	CodeValidation       = "validation_error"
	CodeUnauthorized     = "unauthorized"
	CodeInvalidToken     = "invalid_token"
	CodeTokenExpired     = "token_expired"
	CodeTokenReused      = "token_reused"
	CodeLoginFailed      = "invalid_credentials"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeEmailTaken       = "email_taken"
	CodePayloadTooLarge  = "payload_too_large"
	CodeRateLimited      = "rate_limited"
	CodeNotReady         = "not_ready"
	CodeOverloaded       = "overloaded"
	CodeBusy             = "busy"
	CodeShuttingDown     = "shutting_down"
	CodeGone             = "gone"
	CodeInternal         = "internal"
)

const (
	defaultRateLimitRetryAfter = "60"
	unavailableRetryAfter      = "1"
)

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type apiError struct {
	status     int
	code       string
	message    string
	retryAfter string
}

var sentinelErrors = []struct {
	target error
	apiError
}{
	{errBadJSON, apiError{status: http.StatusBadRequest, code: CodeBadRequest, message: "request body must be valid JSON"}},
	{domain.ErrNotFound, apiError{status: http.StatusNotFound, code: CodeNotFound, message: "resource not found"}},
	{domain.ErrForbidden, apiError{status: http.StatusForbidden, code: CodeForbidden, message: "insufficient permissions"}},
	{domain.ErrEmailTaken, apiError{status: http.StatusConflict, code: CodeEmailTaken, message: "email is already registered"}},
	{domain.ErrInvalidCredential, apiError{status: http.StatusUnauthorized, code: CodeLoginFailed, message: "invalid email or password"}},
	{domain.ErrRateLimited, apiError{status: http.StatusTooManyRequests, code: CodeRateLimited, message: "too many attempts, try again later", retryAfter: defaultRateLimitRetryAfter}},
	{domain.ErrOverloaded, apiError{status: http.StatusServiceUnavailable, code: CodeOverloaded, message: "server is overloaded, try again later", retryAfter: unavailableRetryAfter}},
	{domain.ErrGone, apiError{status: http.StatusGone, code: CodeGone, message: "requested events are no longer retained, load a snapshot"}},
	{domain.ErrBusy, apiError{status: http.StatusServiceUnavailable, code: CodeBusy, message: "resource is busy, try again later", retryAfter: unavailableRetryAfter}},
	{domain.ErrTokenExpired, apiError{status: http.StatusUnauthorized, code: CodeTokenExpired, message: "token has expired"}},
	{domain.ErrTokenReused, apiError{status: http.StatusUnauthorized, code: CodeTokenReused, message: "refresh token was already used, session revoked"}},
	{domain.ErrTokenInvalid, apiError{status: http.StatusUnauthorized, code: CodeInvalidToken, message: "token is invalid"}},
}

func mapError(err error) (apiError, bool) {
	var rlErr *domain.RateLimitError
	if errors.As(err, &rlErr) {
		return apiError{status: http.StatusTooManyRequests, code: CodeRateLimited, message: "too many attempts, try again later", retryAfter: retryAfterSeconds(rlErr.RetryAfter)}, true
	}
	var vErr *domain.ValidationError
	if errors.As(err, &vErr) {
		return apiError{status: http.StatusBadRequest, code: CodeValidation, message: vErr.Error()}, true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return apiError{status: http.StatusRequestEntityTooLarge, code: CodePayloadTooLarge, message: "request body is too large"}, true
	}
	for _, s := range sentinelErrors {
		if errors.Is(err, s.target) {
			return s.apiError, true
		}
	}
	return apiError{}, false
}

func abortWithError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: ErrorBody{Code: code, Message: message}})
}

func abortWithAPIError(c *gin.Context, e apiError) {
	if e.retryAfter != "" {
		c.Header("Retry-After", e.retryAfter)
	}
	abortWithError(c, e.status, e.code, e.message)
}

func (h *handlers) fail(c *gin.Context, err error) {
	if e, ok := mapError(err); ok {
		abortWithAPIError(c, e)
		return
	}
	h.logger.ErrorContext(c.Request.Context(), "request failed",
		slog.String("request_id", requestID(c)),
		slog.String("route", c.FullPath()),
		slog.Any("error", err),
	)
	abortWithError(c, http.StatusInternalServerError, CodeInternal, "internal server error")
}

func (h *handlers) failCurrentUser(c *gin.Context, err error) {
	if errors.Is(err, domain.ErrNotFound) {
		abortWithError(c, http.StatusUnauthorized, CodeUnauthorized, "user no longer exists")
		return
	}
	h.fail(c, err)
}

func retryAfterSeconds(d time.Duration) string {
	if d <= 0 {
		return defaultRateLimitRetryAfter
	}
	secs := int64((d + time.Second - 1) / time.Second)
	return strconv.FormatInt(secs, 10)
}
