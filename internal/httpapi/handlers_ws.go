package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type ClaimsParser interface {
	Parse(token string) (domain.AccessClaims, error)
}

type BoardSockets interface {
	Authorize(ctx context.Context, userID, boardID uuid.UUID) (domain.Member, error)
	Serve(w http.ResponseWriter, r *http.Request, boardID uuid.UUID, member domain.Member, tokenExpiresAt time.Time) error
}

var shuttingDownError = apiError{
	status:     http.StatusServiceUnavailable,
	code:       CodeShuttingDown,
	message:    shuttingDownMessage,
	retryAfter: unavailableRetryAfter,
}

func (h *handlers) connectBoardSocket(c *gin.Context) {
	if h.isShuttingDown() {
		abortWithAPIError(c, shuttingDownError)
		return
	}
	token, ok := h.bearerToken(c)
	if !ok {
		return
	}
	claims, err := h.claims.Parse(token)
	if err != nil {
		h.rejectToken(c, err)
		return
	}
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	member, err := h.sockets.Authorize(c, claims.UserID, boardID)
	if err != nil {
		h.fail(c, err)
		return
	}
	if err := h.sockets.Serve(c.Writer, c.Request, boardID, member, claims.ExpiresAt); err != nil {
		h.logger.WarnContext(c.Request.Context(), "websocket connection rejected",
			slog.String("request_id", requestID(c)),
			slog.String("board_id", boardID.String()),
			slog.String("user_id", claims.UserID.String()),
			slog.Any("error", err),
		)
	}
}
