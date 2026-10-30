package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
)

func requireValidationField(t *testing.T, err error, field string) {
	t.Helper()
	require.Error(t, err)
	var vErr *domain.ValidationError
	require.True(t, errors.As(err, &vErr), "expected ValidationError, got %T", err)
	assert.Equal(t, field, vErr.Field)
	assert.NotEmpty(t, vErr.Message)
	assert.Contains(t, err.Error(), field)
}

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()
	longValid := strings.Repeat("a", 64) + "@" + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 55) + ".com"
	tests := []struct {
		name    string
		in      string
		want    string
		invalid bool
	}{
		{name: "simple", in: "user@example.com", want: "user@example.com"},
		{name: "lowercased and trimmed", in: "  User@Example.COM \n", want: "user@example.com"},
		{name: "plus tag", in: "a+b@example.com", want: "a+b@example.com"},
		{name: "empty", in: "", invalid: true},
		{name: "blank", in: "   ", invalid: true},
		{name: "no at", in: "userexample.com", invalid: true},
		{name: "no domain", in: "user@", invalid: true},
		{name: "display name form", in: "John <john@example.com>", invalid: true},
		{name: "angle brackets only", in: "<john@example.com>", invalid: true},
		{name: "two addresses", in: "a@example.com, b@example.com", invalid: true},
		{name: "space inside", in: "us er@example.com", invalid: true},
		{name: "too long", in: strings.Repeat("a", 250) + "@example.com", invalid: true},
		{name: "max length ok", in: longValid, want: longValid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NormalizeEmail(tt.in)
			if tt.invalid {
				requireValidationField(t, err, "email")
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		invalid bool
	}{
		{name: "min length", in: "12345678"},
		{name: "max length", in: strings.Repeat("a", 128)},
		{name: "too short", in: "1234567", invalid: true},
		{name: "empty", in: "", invalid: true},
		{name: "too long", in: strings.Repeat("a", 129), invalid: true},
		{name: "counts runes not bytes at min", in: "пароль12"},
		{name: "counts runes not bytes at max", in: strings.Repeat("я", 128)},
		{name: "runes too short", in: "пароль1", invalid: true},
		{name: "runes too long", in: strings.Repeat("я", 129), invalid: true},
		{name: "spaces allowed", in: "        "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := domain.ValidatePassword(tt.in)
			if tt.invalid {
				requireValidationField(t, err, "password")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestNormalizeDisplayName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		invalid bool
	}{
		{name: "plain", in: "Alice", want: "Alice"},
		{name: "trimmed", in: "  Alice Smith  ", want: "Alice Smith"},
		{name: "unicode", in: "Алиса", want: "Алиса"},
		{name: "empty", in: "", invalid: true},
		{name: "blank", in: " \t ", invalid: true},
		{name: "max runes", in: strings.Repeat("я", 100), want: strings.Repeat("я", 100)},
		{name: "over max runes", in: strings.Repeat("я", 101), invalid: true},
		{name: "trim before counting", in: " " + strings.Repeat("a", 100) + " ", want: strings.Repeat("a", 100)},
		{name: "nul byte", in: "Ali\x00ce", invalid: true},
		{name: "inner newline", in: "Ali\nce", invalid: true},
		{name: "invalid utf8", in: "Ali\xffce", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NormalizeDisplayName(tt.in)
			if tt.invalid {
				requireValidationField(t, err, "display_name")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeAvatarColor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		invalid bool
	}{
		{name: "upper", in: "#1A2B3C", want: "#1A2B3C"},
		{name: "lower is uppercased", in: "#abcdef", want: "#ABCDEF"},
		{name: "trimmed", in: " #00ff00 ", want: "#00FF00"},
		{name: "no hash", in: "1A2B3C", invalid: true},
		{name: "short form", in: "#fff", invalid: true},
		{name: "too long", in: "#1A2B3C4D", invalid: true},
		{name: "non hex", in: "#GGGGGG", invalid: true},
		{name: "empty", in: "", invalid: true},
		{name: "named color", in: "red", invalid: true},
		{name: "garbage after newline", in: "#1A2B3C\nx", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NormalizeAvatarColor(tt.in)
			if tt.invalid {
				requireValidationField(t, err, "avatar_color")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNormalizeBoardTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		in      string
		want    string
		invalid bool
	}{
		{name: "plain", in: "Sprint", want: "Sprint"},
		{name: "trimmed", in: "  Sprint 1 ", want: "Sprint 1"},
		{name: "empty", in: "", invalid: true},
		{name: "blank", in: "   ", invalid: true},
		{name: "max", in: strings.Repeat("ж", 200), want: strings.Repeat("ж", 200)},
		{name: "over max", in: strings.Repeat("ж", 201), invalid: true},
		{name: "nul byte", in: "Spr\x00int", invalid: true},
		{name: "escape char", in: "Spr\x1bint", invalid: true},
		{name: "invalid utf8", in: "Spr\xffint", invalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := domain.NormalizeBoardTitle(tt.in)
			if tt.invalid {
				requireValidationField(t, err, "title")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDefaultAvatarColor(t *testing.T) {
	t.Parallel()
	assert.Equal(t, domain.DefaultAvatarColor("alice@example.com"), domain.DefaultAvatarColor("alice@example.com"))
	seen := map[string]struct{}{}
	for _, seed := range []string{"", "a", "b", "alice@example.com", "bob@example.com", "carol@example.com", "x", "y", "z", "1", "2", "3", "4", "5"} {
		c := domain.DefaultAvatarColor(seed)
		normalized, err := domain.NormalizeAvatarColor(c)
		require.NoError(t, err, "palette color %q must be a valid avatar color", c)
		assert.Equal(t, normalized, c)
		seen[c] = struct{}{}
	}
	assert.Greater(t, len(seen), 1)
}

func TestRole(t *testing.T) {
	t.Parallel()
	for _, r := range []domain.Role{domain.RoleOwner, domain.RoleEditor, domain.RoleViewer} {
		assert.True(t, r.Valid(), r)
	}
	for _, r := range []domain.Role{"", "admin", "OWNER"} {
		assert.False(t, r.Valid(), r)
	}

	tests := []struct {
		role, min domain.Role
		want      bool
	}{
		{domain.RoleOwner, domain.RoleOwner, true},
		{domain.RoleOwner, domain.RoleEditor, true},
		{domain.RoleOwner, domain.RoleViewer, true},
		{domain.RoleEditor, domain.RoleOwner, false},
		{domain.RoleEditor, domain.RoleEditor, true},
		{domain.RoleEditor, domain.RoleViewer, true},
		{domain.RoleViewer, domain.RoleOwner, false},
		{domain.RoleViewer, domain.RoleEditor, false},
		{domain.RoleViewer, domain.RoleViewer, true},
		{"", domain.RoleViewer, false},
		{"", "", false},
		{"admin", domain.RoleViewer, false},
		{"admin", "", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.role)+">="+string(tt.min), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.role.AtLeast(tt.min))
		})
	}
}

func TestValidationErrorMessage(t *testing.T) {
	t.Parallel()
	assert.EqualError(t, domain.NewValidationError("f", "bad"), "f: bad")
}
