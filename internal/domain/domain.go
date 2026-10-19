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
)

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
