package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type patchMeRequest struct {
	DisplayName *string `json:"display_name"`
	AvatarColor *string `json:"avatar_color"`
}

func (h *handlers) getMe(c *gin.Context) {
	u, err := h.users.Get(c, currentUserID(c))
	if err != nil {
		h.failCurrentUser(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(u))
}

func (h *handlers) patchMe(c *gin.Context) {
	var req patchMeRequest
	if !h.bindJSON(c, &req) {
		return
	}
	u, err := h.users.UpdateProfile(c, currentUserID(c), domain.ProfileUpdate(req))
	if err != nil {
		h.failCurrentUser(c, err)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(u))
}
