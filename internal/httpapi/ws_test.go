package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

type fakeClaims struct {
	parse func(token string) (domain.AccessClaims, error)
}

func (f fakeClaims) Parse(token string) (domain.AccessClaims, error) {
	return f.parse(token)
}

type fakeSockets struct {
	authorize func(ctx context.Context, userID, boardID uuid.UUID) (domain.Member, error)
	serve     func(w http.ResponseWriter, r *http.Request, boardID uuid.UUID, member domain.Member, expiresAt time.Time) error
	served    int
	authed    int
}

func (f *fakeSockets) Authorize(ctx context.Context, userID, boardID uuid.UUID) (domain.Member, error) {
	f.authed++
	return f.authorize(ctx, userID, boardID)
}

func (f *fakeSockets) Serve(w http.ResponseWriter, r *http.Request, boardID uuid.UUID, member domain.Member, expiresAt time.Time) error {
	f.served++
	return f.serve(w, r, boardID, member, expiresAt)
}

type wsEnv struct {
	router   *gin.Engine
	sockets  *fakeSockets
	shutting bool
	expires  time.Time
}

func newWSEnv(t *testing.T) *wsEnv {
	t.Helper()
	e := newEnv(t)
	w := &wsEnv{
		sockets: &fakeSockets{},
		expires: time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	w.sockets.authorize = func(_ context.Context, userID, boardID uuid.UUID) (domain.Member, error) {
		return domain.Member{UserID: userID, DisplayName: "Ann", Role: domain.RoleEditor}, nil
	}
	w.sockets.serve = func(rw http.ResponseWriter, _ *http.Request, _ uuid.UUID, _ domain.Member, _ time.Time) error {
		rw.WriteHeader(http.StatusAccepted)
		return nil
	}
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:   e.auth,
		Tokens: e.tokens,
		Users:  e.users,
		Boards: e.boards,
		Claims: fakeClaims{parse: func(token string) (domain.AccessClaims, error) {
			switch token {
			case "good":
				return domain.AccessClaims{UserID: testUserID, ExpiresAt: w.expires}, nil
			case "expired":
				return domain.AccessClaims{}, domain.ErrTokenExpired
			}
			return domain.AccessClaims{}, domain.ErrTokenInvalid
		}},
		Sockets:      w.sockets,
		ShuttingDown: func() bool { return w.shutting },
	})
	require.NoError(t, err)
	w.router = router
	return w
}

func (w *wsEnv) get(t *testing.T, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, req)
	return rec
}

func wsPath() string {
	return "/ws/boards/" + sampleBoardID
}

func TestSocketEndpointRequiresConfiguredDependencies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r, err := httpapi.NewRouter(httpapi.Deps{Auth: e.auth, Tokens: e.tokens, Users: e.users, Boards: e.boards})
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, wsPath(), nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSocketEndpointRejectsWhileShuttingDownBeforeAnyOtherCheck(t *testing.T) {
	t.Parallel()
	w := newWSEnv(t)
	w.shutting = true
	rec := w.get(t, wsPath(), map[string]string{"Authorization": "Bearer good"})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Equal(t, 0, w.sockets.authed)
	assert.Equal(t, 0, w.sockets.served)

	noAuth := w.get(t, wsPath(), nil)
	assert.Equal(t, http.StatusServiceUnavailable, noAuth.Code)
}

func TestSocketEndpointAuthenticationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		headers map[string]string
		status  int
		code    string
	}{
		{"missing header", nil, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"basic scheme", map[string]string{"Authorization": "Basic abc"}, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"blank bearer", map[string]string{"Authorization": "Bearer   "}, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"invalid token", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized, httpapi.CodeInvalidToken},
		{"expired token", map[string]string{"Authorization": "Bearer expired"}, http.StatusUnauthorized, httpapi.CodeTokenExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWSEnv(t)
			rec := w.get(t, wsPath(), tt.headers)
			assert.Equal(t, tt.status, rec.Code)
			code, _ := response{rec}.errorBody(t)
			assert.Equal(t, tt.code, code)
			assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer")
			assert.Equal(t, 0, w.sockets.authed, "membership is never looked up for an unauthenticated request")
			assert.Equal(t, 0, w.sockets.served)
		})
	}
}

