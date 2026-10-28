package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

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

func toTokenResponse(p auth.TokenPair, user *domain.User, now time.Time) tokenResponse {
	resp := tokenResponse{
		TokenType:        "Bearer",
		AccessToken:      p.AccessToken,
		AccessExpiresAt:  p.AccessExpiresAt.UTC(),
		ExpiresIn:        secondsUntil(now, p.AccessExpiresAt),
		RefreshToken:     p.RefreshToken,
		RefreshExpiresAt: p.RefreshExpiresAt.UTC(),
	}
	if user != nil {
		u := toUserResponse(*user)
		resp.User = &u
	}
	return resp
}

func secondsUntil(now, deadline time.Time) int64 {
	secs := int64(deadline.Sub(now).Round(time.Second) / time.Second)
	return max(secs, 0)
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

type boardListResponse struct {
	Boards []boardResponse `json:"boards"`
}

func toBoardListResponse(list []domain.BoardWithRole) boardListResponse {
	return boardListResponse{Boards: mapSlice(list, toBoardResponse)}
}

type readinessResponse struct {
	Status string            `json:"status,omitempty"`
	Error  *ErrorBody        `json:"error,omitempty"`
	Checks map[string]string `json:"checks"`
}

func mapSlice[T, U any](in []T, fn func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, fn(v))
	}
	return out
}

type memberListResponse struct {
	Members []protocol.Member `json:"members"`
}

func toMemberListResponse(list []domain.Member) memberListResponse {
	return memberListResponse{Members: mapSlice(list, protocol.FromMember)}
}

type inviteResponse struct {
	ID        uuid.UUID   `json:"id"`
	Role      domain.Role `json:"role"`
	ExpiresAt time.Time   `json:"expires_at"`
	MaxUses   int         `json:"max_uses"`
	Uses      int         `json:"uses"`
}

func toInviteResponse(inv domain.Invite) inviteResponse {
	return inviteResponse{ID: inv.ID, Role: inv.Role, ExpiresAt: inv.ExpiresAt.UTC(), MaxUses: inv.MaxUses, Uses: inv.Uses}
}

type createdInviteResponse struct {
	inviteResponse
	Token string `json:"token"`
}

func toCreatedInviteResponse(created domain.NewInvite) createdInviteResponse {
	return createdInviteResponse{inviteResponse: toInviteResponse(created.Invite), Token: created.Token}
}

type inviteListResponse struct {
	Invites []inviteResponse `json:"invites"`
}

func toInviteListResponse(list []domain.Invite) inviteListResponse {
	return inviteListResponse{Invites: mapSlice(list, toInviteResponse)}
}
