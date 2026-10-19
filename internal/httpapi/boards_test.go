package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

const sampleBoardID = "22222222-2222-7222-8222-222222222222"

func strPtr(s string) *string { return &s }

func TestGetMe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.users.get = func(_ context.Context, id uuid.UUID) (domain.User, error) {
		assert.Equal(t, testUserID, id)
		return sampleUser(), nil
	}
	rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.json(t)
	assert.Equal(t, testUserID.String(), body["id"])
	assert.Equal(t, "alice@example.com", body["email"])
	assert.Equal(t, "Alice", body["display_name"])
	assert.Equal(t, "#ABCDEF", body["avatar_color"])
	assert.NotContains(t, body, "password_hash")
}

func TestGetMeErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"user deleted", domain.ErrNotFound, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"unexpected", errors.New("db down"), http.StatusInternalServerError, httpapi.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.users.get = func(context.Context, uuid.UUID) (domain.User, error) { return domain.User{}, tt.err }
			rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "good"})
			assert.Equal(t, tt.wantStatus, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.wantCode, code)
		})
	}
}

func TestPatchMe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		body      string
		wantName  *string
		wantColor *string
	}{
		{"both", `{"display_name":"Alicia","avatar_color":"#123456"}`, strPtr("Alicia"), strPtr("#123456")},
		{"name only", `{"display_name":"Alicia"}`, strPtr("Alicia"), nil},
		{"color only", `{"avatar_color":"#123456"}`, nil, strPtr("#123456")},
		{"empty object", `{}`, nil, nil},
		{"explicit nulls", `{"display_name":null,"avatar_color":null}`, nil, nil},
		{"unknown fields ignored", `{"email":"evil@example.com","id":"x"}`, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			var got domain.ProfileUpdate
			e.users.update = func(_ context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error) {
				assert.Equal(t, testUserID, id)
				got = upd
				return sampleUser(), nil
			}
			rec := e.do(t, request{method: http.MethodPatch, path: "/api/me", body: tt.body, token: "good"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, tt.wantName, got.DisplayName)
			assert.Equal(t, tt.wantColor, got.AvatarColor)
			assert.Equal(t, "alice@example.com", rec.json(t)["email"])
		})
	}
}

func TestPatchMeErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"validation", domain.NewValidationError("avatar_color", "bad"), http.StatusBadRequest, httpapi.CodeValidation},
		{"user deleted", domain.ErrNotFound, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"unexpected", errors.New("db down"), http.StatusInternalServerError, httpapi.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.users.update = func(context.Context, uuid.UUID, domain.ProfileUpdate) (domain.User, error) {
				return domain.User{}, tt.err
			}
			rec := e.do(t, request{method: http.MethodPatch, path: "/api/me", body: `{"display_name":"x"}`, token: "good"})
			assert.Equal(t, tt.wantStatus, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.wantCode, code)
		})
	}
}

func TestListBoards(t *testing.T) {
	t.Parallel()
	t.Run("empty list is an empty array", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.boards.list = func(context.Context, uuid.UUID) ([]domain.BoardWithRole, error) { return nil, nil }
		rec := e.do(t, request{method: http.MethodGet, path: "/api/boards", token: "good"})
		require.Equal(t, http.StatusOK, rec.Code)
		assert.JSONEq(t, `{"boards":[]}`, rec.Body.String())
	})

	t.Run("boards with roles", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.boards.list = func(_ context.Context, id uuid.UUID) ([]domain.BoardWithRole, error) {
			assert.Equal(t, testUserID, id)
			return []domain.BoardWithRole{sampleBoard("Mine", domain.RoleOwner), sampleBoard("Shared", domain.RoleViewer)}, nil
		}
		rec := e.do(t, request{method: http.MethodGet, path: "/api/boards", token: "good"})
		require.Equal(t, http.StatusOK, rec.Code)
		boards, ok := rec.json(t)["boards"].([]any)
		require.True(t, ok)
		require.Len(t, boards, 2)
		first := boards[0].(map[string]any)
		assert.Equal(t, sampleBoardID, first["id"])
		assert.Equal(t, "Mine", first["title"])
		assert.Equal(t, testUserID.String(), first["owner_id"])
		assert.Equal(t, float64(3), first["seq"])
		assert.Equal(t, "owner", first["role"])
		assert.Equal(t, "2030-01-02T03:04:05Z", first["created_at"])
		assert.Equal(t, "2030-01-02T03:05:05Z", first["updated_at"])
		assert.NotContains(t, first, "deleted_at")
		assert.Equal(t, "viewer", boards[1].(map[string]any)["role"])
	})

	t.Run("unexpected error", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		e.boards.list = func(context.Context, uuid.UUID) ([]domain.BoardWithRole, error) { return nil, errors.New("db") }
		rec := e.do(t, request{method: http.MethodGet, path: "/api/boards", token: "good"})
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
		code, _ := rec.errorBody(t)
		assert.Equal(t, httpapi.CodeInternal, code)
	})
}