func TestSocketEndpointRejectsAnInvalidBoardIDBeforeLookingUpMembership(t *testing.T) {
	t.Parallel()
	w := newWSEnv(t)
	rec := w.get(t, "/ws/boards/not-a-uuid", map[string]string{"Authorization": "Bearer good"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	code, _ := response{rec}.errorBody(t)
	assert.Equal(t, httpapi.CodeValidation, code)
	assert.Equal(t, 0, w.sockets.authed)
}

func TestSocketEndpointAuthorizationErrors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		status     int
		code       string
		retryAfter bool
	}{
		{"not a member or no such board", domain.ErrNotFound, http.StatusNotFound, httpapi.CodeNotFound, false},
		{"forbidden", domain.ErrForbidden, http.StatusForbidden, httpapi.CodeForbidden, false},
		{"database busy", domain.ErrBusy, http.StatusServiceUnavailable, httpapi.CodeBusy, true},
		{"overloaded", domain.ErrOverloaded, http.StatusServiceUnavailable, httpapi.CodeOverloaded, true},
		{"unexpected", errors.New("db is down"), http.StatusInternalServerError, httpapi.CodeInternal, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := newWSEnv(t)
			w.sockets.authorize = func(context.Context, uuid.UUID, uuid.UUID) (domain.Member, error) {
				return domain.Member{}, tt.err
			}
			rec := w.get(t, wsPath(), map[string]string{"Authorization": "Bearer good"})
			assert.Equal(t, tt.status, rec.Code)
			code, _ := response{rec}.errorBody(t)
			assert.Equal(t, tt.code, code)
			assert.Equal(t, tt.retryAfter, rec.Header().Get("Retry-After") != "")
			assert.NotContains(t, rec.Body.String(), "db is down")
			assert.Equal(t, 0, w.sockets.served)
		})
	}
}

func TestSocketEndpointPassesTheAuthenticatedIdentityToServe(t *testing.T) {
	t.Parallel()
	w := newWSEnv(t)
	var (
		gotBoard   uuid.UUID
		gotMember  domain.Member
		gotExpires time.Time
	)
	w.sockets.authorize = func(_ context.Context, userID, boardID uuid.UUID) (domain.Member, error) {
		assert.Equal(t, testUserID, userID)
		assert.Equal(t, sampleBoardID, boardID.String())
		return domain.Member{UserID: userID, DisplayName: "Ann", Role: domain.RoleViewer}, nil
	}
	w.sockets.serve = func(rw http.ResponseWriter, _ *http.Request, boardID uuid.UUID, member domain.Member, expiresAt time.Time) error {
		gotBoard, gotMember, gotExpires = boardID, member, expiresAt
		rw.WriteHeader(http.StatusSwitchingProtocols)
		return nil
	}
	rec := w.get(t, wsPath(), map[string]string{"Authorization": "bearer good"})
	assert.Equal(t, http.StatusSwitchingProtocols, rec.Code)
	assert.Equal(t, sampleBoardID, gotBoard.String())
	assert.Equal(t, testUserID, gotMember.UserID)
	assert.Equal(t, domain.RoleViewer, gotMember.Role)
	assert.Equal(t, w.expires, gotExpires, "the connection lifetime is bound to the access token expiry")
}

func TestSocketEndpointServeFailureIsNotAnAPIError(t *testing.T) {
	t.Parallel()
	w := newWSEnv(t)
	w.sockets.serve = func(rw http.ResponseWriter, _ *http.Request, _ uuid.UUID, _ domain.Member, _ time.Time) error {
		rw.WriteHeader(http.StatusBadRequest)
		return errors.New("upgrade websocket: not a websocket handshake")
	}
	rec := w.get(t, wsPath(), map[string]string{"Authorization": "Bearer good"})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.NotContains(t, rec.Body.String(), "upgrade websocket")
}

func TestSocketEndpointOnlyAcceptsGET(t *testing.T) {
	t.Parallel()
	w := newWSEnv(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequestWithContext(t.Context(), method, wsPath(), nil)
		req.Header.Set("Authorization", "Bearer good")
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
	}
	assert.Equal(t, 0, w.sockets.authed)
}
