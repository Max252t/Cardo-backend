package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
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
	resp := boardListResponse{Boards: make([]boardResponse, 0, len(list))}
	for _, b := range list {
		resp.Boards = append(resp.Boards, toBoardResponse(b))
	}
	return resp
}

type readinessResponse struct {
	Status string            `json:"status,omitempty"`
	Error  *ErrorBody        `json:"error,omitempty"`
	Checks map[string]string `json:"checks"`
}
