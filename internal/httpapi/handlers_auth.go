package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

type registerRequest struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	AvatarColor string `json:"avatar_color"`
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func (h *handlers) register(c *gin.Context) {
	var req registerRequest
	if !h.bindJSON(c, &req) {
		return
	}
	s, err := h.auth.Register(c, auth.RegisterInput(req))
	if err != nil {
		h.fail(c, err)
		return
	}
	h.respondSession(c, http.StatusCreated, s)
}

func (h *handlers) login(c *gin.Context) {
	var req loginRequest
	if !h.bindJSON(c, &req) {
		return
	}
	s, err := h.auth.Login(c, auth.LoginInput{Email: req.Email, Password: req.Password, ClientIP: c.ClientIP()})
	if err != nil {
		h.fail(c, err)
		return
	}
	h.respondSession(c, http.StatusOK, s)
}

func (h *handlers) refresh(c *gin.Context) {
	token, ok := h.bindRefreshToken(c)
	if !ok {
		return
	}
	pair, err := h.auth.Refresh(c, token)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toTokenResponse(pair, nil, h.clock.Now()))
}

func (h *handlers) logout(c *gin.Context) {
	token, ok := h.bindRefreshToken(c)
	if !ok {
		return
	}
	if err := h.auth.Logout(c, currentUserID(c), token); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handlers) bindRefreshToken(c *gin.Context) (string, bool) {
	var req refreshRequest
	if !h.bindJSON(c, &req) {
		return "", false
	}
	if req.RefreshToken == "" {
		h.fail(c, domain.NewValidationError("refresh_token", "is required"))
		return "", false
	}
	return req.RefreshToken, true
}

func (h *handlers) respondSession(c *gin.Context, status int, s auth.Session) {
	c.JSON(status, toTokenResponse(s.Tokens, &s.User, h.clock.Now()))
}
