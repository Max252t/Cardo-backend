package domain

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrForbidden         = errors.New("forbidden")
	ErrEmailTaken        = errors.New("email already registered")
	ErrInvalidCredential = errors.New("invalid credentials")
	ErrRateLimited       = errors.New("rate limited")
	ErrTokenInvalid      = errors.New("token invalid")
	ErrTokenExpired      = errors.New("token expired")
	ErrTokenReused       = errors.New("refresh token reused")
	ErrOverloaded        = errors.New("server overloaded")
	ErrBusy              = errors.New("resource busy")
	ErrGone              = errors.New("gone")
	ErrAlreadyMember     = errors.New("already a member")
	ErrInviteUnavailable = errors.New("invite is revoked, expired or exhausted")
)

type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return ErrRateLimited.Error()
}

func (e *RateLimitError) Unwrap() error {
	return ErrRateLimited
}

type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

func NewValidationError(field, message string) error {
	return &ValidationError{Field: field, Message: message}
}

type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

func (r Role) Valid() bool {
	switch r {
	case RoleOwner, RoleEditor, RoleViewer:
		return true
	}
	return false
}

func (r Role) rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

func (r Role) AtLeast(min Role) bool {
	return r.rank() >= min.rank() && r.rank() > 0
}

type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string
	DisplayName  string
	AvatarColor  string
	CreatedAt    time.Time
}

type ProfileUpdate struct {
	DisplayName *string
	AvatarColor *string
}

type AccessClaims struct {
	UserID    uuid.UUID
	ExpiresAt time.Time
}

type RefreshToken struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	FamilyID  uuid.UUID
	TokenHash string
	ExpiresAt time.Time
	RevokedAt *time.Time
	CreatedAt time.Time
}

type Board struct {
	ID        uuid.UUID
	Title     string
	OwnerID   uuid.UUID
	Seq       int64
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt *time.Time
}

type BoardWithRole struct {
	Board
	Role Role
}

const (
	EventBoardRenamed = "board.renamed"
	EventBoardDeleted = "board.deleted"
)

type Event struct {
	BoardID   uuid.UUID
	Seq       int64
	Type      string
	Payload   json.RawMessage
	OpID      *uuid.UUID
	ActorID   *uuid.UUID
	CreatedAt time.Time
}

const (
	MaxCardTitleLength   = 500
	MaxDescriptionLength = 10000
	MaxColumnTitleLength = 200
)

type Column struct {
	ID         uuid.UUID
	BoardID    uuid.UUID
	Title      string
	Rank       string
	Version    int64
	ArchivedAt *time.Time
}

type Card struct {
	ID                 uuid.UUID
	BoardID            uuid.UUID
	ColumnID           uuid.UUID
	Title              string
	Description        string
	Rank               string
	Version            int64
	DescriptionVersion int64
	CreatedBy          uuid.UUID
	UpdatedBy          uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ArchivedAt         *time.Time
}

type Member struct {
	UserID      uuid.UUID
	DisplayName string
	AvatarColor string
	Role        Role
	JoinedAt    time.Time
}

type BoardSnapshot struct {
	Board   BoardWithRole
	Columns []Column
	Cards   []Card
	Members []Member
	Seq     int64
}

type EventPage struct {
	Events  []Event
	Seq     int64
	HasMore bool
}

type PresenceUser struct {
	ID          uuid.UUID
	Name        string
	AvatarColor string
}

type CatchUp struct {
	Snapshot *BoardSnapshot
	Events   []Event
	Seq      int64
}

func (c CatchUp) IsSnapshot() bool {
	return c.Snapshot != nil
}

type Invite struct {
	ID        uuid.UUID
	BoardID   uuid.UUID
	TokenHash string
	Role      Role
	CreatedBy uuid.UUID
	ExpiresAt time.Time
	MaxUses   int
	Uses      int
	RevokedAt *time.Time
}

func (i Invite) Usable(now time.Time) bool {
	return i.RevokedAt == nil && now.Before(i.ExpiresAt) && i.Uses < i.MaxUses
}

type NewInvite struct {
	Invite
	Token string
}
