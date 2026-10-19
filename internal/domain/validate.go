package domain

import (
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxEmailLength       = 254
	MinPasswordLength    = 8
	MaxPasswordLength    = 128
	MaxDisplayNameLength = 100
	MaxBoardTitleLength  = 200
)

var avatarColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" {
		return "", NewValidationError("email", "is required")
	}
	if len(email) > MaxEmailLength {
		return "", NewValidationError("email", "is too long")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || addr.Name != "" {
		return "", NewValidationError("email", "is invalid")
	}
	return email, nil
}

func ValidatePassword(password string) error {
	n := utf8.RuneCountInString(password)
	if n < MinPasswordLength {
		return NewValidationError("password", "must be at least 8 characters")
	}
	if n > MaxPasswordLength {
		return NewValidationError("password", "must be at most 128 characters")
	}
	return nil
}

func NormalizeDisplayName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if err := checkText("display_name", name); err != nil {
		return "", err
	}
	n := utf8.RuneCountInString(name)
	if n == 0 {
		return "", NewValidationError("display_name", "is required")
	}
	if n > MaxDisplayNameLength {
		return "", NewValidationError("display_name", "must be at most 100 characters")
	}
	return name, nil
}

func NormalizeAvatarColor(raw string) (string, error) {
	color := strings.TrimSpace(raw)
	if !avatarColorPattern.MatchString(color) {
		return "", NewValidationError("avatar_color", "must be a hex color like #1A2B3C")
	}
	return strings.ToUpper(color), nil
}

func NormalizeBoardTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if err := checkText("title", title); err != nil {
		return "", err
	}
	n := utf8.RuneCountInString(title)
	if n == 0 {
		return "", NewValidationError("title", "is required")
	}
	if n > MaxBoardTitleLength {
		return "", NewValidationError("title", "must be at most 200 characters")
	}
	return title, nil
}

const avatarPaletteSize = 12

var avatarPalette = [avatarPaletteSize]string{
	"#E57373", "#F06292", "#BA68C8", "#9575CD",
	"#7986CB", "#64B5F6", "#4FC3F7", "#4DD0E1",
	"#4DB6AC", "#81C784", "#AED581", "#FFB74D",
}

func DefaultAvatarColor(seed string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(seed); i++ {
		h ^= uint32(seed[i])
		h *= 16777619
	}
	return avatarPalette[h%avatarPaletteSize]
}

func checkText(field, s string) error {
	if !utf8.ValidString(s) {
		return NewValidationError(field, "must be valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return NewValidationError(field, "must not contain control characters")
		}
	}
	return nil
}
