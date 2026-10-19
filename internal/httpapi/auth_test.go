package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

func TestRegister(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var got auth.RegisterInput
	e.auth.register = func(_ context.Context, in auth.RegisterInput) (auth.Session, error) {
		got = in
		return auth.Session{User: sampleUser(), Tokens: samplePair()}, nil
	}

	rec := e.do(t, request{
		method: http.MethodPost, path: "/api/auth/register",
		body: `{"email":"alice@example.com","password":"s3cret-pass","display_name":"Alice","avatar_color":"#ABCDEF"}`,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, auth.RegisterInput{Email: "alice@example.com", Password: "s3cret-pass", DisplayName: "Alice", AvatarColor: "#ABCDEF"}, got)

	body := rec.json(t)
	assert.Equal(t, "Bearer", body["token_type"])
	assert.Equal(t, "access-1", body["access_token"])
	assert.Equal(t, "refresh-1", body["refresh_token"])
	assert.NotEmpty(t, body["access_expires_at"])
	assert.NotEmpty(t, body["refresh_expires_at"])
	expiresIn, ok := body["expires_in"].(float64)
	require.True(t, ok)
	assert.InDelta(t, 900, expiresIn, 5)

	user, ok := body["user"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, testUserID.String(), user["id"])
	assert.Equal(t, "alice@example.com", user["email"])
	assert.Equal(t, "Alice", user["display_name"])
	assert.Equal(t, "#ABCDEF", user["avatar_color"])
	assert.Equal(t, "2030-01-02T03:04:05Z", user["created_at"])
	assert.NotContains(t, user, "password_hash")
	assert.NotContains(t, rec.Body.String(), "password")
}

func TestAuthEndpointsErrorMapping(t *testing.T) {
	t.Parallel()
	validation := domain.NewValidationError("email", "is invalid")
	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantCode    string
		wantMessage string
		retryAfter  bool
	}{
		{"validation", validation, http.StatusBadRequest, httpapi.CodeValidation, "email: is invalid", false},
		{"wrapped validation", errors.Join(errors.New("ctx"), validation), http.StatusBadRequest, httpapi.CodeValidation, "", false},
		{"email taken", domain.ErrEmailTaken, http.StatusConflict, httpapi.CodeEmailTaken, "", false},
		{"invalid credentials", domain.ErrInvalidCredential, http.StatusUnauthorized, httpapi.CodeLoginFailed, "", false},
		{"rate limited", domain.ErrRateLimited, http.StatusTooManyRequests, httpapi.CodeRateLimited, "", true},
		{"token invalid", domain.ErrTokenInvalid, http.StatusUnauthorized, httpapi.CodeInvalidToken, "", false},
		{"token expired", domain.ErrTokenExpired, http.StatusUnauthorized, httpapi.CodeTokenExpired, "", false},
		{"token reused", domain.ErrTokenReused, http.StatusUnauthorized, httpapi.CodeTokenReused, "", false},
		{"not found", domain.ErrNotFound, http.StatusNotFound, httpapi.CodeNotFound, "", false},
		{"forbidden", domain.ErrForbidden, http.StatusForbidden, httpapi.CodeForbidden, "", false},
		{"unexpected", errors.New("db is down"), http.StatusInternalServerError, httpapi.CodeInternal, "internal server error", false},
	}

	endpoints := []struct {
		name  string
		path  string
		body  string
		token string
		wire  func(*env, error)
	}{
		{
			name: "register", path: "/api/auth/register", body: `{"email":"a@b.co","password":"password1","display_name":"A"}`,
			wire: func(e *env, err error) {
				e.auth.register = func(context.Context, auth.RegisterInput) (auth.Session, error) { return auth.Session{}, err }
			},
		},
		{
			name: "login", path: "/api/auth/login", body: `{"email":"a@b.co","password":"password1"}`,
			wire: func(e *env, err error) {
				e.auth.login = func(context.Context, auth.LoginInput) (auth.Session, error) { return auth.Session{}, err }
			},
		},
		{
			name: "refresh", path: "/api/auth/refresh", body: `{"refresh_token":"r"}`,
			wire: func(e *env, err error) {
				e.auth.refresh = func(context.Context, string) (auth.TokenPair, error) { return auth.TokenPair{}, err }
			},
		},
		{
			name: "logout", path: "/api/auth/logout", body: `{"refresh_token":"r"}`, token: "good",
			wire: func(e *env, err error) {
				e.auth.logout = func(context.Context, uuid.UUID, string) error { return err }
			},
		},
	}

	for _, ep := range endpoints {
		for _, tt := range tests {
			t.Run(ep.name+"/"+tt.name, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t)
				ep.wire(e, tt.err)
				rec := e.do(t, request{method: http.MethodPost, path: ep.path, body: ep.body, token: ep.token})
				assert.Equal(t, tt.wantStatus, rec.Code)
				code, msg := rec.errorBody(t)
				assert.Equal(t, tt.wantCode, code)
				if tt.wantMessage != "" {
					assert.Equal(t, tt.wantMessage, msg)
				}
				if tt.retryAfter {
					assert.Equal(t, "60", rec.Header().Get("Retry-After"))
				} else {
					assert.Empty(t, rec.Header().Get("Retry-After"))
				}
				assert.NotContains(t, rec.Body.String(), "db is down")
			})
		}
	}
}

func TestLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var got auth.LoginInput
	e.auth.login = func(_ context.Context, in auth.LoginInput) (auth.Session, error) {
		got = in
		return auth.Session{User: sampleUser(), Tokens: samplePair()}, nil
	}
	rec := e.do(t, request{method: http.MethodPost, path: "/api/auth/login", body: `{"email":"alice@example.com","password":"s3cret-pass"}`})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, auth.LoginInput{Email: "alice@example.com", Password: "s3cret-pass", ClientIP: "192.0.2.10"}, got)

	body := rec.json(t)
	assert.Equal(t, "access-1", body["access_token"])
	assert.Equal(t, "refresh-1", body["refresh_token"])
	assert.Contains(t, body, "user")
}

func TestLoginIgnoresSpoofedForwardedForWithoutTrustedProxy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var got auth.LoginInput
	e.auth.login = func(_ context.Context, in auth.LoginInput) (auth.Session, error) {
		got = in
		return auth.Session{}, domain.ErrInvalidCredential
	}
	e.do(t, request{
		method: http.MethodPost, path: "/api/auth/login", body: `{"email":"a@b.co","password":"x"}`,
		headers: map[string]string{"X-Forwarded-For": "203.0.113.99", "X-Real-IP": "203.0.113.98"},
	})
	assert.Equal(t, "192.0.2.10", got.ClientIP)
}

func TestRefresh(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var got string
	e.auth.refresh = func(_ context.Context, token string) (auth.TokenPair, error) {
		got = token
		return samplePair(), nil
	}
	rec := e.do(t, request{method: http.MethodPost, path: "/api/auth/refresh", body: `{"refresh_token":"old-token"}`})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "old-token", got)

	body := rec.json(t)
	assert.Equal(t, "access-1", body["access_token"])
	assert.Equal(t, "refresh-1", body["refresh_token"])
	assert.NotContains(t, body, "user")
}

func TestLogout(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var (
		gotUser  uuid.UUID
		gotToken string
	)
	e.auth.logout = func(_ context.Context, userID uuid.UUID, token string) error {
		gotUser, gotToken = userID, token
		return nil
	}
	rec := e.do(t, request{method: http.MethodPost, path: "/api/auth/logout", body: `{"refresh_token":"r-1"}`, token: "good"})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, testUserID, gotUser)
	assert.Equal(t, "r-1", gotToken)
}

func TestRefreshTokenRequiredOnRefreshAndLogout(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/api/auth/refresh", "/api/auth/logout"} {
		for _, body := range []string{`{}`, `{"refresh_token":""}`, `{"refresh_token":null}`} {
			t.Run(path+" "+body, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t)
				rec := e.do(t, request{method: http.MethodPost, path: path, body: body, token: "good"})
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				code, msg := rec.errorBody(t)
				assert.Equal(t, httpapi.CodeValidation, code)
				assert.Contains(t, msg, "refresh_token")
			})
		}
	}
}

