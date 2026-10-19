package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

var errBadJSON = errors.New("invalid json body")

func decodeJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return errBadJSON
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return err
		}
		return errBadJSON
	}
	return nil
}

type userResponse struct {
	ID          uuid.UUID `json:"id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	AvatarColor string    `json:"avatar_color"`
	CreatedAt   time.Time `json:"created_at"`
}

func toUserResponse(u domain.User) userResponse {
	return userResponse{
		ID:          u.ID,
		Email:       u.Email,
		DisplayName: u.DisplayName,
		AvatarColor: u.AvatarColor,
		CreatedAt:   u.CreatedAt.UTC(),
	}
}

type tokenResponse struct {
	User             *userResponse `json:"user,omitempty"`
	TokenType        string        `json:"token_type"`
	AccessToken      string        `json:"access_token"`
	AccessExpiresAt  time.Time     `json:"access_expires_at"`
	ExpiresIn        int64         `json:"expires_in"`
	RefreshToken     string        `json:"refresh_token"`
	RefreshExpiresAt time.Time     `json:"refresh_expires_at"`
}

func toTokenResponse(p auth.TokenPair, user *domain.User) tokenResponse {
	resp := tokenResponse{
		TokenType:        "Bearer",
		AccessToken:      p.AccessToken,
		AccessExpiresAt:  p.AccessExpiresAt.UTC(),
		ExpiresIn:        int64(time.Until(p.AccessExpiresAt).Round(time.Second) / time.Second),
		RefreshToken:     p.RefreshToken,
		RefreshExpiresAt: p.RefreshExpiresAt.UTC(),
	}
	if resp.ExpiresIn < 0 {
		resp.ExpiresIn = 0
	}
	if user != nil {
		u := toUserResponse(*user)
		resp.User = &u
	}
	return resp
}

type boardResponse struct {
	ID        uuid.UUID   `json:"id"`
	Title     string      `json:"title"`
	OwnerID   uuid.UUID   `json:"owner_id"`
	Seq       int64       `json:"seq"`
	Role      domain.Role `json:"role"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

func toBoardResponse(b domain.BoardWithRole) boardResponse {
	return boardResponse{
		ID:        b.ID,
		Title:     b.Title,
		OwnerID:   b.OwnerID,
		Seq:       b.Seq,
		Role:      b.Role,
		CreatedAt: b.CreatedAt.UTC(),
		UpdatedAt: b.UpdatedAt.UTC(),
	}
}

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	AvatarColor string `json:"avatar_color"`
}

func (h *handlers) register(c *gin.Context) {
	var req registerRequest
	if err := decodeJSON(c, &req); err != nil {
		h.fail(c, err)
		return
	}
	s, err := h.auth.Register(c, auth.RegisterInput(req))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, toTokenResponse(s.Tokens, &s.User))
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (h *handlers) login(c *gin.Context) {
	var req loginRequest
	if err := decodeJSON(c, &req); err != nil {
		h.fail(c, err)
		return
	}
	s, err := h.auth.Login(c, auth.LoginInput{Email: req.Email, Password: req.Password, ClientIP: c.ClientIP()})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toTokenResponse(s.Tokens, &s.User))
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *handlers) refresh(c *gin.Context) {
	var req refreshRequest
	if err := decodeJSON(c, &req); err != nil {
		h.fail(c, err)
		return
	}
	if req.RefreshToken == "" {
		h.fail(c, domain.NewValidationError("refresh_token", "is required"))
		return
	}
	pair, err := h.auth.Refresh(c, req.RefreshToken)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toTokenResponse(pair, nil))
}

func (h *handlers) logout(c *gin.Context) {
	var req refreshRequest
	if err := decodeJSON(c, &req); err != nil {
		h.fail(c, err)
		return
	}
	if req.RefreshToken == "" {
		h.fail(c, domain.NewValidationError("refresh_token", "is required"))
		return
	}
	if err := h.auth.Logout(c, mustUserID(c), req.RefreshToken); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handlers) getMe(c *gin.Context) {
	u, err := h.users.Get(c, mustUserID(c))
	if errors.Is(err, domain.ErrNotFound) {
		abortWithError(c, http.StatusUnauthorized, CodeUnauthorized, "user no longer exists")
		return
	}
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(u))
}

type patchMeRequest struct {
	DisplayName *string `json:"display_name"`
	AvatarColor *string `json:"avatar_color"`
}

func (h *handlers) patchMe(c *gin.Context) {
	var req patchMeRequest
	if err := decodeJSON(c, &req); err != nil {
		h.fail(c, err)
		return
	}
	u, err := h.users.UpdateProfile(c, mustUserID(c), domain.ProfileUpdate(req))
	if errors.Is(err, domain.ErrNotFound) {
		abortWithError(c, http.StatusUnauthorized, CodeUnauthorized, "user no longer exists")
		return
	}
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(u))
}

type boardListResponse struct {
	Boards []boardResponse `json:"boards"`
}

func (h *handlers) listBoards(c *gin.Context) {
	list, err := h.boards.List(c, mustUserID(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	resp := boardListResponse{Boards: make([]boardResponse, 0, len(list))}
	for _, b := range list {
		resp.Boards = append(resp.Boards, toBoardResponse(b))
	}
	c.JSON(http.StatusOK, resp)
}

type boardTitleRequest struct {
	Title string `json:"title"`
}

func (h *handlers) createBoard(c *gin.Context) {
	var req boardTitleRequest
	if err := decodeJSON(c, &req); err != nil {
		h.fail(c, err)
		return
	}
	b, err := h.boards.Create(c, mustUserID(c), req.Title)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, toBoardResponse(b))
}

func (h *handlers) renameBoard(c *gin.Context) {
	boardID, ok := h.boardIDParam(c)
	if !ok {
		return
	}
	var req boardTitleRequest
	if err := decodeJSON(c, &req); err != nil {
		h.fail(c, err)
		return
	}
	b, _, err := h.boards.Rename(c, mustUserID(c), boardID, req.Title)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toBoardResponse(b))
}

func (h *handlers) deleteBoard(c *gin.Context) {
	boardID, ok := h.boardIDParam(c)
	if !ok {
		return
	}
	if _, err := h.boards.Delete(c, mustUserID(c), boardID); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handlers) boardIDParam(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		h.fail(c, domain.NewValidationError("id", "must be a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func (h *handlers) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

const readinessTimeout = 2 * time.Second

func (h *handlers) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), readinessTimeout)
	defer cancel()
	names := make([]string, 0, len(h.readiness))
	for name := range h.readiness {
		names = append(names, name)
	}
	sort.Strings(names)
	checks := make(map[string]string, len(names))
	var failed []string
	for _, name := range names {
		if err := h.readiness[name].Ping(ctx); err != nil {
			checks[name] = "unavailable"
			failed = append(failed, name)
			h.logger.WarnContext(ctx, "readiness check failed", slog.String("check", name), slog.Any("error", err))
			continue
		}
		checks[name] = "ok"
	}
	if len(failed) > 0 {
		c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
			"error":  ErrorBody{Code: CodeNotReady, Message: "dependencies unavailable: " + strings.Join(failed, ", ")},
			"checks": checks,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "checks": checks})
}
