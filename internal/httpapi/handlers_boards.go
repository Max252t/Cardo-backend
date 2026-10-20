package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/Max252t/Cardo-backend/internal/protocol"
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
	boardID, ok := h.boardIDFromPath(c)
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
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	if _, err := h.boards.Delete(c, currentUserID(c), boardID); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handlers) getBoard(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	snap, err := h.boards.Snapshot(c, currentUserID(c), boardID)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, protocol.FromSnapshot(snap))
}

func (h *handlers) boardEvents(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	afterSeq, ok := h.afterSeqFromQuery(c)
	if !ok {
		return
	}
	page, err := h.boards.Events(c, currentUserID(c), boardID, afterSeq)
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, protocol.FromEventPage(page))
}
