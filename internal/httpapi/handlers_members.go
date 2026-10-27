package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/members"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const (
	memberIDParam = "userId"
	inviteIDParam = "inviteId"
	tokenParam    = "token"
)

type MemberService interface {
	List(ctx context.Context, userID, boardID uuid.UUID) ([]domain.Member, error)
	Add(ctx context.Context, actorID, boardID uuid.UUID, email string, role domain.Role) (domain.Member, error)
	ChangeRole(ctx context.Context, actorID, boardID, targetID uuid.UUID, role domain.Role) (domain.Member, error)
	Remove(ctx context.Context, actorID, boardID, targetID uuid.UUID) error
	CreateInvite(ctx context.Context, actorID, boardID uuid.UUID, in members.InviteInput) (domain.NewInvite, error)
	ListInvites(ctx context.Context, actorID, boardID uuid.UUID) ([]domain.Invite, error)
	RevokeInvite(ctx context.Context, actorID, boardID, inviteID uuid.UUID) error
	Accept(ctx context.Context, userID uuid.UUID, token string) (domain.BoardWithRole, error)
}

type addMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

type changeRoleRequest struct {
	Role string `json:"role"`
}

type createInviteRequest struct {
	Role      string `json:"role"`
	ExpiresIn int64  `json:"expires_in"`
	MaxUses   int    `json:"max_uses"`
}

type memberListResponse struct {
	Members []protocol.Member `json:"members"`
}

type inviteResponse struct {
	ID        uuid.UUID   `json:"id"`
	Role      domain.Role `json:"role"`
	ExpiresAt time.Time   `json:"expires_at"`
	MaxUses   int         `json:"max_uses"`
	Uses      int         `json:"uses"`
}

type createdInviteResponse struct {
	inviteResponse
	Token string `json:"token"`
}

type inviteListResponse struct {
	Invites []inviteResponse `json:"invites"`
}

func toInviteResponse(inv domain.Invite) inviteResponse {
	return inviteResponse{ID: inv.ID, Role: inv.Role, ExpiresAt: inv.ExpiresAt.UTC(), MaxUses: inv.MaxUses, Uses: inv.Uses}
}

func (h *handlers) uuidParam(c *gin.Context, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param(name))
	if err != nil {
		h.fail(c, domain.NewValidationError(name, "must be a UUID"))
		return uuid.Nil, false
	}
	return id, true
}

func (h *handlers) listMembers(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	list, err := h.members.List(c, currentUserID(c), boardID)
	if err != nil {
		h.fail(c, err)
		return
	}
	resp := memberListResponse{Members: make([]protocol.Member, 0, len(list))}
	for _, m := range list {
		resp.Members = append(resp.Members, protocol.FromMember(m))
	}
	c.JSON(http.StatusOK, resp)
}

func (h *handlers) addMember(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	var req addMemberRequest
	if !h.bindJSON(c, &req) {
		return
	}
	m, err := h.members.Add(c, currentUserID(c), boardID, req.Email, domain.Role(req.Role))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, protocol.FromMember(m))
}

func (h *handlers) changeMemberRole(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	targetID, ok := h.uuidParam(c, memberIDParam)
	if !ok {
		return
	}
	var req changeRoleRequest
	if !h.bindJSON(c, &req) {
		return
	}
	m, err := h.members.ChangeRole(c, currentUserID(c), boardID, targetID, domain.Role(req.Role))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, protocol.FromMember(m))
}

func (h *handlers) removeMember(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	targetID, ok := h.uuidParam(c, memberIDParam)
	if !ok {
		return
	}
	if err := h.members.Remove(c, currentUserID(c), boardID, targetID); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handlers) createInvite(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	var req createInviteRequest
	if !h.bindJSON(c, &req) {
		return
	}
	created, err := h.members.CreateInvite(c, currentUserID(c), boardID, members.InviteInput{
		Role:      domain.Role(req.Role),
		ExpiresIn: req.ExpiresIn,
		MaxUses:   req.MaxUses,
	})
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, createdInviteResponse{inviteResponse: toInviteResponse(created.Invite), Token: created.Token})
}

func (h *handlers) listInvites(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	list, err := h.members.ListInvites(c, currentUserID(c), boardID)
	if err != nil {
		h.fail(c, err)
		return
	}
	resp := inviteListResponse{Invites: make([]inviteResponse, 0, len(list))}
	for _, inv := range list {
		resp.Invites = append(resp.Invites, toInviteResponse(inv))
	}
	c.JSON(http.StatusOK, resp)
}

func (h *handlers) revokeInvite(c *gin.Context) {
	boardID, ok := h.boardIDFromPath(c)
	if !ok {
		return
	}
	inviteID, ok := h.uuidParam(c, inviteIDParam)
	if !ok {
		return
	}
	if err := h.members.RevokeInvite(c, currentUserID(c), boardID, inviteID); err != nil {
		h.fail(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *handlers) acceptInvite(c *gin.Context) {
	b, err := h.members.Accept(c, currentUserID(c), c.Param(tokenParam))
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, toBoardResponse(b))
}
