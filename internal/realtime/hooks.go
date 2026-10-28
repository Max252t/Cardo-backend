package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func defaultHooks() map[string][]EventHook {
	return map[string][]EventHook{
		boardcontent.EventBoardDeleted:      {closeBoardOnDelete},
		boardcontent.EventMemberRemoved:     {closeRemovedMember},
		boardcontent.EventMemberRoleChanged: {applyRoleChange},
	}
}

func closeBoardOnDelete(h *Hub, boardID uuid.UUID, _ protocol.Envelope) {
	h.CloseBoard(boardID, protocol.CloseBoardDeleted, "board deleted")
}

func closeRemovedMember(h *Hub, boardID uuid.UUID, ev protocol.Envelope) {
	var removed boardcontent.MemberRemoved
	if err := json.Unmarshal(ev.Payload, &removed); err != nil {
		h.logger.Warn("dropping malformed member.removed event", slog.String("board_id", boardID.String()), slog.Any("error", err))
		return
	}
	h.CloseUser(boardID, removed.UserID, protocol.CloseAccessRevoked, "access revoked")
}

func applyRoleChange(h *Hub, boardID uuid.UUID, ev protocol.Envelope) {
	var changed boardcontent.MemberRoleChanged
	if err := json.Unmarshal(ev.Payload, &changed); err != nil || !changed.Role.Valid() {
		h.logger.Warn("dropping malformed member.role_changed event", slog.String("board_id", boardID.String()))
		return
	}
	h.SetRole(boardID, changed.UserID, changed.Role)
}

func (h *Hub) revalidateMembers(ctx context.Context, boardID uuid.UUID) {
	byUser := make(map[uuid.UUID][]*Client)
	for _, c := range h.clientsWhere(boardID, anyClient) {
		byUser[c.member.UserID] = append(byUser[c.member.UserID], c)
	}
	for userID, clients := range byUser {
		opCtx, cancel := context.WithTimeout(ctx, h.cfg.BusTimeout)
		member, err := h.members.BoardMember(opCtx, boardID, userID)
		cancel()
		switch {
		case errors.Is(err, domain.ErrNotFound):
			for _, c := range clients {
				c.closeWith(protocol.CloseAccessRevoked, "access revoked")
			}
		case err != nil:
			h.logger.Warn("member revalidation failed", slog.String("board_id", boardID.String()), slog.Any("error", err))
		default:
			for _, c := range clients {
				c.setRole(member.Role)
			}
		}
	}
}

func (c *Client) recheckMembership(ctx context.Context) {
	member, err := c.hub.members.BoardMember(ctx, c.boardID, c.member.UserID)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		c.closeWith(protocol.CloseAccessRevoked, "access revoked")
	case err != nil:
		c.hub.logger.Warn("membership recheck failed", c.logAttrs(slog.Any("error", err))...)
	default:
		c.setRole(member.Role)
	}
}

func (c *Client) canWrite() bool {
	if c.Role().AtLeast(domain.RoleEditor) {
		return true
	}
	now := c.hub.clock.Now().UnixNano()
	last := c.roleCheckedAt.Load()
	if last != 0 && now-last < int64(roleRecheckInterval) {
		return false
	}
	c.roleCheckedAt.Store(now)
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.cfg.BusTimeout)
	defer cancel()
	c.recheckMembership(ctx)
	return c.Role().AtLeast(domain.RoleEditor)
}
