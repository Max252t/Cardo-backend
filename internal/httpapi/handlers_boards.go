package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

const boardIDParam = "id"

type boardTitleRequest struct {
	Title string `json:"title"`
}

func (h *handlers) listBoards(c *gin.Context) {
	list, err := h.boards.List(c, currentUserID(c))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toBoardListResponse(list))
}

func (h *handlers) createBoard(c *gin.Context) {
	var req boardTitleRequest
	if !h.bindJSON(c, &req) {
		return
	}
	b, err := h.boards.Create(c, currentUserID(c), req.Title)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, toBoardResponse(b))
}

func (h *handlers) renameBoard(c *gin.Context) {
	boardID, ok := h.uuidParam(c, boardIDParam)
	if !ok {
		return
	}
	var req boardTitleRequest
	if !h.bindJSON(c, &req) {
		return
	}
	b, _, err := h.boards.Rename(c, currentUserID(c), boardID, req.Title)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toBoardResponse(b))
}

func (h *handlers) deleteBoard(c *gin.Context) {
	boardID, ok := h.uuidParam(c, boardIDParam)
	if !ok {
		return
	}
	if _, err := h.boards.Delete(c, currentUserID(c), boardID); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}
