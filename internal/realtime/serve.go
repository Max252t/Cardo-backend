package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const handshakeBufferSize = 1024

func (h *Hub) Authorize(ctx context.Context, userID, boardID uuid.UUID) (domain.Member, error) {
	return h.members.BoardMember(ctx, boardID, userID)
}

func (h *Hub) upgrader() *websocket.Upgrader {
	return &websocket.Upgrader{
		ReadBufferSize:  handshakeBufferSize,
		WriteBufferSize: handshakeBufferSize,
		CheckOrigin:     h.checkOrigin,
		Error:           rejectUpgrade,
	}
}

func (h *Hub) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if len(h.cfg.AllowedOrigins) == 0 {
		u, err := url.Parse(origin)
		return err == nil && strings.EqualFold(u.Host, r.Host)
	}
	return slices.ContainsFunc(h.cfg.AllowedOrigins, func(allowed string) bool {
		return strings.EqualFold(strings.TrimRight(allowed, "/"), strings.TrimRight(origin, "/"))
	})
}

func (h *Hub) Serve(w http.ResponseWriter, r *http.Request, boardID uuid.UUID, member domain.Member, tokenExpiresAt time.Time) error {
	conn, err := h.upgrader().Upgrade(w, r, nil)
	if err != nil {
		return fmt.Errorf("upgrade websocket: %w", err)
	}
	c := newClient(h, conn, boardID, member, tokenExpiresAt)
	if err := h.join(c); err != nil {
		code := closeCodeFor(err)
		h.metrics.ConnectionRefused(code)
		writeCloseFrame(conn, code, "unavailable", h.cfg.WriteWait)
		c.cancel()
		_ = conn.Close()
		return err
	}
	h.metrics.ConnectionOpened()
	go c.run()
	return nil
}

func closeCodeFor(err error) int {
	if errors.Is(err, ErrShuttingDown) {
		return protocol.CloseGoingAway
	}
	return protocol.CloseTryAgainLater
}

type upgradeErrorBody struct {
	Error upgradeErrorDetail `json:"error"`
}

type upgradeErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func rejectUpgrade(w http.ResponseWriter, _ *http.Request, status int, _ error) {
	code, message := "bad_request", "invalid websocket handshake"
	if status == http.StatusForbidden {
		code, message = "forbidden", "origin is not allowed"
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(upgradeErrorBody{Error: upgradeErrorDetail{Code: code, Message: message}})
}
