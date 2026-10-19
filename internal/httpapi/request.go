package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

var errBadJSON = errors.New("invalid json body")

func decodeJSON(c *gin.Context, dst any) error {
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(dst); err != nil {
		return bodyReadError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bodyReadError(err)
	}
	return nil
}

func bodyReadError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return err
	}
	return errBadJSON
}

func (h *handlers) bindJSON(c *gin.Context, dst any) bool {
	if err := decodeJSON(c, dst); err != nil {
		h.fail(c, err)
		return false
	}
	return true
}

func (h *handlers) boardIDFromPath(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(boardIDParam))
	if err != nil {
		h.fail(c, domain.NewValidationError(boardIDParam, "must be a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

const afterSeqQuery = "after_seq"

func (h *handlers) afterSeqFromQuery(c *gin.Context) (int64, bool) {
	raw, present := c.GetQuery(afterSeqQuery)
	if !present {
		h.fail(c, domain.NewValidationError(afterSeqQuery, "is required"))
		return 0, false
	}
	afterSeq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || afterSeq < 0 {
		h.fail(c, domain.NewValidationError(afterSeqQuery, "must be a non-negative integer"))
		return 0, false
	}
	return afterSeq, true
}