func TestCreateBoard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.boards.create = func(_ context.Context, id uuid.UUID, title string) (domain.BoardWithRole, error) {
		assert.Equal(t, testUserID, id)
		return sampleBoard(title, domain.RoleOwner), nil
	}
	rec := e.do(t, request{method: http.MethodPost, path: "/api/boards", body: `{"title":"Roadmap"}`, token: "good"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	body := rec.json(t)
	assert.Equal(t, "Roadmap", body["title"])
	assert.Equal(t, "owner", body["role"])
}

func TestCreateBoardErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"validation", domain.NewValidationError("title", "is required"), http.StatusBadRequest, httpapi.CodeValidation},
		{"unexpected", errors.New("db"), http.StatusInternalServerError, httpapi.CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.boards.create = func(context.Context, uuid.UUID, string) (domain.BoardWithRole, error) {
				return domain.BoardWithRole{}, tt.err
			}
			rec := e.do(t, request{method: http.MethodPost, path: "/api/boards", body: `{"title":""}`, token: "good"})
			assert.Equal(t, tt.wantStatus, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.wantCode, code)
		})
	}
}

func TestRenameBoard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.boards.rename = func(_ context.Context, userID, boardID uuid.UUID, title string) (domain.BoardWithRole, domain.Event, error) {
		assert.Equal(t, testUserID, userID)
		assert.Equal(t, sampleBoardID, boardID.String())
		return sampleBoard(title, domain.RoleOwner), domain.Event{Seq: 4}, nil
	}
	rec := e.do(t, request{method: http.MethodPatch, path: "/api/boards/" + sampleBoardID, body: `{"title":"New"}`, token: "good"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "New", rec.json(t)["title"])
}

func TestDeleteBoard(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.boards.delete = func(_ context.Context, userID, boardID uuid.UUID) (domain.Event, error) {
		assert.Equal(t, testUserID, userID)
		assert.Equal(t, sampleBoardID, boardID.String())
		return domain.Event{Seq: 5}, nil
	}
	rec := e.do(t, request{method: http.MethodDelete, path: "/api/boards/" + sampleBoardID, token: "good"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
}

func TestBoardMutationErrors(t *testing.T) {
	t.Parallel()
	errs := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"not found", domain.ErrNotFound, http.StatusNotFound, httpapi.CodeNotFound},
		{"forbidden", domain.ErrForbidden, http.StatusForbidden, httpapi.CodeForbidden},
		{"validation", domain.NewValidationError("title", "is required"), http.StatusBadRequest, httpapi.CodeValidation},
		{"unexpected", errors.New("db"), http.StatusInternalServerError, httpapi.CodeInternal},
	}
	for _, tt := range errs {
		t.Run("rename "+tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.boards.rename = func(context.Context, uuid.UUID, uuid.UUID, string) (domain.BoardWithRole, domain.Event, error) {
				return domain.BoardWithRole{}, domain.Event{}, tt.err
			}
			rec := e.do(t, request{method: http.MethodPatch, path: "/api/boards/" + sampleBoardID, body: `{"title":"x"}`, token: "good"})
			assert.Equal(t, tt.wantStatus, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.wantCode, code)
		})
		t.Run("delete "+tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.boards.delete = func(context.Context, uuid.UUID, uuid.UUID) (domain.Event, error) {
				return domain.Event{}, tt.err
			}
			rec := e.do(t, request{method: http.MethodDelete, path: "/api/boards/" + sampleBoardID, token: "good"})
			assert.Equal(t, tt.wantStatus, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.wantCode, code)
		})
	}
}

func TestBoardIDMustBeUUID(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"abc", "123", "not-a-uuid", "22222222-2222-7222-8222-22222222222", "%20"} {
		for _, method := range []string{http.MethodPatch, http.MethodDelete} {
			t.Run(method+" "+id, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t)
				body := ""
				if method == http.MethodPatch {
					body = `{"title":"x"}`
				}
				rec := e.do(t, request{method: method, path: "/api/boards/" + id, body: body, token: "good"})
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				code, msg := rec.errorBody(t)
				assert.Equal(t, httpapi.CodeValidation, code)
				assert.Contains(t, msg, "id")
			})
		}
	}
}
