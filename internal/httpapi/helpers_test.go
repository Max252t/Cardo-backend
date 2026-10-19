package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

func init() {
	gin.SetMode(gin.TestMode)
}

var testUserID = uuid.MustParse("11111111-1111-7111-8111-111111111111")

type fakeAuth struct {
	register func(ctx context.Context, in auth.RegisterInput) (auth.Session, error)
	login    func(ctx context.Context, in auth.LoginInput) (auth.Session, error)
	refresh  func(ctx context.Context, token string) (auth.TokenPair, error)
	logout   func(ctx context.Context, userID uuid.UUID, token string) error
}

func (f *fakeAuth) Register(ctx context.Context, in auth.RegisterInput) (auth.Session, error) {
	return f.register(ctx, in)
}

func (f *fakeAuth) Login(ctx context.Context, in auth.LoginInput) (auth.Session, error) {
	return f.login(ctx, in)
}

func (f *fakeAuth) Refresh(ctx context.Context, token string) (auth.TokenPair, error) {
	return f.refresh(ctx, token)
}

func (f *fakeAuth) Logout(ctx context.Context, userID uuid.UUID, token string) error {
	return f.logout(ctx, userID, token)
}

type fakeTokens struct {
	verify func(token string) (uuid.UUID, error)
}

func (f *fakeTokens) Verify(token string) (uuid.UUID, error) {
	return f.verify(token)
}

type fakeUsers struct {
	get    func(ctx context.Context, id uuid.UUID) (domain.User, error)
	update func(ctx context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error)
}

func (f *fakeUsers) Get(ctx context.Context, id uuid.UUID) (domain.User, error) {
	return f.get(ctx, id)
}

func (f *fakeUsers) UpdateProfile(ctx context.Context, id uuid.UUID, upd domain.ProfileUpdate) (domain.User, error) {
	return f.update(ctx, id, upd)
}

type fakeBoards struct {
	list   func(ctx context.Context, userID uuid.UUID) ([]domain.BoardWithRole, error)
	create func(ctx context.Context, userID uuid.UUID, title string) (domain.BoardWithRole, error)
	rename func(ctx context.Context, userID, boardID uuid.UUID, title string) (domain.BoardWithRole, domain.Event, error)
	delete func(ctx context.Context, userID, boardID uuid.UUID) (domain.Event, error)
}

func (f *fakeBoards) List(ctx context.Context, userID uuid.UUID) ([]domain.BoardWithRole, error) {
	return f.list(ctx, userID)
}

func (f *fakeBoards) Create(ctx context.Context, userID uuid.UUID, title string) (domain.BoardWithRole, error) {
	return f.create(ctx, userID, title)
}

func (f *fakeBoards) Rename(ctx context.Context, userID, boardID uuid.UUID, title string) (domain.BoardWithRole, domain.Event, error) {
	return f.rename(ctx, userID, boardID, title)
}

func (f *fakeBoards) Delete(ctx context.Context, userID, boardID uuid.UUID) (domain.Event, error) {
	return f.delete(ctx, userID, boardID)
}

type env struct {
	auth      *fakeAuth
	tokens    *fakeTokens
	users     *fakeUsers
	boards    *fakeBoards
	readiness map[string]httpapi.Checker
	maxBody   int64
	router    *gin.Engine
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		auth: &fakeAuth{},
		tokens: &fakeTokens{verify: func(token string) (uuid.UUID, error) {
			if token == "good" {
				return testUserID, nil
			}
			return uuid.Nil, domain.ErrTokenInvalid
		}},
		users:  &fakeUsers{},
		boards: &fakeBoards{},
	}
	return e
}

func (e *env) build(t *testing.T) *gin.Engine {
	t.Helper()
	r, err := httpapi.NewRouter(httpapi.Deps{
		Auth:         e.auth,
		Tokens:       e.tokens,
		Users:        e.users,
		Boards:       e.boards,
		Readiness:    e.readiness,
		MaxBodyBytes: e.maxBody,
	})
	require.NoError(t, err)
	e.router = r
	return r
}

type request struct {
	method  string
	path    string
	body    string
	token   string
	headers map[string]string
}

type response struct {
	*httptest.ResponseRecorder
}

func (e *env) do(t *testing.T, rq request) response {
	t.Helper()
	if e.router == nil {
		e.build(t)
	}
	var body io.Reader
	if rq.body != "" {
		body = strings.NewReader(rq.body)
	}
	req := httptest.NewRequestWithContext(t.Context(), rq.method, rq.path, body)
	req.RemoteAddr = "192.0.2.10:4321"
	if rq.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if rq.token != "" {
		req.Header.Set("Authorization", "Bearer "+rq.token)
	}
	for k, v := range rq.headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return response{rec}
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.NewDecoder(bytes.NewReader(r.Body.Bytes())).Decode(&out), "body: %s", r.Body.String())
	return out
}

func (r response) errorBody(t *testing.T) (code, message string) {
	t.Helper()
	require.Contains(t, r.Header().Get("Content-Type"), "application/json")
	raw := r.json(t)
	errObj, ok := raw["error"].(map[string]any)
	require.True(t, ok, "response must contain an error object: %s", r.Body.String())
	code, _ = errObj["code"].(string)
	message, _ = errObj["message"].(string)
	require.NotEmpty(t, code)
	require.NotEmpty(t, message)
	return code, message
}

func sampleUser() domain.User {
	return domain.User{
		ID:          testUserID,
		Email:       "alice@example.com",
		DisplayName: "Alice",
		AvatarColor: "#ABCDEF",
		CreatedAt:   time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func sampleBoard(title string, role domain.Role) domain.BoardWithRole {
	return domain.BoardWithRole{
		Board: domain.Board{
			ID:        uuid.MustParse("22222222-2222-7222-8222-222222222222"),
			Title:     title,
			OwnerID:   testUserID,
			Seq:       3,
			CreatedAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
			UpdatedAt: time.Date(2030, 1, 2, 3, 5, 5, 0, time.UTC),
		},
		Role: role,
	}
}

func samplePair() auth.TokenPair {
	return auth.TokenPair{
		AccessToken:      "access-1",
		AccessExpiresAt:  time.Now().Add(15 * time.Minute),
		RefreshToken:     "refresh-1",
		RefreshExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	}
}
