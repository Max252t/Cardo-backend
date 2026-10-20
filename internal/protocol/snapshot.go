package protocol

import (
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

type Member struct {
	UserID      uuid.UUID   `json:"user_id"`
	DisplayName string      `json:"display_name"`
	AvatarColor string      `json:"avatar_color"`
	Role        domain.Role `json:"role"`
	JoinedAt    time.Time   `json:"joined_at"`
}

func FromMember(m domain.Member) Member {
	return Member{
		UserID:      m.UserID,
		DisplayName: m.DisplayName,
		AvatarColor: m.AvatarColor,
		Role:        m.Role,
		JoinedAt:    m.JoinedAt.UTC(),
	}
}

type BoardInfo struct {
	ID        uuid.UUID   `json:"id"`
	Title     string      `json:"title"`
	OwnerID   uuid.UUID   `json:"owner_id"`
	Role      domain.Role `json:"role"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

func FromBoard(b domain.BoardWithRole) BoardInfo {
	return BoardInfo{
		ID:        b.ID,
		Title:     b.Title,
		OwnerID:   b.OwnerID,
		Role:      b.Role,
		CreatedAt: b.CreatedAt.UTC(),
		UpdatedAt: b.UpdatedAt.UTC(),
	}
}

type Snapshot struct {
	Board   BoardInfo `json:"board"`
	Columns []Column  `json:"columns"`
	Cards   []Card    `json:"cards"`
	Members []Member  `json:"members"`
	Seq     int64     `json:"seq"`
}

func FromSnapshot(s domain.BoardSnapshot) Snapshot {
	return Snapshot{
		Board:   FromBoard(s.Board),
		Columns: mapSlice(s.Columns, FromColumn),
		Cards:   mapSlice(s.Cards, FromCard),
		Members: mapSlice(s.Members, FromMember),
		Seq:     s.Seq,
	}
}

type EventsPage struct {
	Events  []Envelope `json:"events"`
	Seq     int64      `json:"seq"`
	HasMore bool       `json:"has_more"`
}

func FromEventPage(p domain.EventPage) EventsPage {
	return EventsPage{Events: mapSlice(p.Events, EventEnvelope), Seq: p.Seq, HasMore: p.HasMore}
}

func mapSlice[T, U any](in []T, fn func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, fn(v))
	}
	return out
}
