package httpapi

import (
	"errors"
	"log/slog"
	"net/http"

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
	CodeInternal         = "internal"
)

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

func abortWithError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorResponse{Error: ErrorBody{Code: code, Message: message}})
}

func (h *handlers) fail(c *gin.Context, err error) {
	var vErr *domain.ValidationError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &vErr):
		abortWithError(c, http.StatusBadRequest, CodeValidation, vErr.Error())
	case errors.As(err, &tooLarge):
		abortWithError(c, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "request body is too large")
	case errors.Is(err, errBadJSON):
		abortWithError(c, http.StatusBadRequest, CodeBadRequest, "request body must be valid JSON")
	case errors.Is(err, domain.ErrNotFound):
		abortWithError(c, http.StatusNotFound, CodeNotFound, "resource not found")
	case errors.Is(err, domain.ErrForbidden):
		abortWithError(c, http.StatusForbidden, CodeForbidden, "insufficient permissions")
	case errors.Is(err, domain.ErrEmailTaken):
		abortWithError(c, http.StatusConflict, CodeEmailTaken, "email is already registered")
	case errors.Is(err, domain.ErrInvalidCredential):
		abortWithError(c, http.StatusUnauthorized, CodeLoginFailed, "invalid email or password")
	case errors.Is(err, domain.ErrRateLimited):
		c.Header("Retry-After", "60")
		abortWithError(c, http.StatusTooManyRequests, CodeRateLimited, "too many attempts, try again later")
	case errors.Is(err, domain.ErrTokenExpired):
		abortWithError(c, http.StatusUnauthorized, CodeTokenExpired, "token has expired")
	case errors.Is(err, domain.ErrTokenReused):
		abortWithError(c, http.StatusUnauthorized, CodeTokenReused, "refresh token was already used, session revoked")
	case errors.Is(err, domain.ErrTokenInvalid):
		abortWithError(c, http.StatusUnauthorized, CodeInvalidToken, "token is invalid")
	default:
		h.logger.ErrorContext(c.Request.Context(), "request failed",
			slog.String("request_id", requestID(c)),
			slog.String("route", c.FullPath()),
			slog.Any("error", err),
		)
		abortWithError(c, http.StatusInternalServerError, CodeInternal, "internal server error")
	}
}