func TestMalformedJSONBodies(t *testing.T) {
	t.Parallel()
	endpoints := []struct {
		method string
		path   string
		token  string
	}{
		{http.MethodPost, "/api/auth/register", ""},
		{http.MethodPost, "/api/auth/login", ""},
		{http.MethodPost, "/api/auth/refresh", ""},
		{http.MethodPost, "/api/auth/logout", "good"},
		{http.MethodPatch, "/api/me", "good"},
		{http.MethodPost, "/api/boards", "good"},
		{http.MethodPatch, "/api/boards/22222222-2222-7222-8222-222222222222", "good"},
	}
	bodies := map[string]string{
		"empty body":        "",
		"not json":          "hello",
		"truncated":         `{"email":`,
		"array":             `[]`,
		"wrong field type":  `{"email":123,"title":123,"display_name":123,"refresh_token":123}`,
		"trailing garbage":  `{} x`,
		"two documents":     `{}{}`,
		"trailing document": `{"title":"a"} {"title":"b"}`,
	}
	for _, ep := range endpoints {
		for name, body := range bodies {
			t.Run(ep.method+" "+ep.path+" "+name, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t)
				rec := e.do(t, request{method: ep.method, path: ep.path, body: body, token: ep.token})
				assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
				code, _ := rec.errorBody(t)
				assert.Equal(t, httpapi.CodeBadRequest, code)
			})
		}
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	t.Parallel()
	endpoints := []struct {
		method string
		path   string
		token  string
	}{
		{http.MethodPost, "/api/auth/register", ""},
		{http.MethodPost, "/api/auth/login", ""},
		{http.MethodPost, "/api/auth/refresh", ""},
		{http.MethodPost, "/api/auth/logout", "good"},
		{http.MethodPatch, "/api/me", "good"},
		{http.MethodPost, "/api/boards", "good"},
		{http.MethodPatch, "/api/boards/22222222-2222-7222-8222-222222222222", "good"},
	}
	for _, ep := range endpoints {
		t.Run(ep.method+" "+ep.path, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.maxBody = 64
			big := `{"title":"` + strings.Repeat("a", 200) + `"}`
			rec := e.do(t, request{method: ep.method, path: ep.path, body: big, token: ep.token})
			assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
			code, _ := rec.errorBody(t)
			assert.Equal(t, httpapi.CodePayloadTooLarge, code)
		})
	}
}

func TestBodyAtLimitIsAccepted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	body := `{"refresh_token":"abc"}`
	e.maxBody = int64(len(body))
	e.auth.refresh = func(context.Context, string) (auth.TokenPair, error) { return samplePair(), nil }
	rec := e.do(t, request{method: http.MethodPost, path: "/api/auth/refresh", body: body})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestProtectedEndpointsRequireBearer(t *testing.T) {
	t.Parallel()
	const boardID = "22222222-2222-7222-8222-222222222222"
	endpoints := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/auth/logout", `{"refresh_token":"r"}`},
		{http.MethodGet, "/api/me", ""},
		{http.MethodPatch, "/api/me", `{}`},
		{http.MethodGet, "/api/boards", ""},
		{http.MethodPost, "/api/boards", `{"title":"t"}`},
		{http.MethodPatch, "/api/boards/" + boardID, `{"title":"t"}`},
		{http.MethodDelete, "/api/boards/" + boardID, ""},
	}
	headers := []struct {
		name  string
		value string
	}{
		{"missing", ""},
		{"basic scheme", "Basic dXNlcjpwYXNz"},
		{"scheme only", "Bearer"},
		{"scheme with blank token", "Bearer    "},
		{"raw token without scheme", "good"},
		{"bearer prefix glued", "Bearergood"},
	}
	for _, ep := range endpoints {
		for _, h := range headers {
			t.Run(ep.method+" "+ep.path+" "+h.name, func(t *testing.T) {
				t.Parallel()
				e := newEnv(t)
				hdr := map[string]string{}
				if h.value != "" {
					hdr["Authorization"] = h.value
				}
				rec := e.do(t, request{method: ep.method, path: ep.path, body: ep.body, headers: hdr})
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
				code, _ := rec.errorBody(t)
				assert.Equal(t, httpapi.CodeUnauthorized, code)
				assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
			})
		}
	}
}

func TestAuthMiddlewareTokenErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		verifier error
		wantCode string
	}{
		{"expired", domain.ErrTokenExpired, httpapi.CodeTokenExpired},
		{"invalid", domain.ErrTokenInvalid, httpapi.CodeInvalidToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			e.tokens.verify = func(string) (uuid.UUID, error) { return uuid.Nil, tt.verifier }
			rec := e.do(t, request{method: http.MethodGet, path: "/api/me", token: "whatever"})
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			code, _ := rec.errorBody(t)
			assert.Equal(t, tt.wantCode, code)
			assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "invalid_token")
		})
	}
}

func TestAuthMiddlewareAcceptsCaseInsensitiveScheme(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.users.get = func(_ context.Context, id uuid.UUID) (domain.User, error) {
		assert.Equal(t, testUserID, id)
		return sampleUser(), nil
	}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		rec := e.do(t, request{method: http.MethodGet, path: "/api/me", headers: map[string]string{"Authorization": scheme + " good"}})
		assert.Equal(t, http.StatusOK, rec.Code, scheme)
	}
}

func TestPublicEndpointsDoNotRequireBearer(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.auth.refresh = func(context.Context, string) (auth.TokenPair, error) { return samplePair(), nil }
	rec := e.do(t, request{method: http.MethodPost, path: "/api/auth/refresh", body: `{"refresh_token":"x"}`})
	assert.Equal(t, http.StatusOK, rec.Code)
}
