package httpapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/argon2id"
	"github.com/Max252t/Cardo-backend/internal/auth"
	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/jwtauth"
	"github.com/Max252t/Cardo-backend/internal/members"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/ratelimit"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
	"github.com/Max252t/Cardo-backend/internal/users"
)

type flowClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *flowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *flowClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

const (
	flowAccessTTL  = 15 * time.Minute
	flowRefreshTTL = 7 * 24 * time.Hour
)

type flow struct {
	router *gin.Engine
	pool   *pgxpool.Pool
	store  *storage.Store
	clock  *flowClock
	logs   *syncBuffer
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	return buildFlow(t, true)
}

func newBoardFlow(t *testing.T) *flow {
	t.Helper()
	return buildFlow(t, false)
}

func buildFlow(t *testing.T, withRedis bool) *flow {
	t.Helper()
	pool := testutil.NewMigratedPool(t)
	store := storage.New(pool)
	clock := &flowClock{now: time.Now()}
	logs := &syncBuffer{}
	var limiter auth.LoginLimiter = ratelimit.NewMemoryLimiter(5, time.Minute, clock)
	readiness := map[string]httpapi.Checker{"postgres": store}
	if withRedis {
		redisClient := testutil.NewRedis(t)
		limiter = ratelimit.NewRedisLimiter(redisClient, 5, time.Minute)
		readiness["redis"] = httpapi.CheckerFunc(func(ctx context.Context) error { return redisClient.Ping(ctx).Err() })
	}

	jwtm := jwtauth.New(jwtauth.Config{
		Secret:   []byte("0123456789abcdef0123456789abcdef"),
		Issuer:   "cardo",
		Audience: "cardo-api",
		TTL:      flowAccessTTL,
	}, clock)
	authSvc := auth.NewService(auth.Deps{
		Store:      store,
		Hasher:     argon2id.New(argon2id.Params{Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 8, KeyLength: 16}),
		Access:     jwtm,
		Limiter:    limiter,
		Clock:      clock,
		RefreshTTL: flowRefreshTTL,
	})
	router, err := httpapi.NewRouter(httpapi.Deps{
		Auth:      authSvc,
		Tokens:    jwtm,
		Users:     users.NewService(store),
		Boards:    boards.NewService(store),
		Members:   members.NewService(store),
		Logger:    slog.New(slog.NewJSONHandler(logs, nil)),
		Readiness: readiness,
	})
	require.NoError(t, err)
	return &flow{router: router, pool: pool, store: store, clock: clock, logs: logs}
}

func (f *flow) do(t *testing.T, method, path, body, token string) response {
	t.Helper()
	reader := strings.NewReader(body)
	req := httptest.NewRequestWithContext(t.Context(), method, path, reader)
	req.RemoteAddr = "192.0.2.77:5555"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return response{rec}
}

func (f *flow) mustJSON(t *testing.T, method, path, body, token string, wantStatus int) map[string]any {
	t.Helper()
	rec := f.do(t, method, path, body, token)
	require.Equal(t, wantStatus, rec.Code, "%s %s: %s", method, path, rec.Body.String())
	if wantStatus == http.StatusNoContent {
		return nil
	}
	return rec.json(t)
}

func (f *flow) expectError(t *testing.T, method, path, body, token string, wantStatus int, wantCode string) response {
	t.Helper()
	rec := f.do(t, method, path, body, token)
	require.Equal(t, wantStatus, rec.Code, "%s %s: %s", method, path, rec.Body.String())
	code, _ := rec.errorBody(t)
	require.Equal(t, wantCode, code)
	return rec
}

type session struct {
	userID  string
	access  string
	refresh string
	email   string
}

func jsonBody(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func (f *flow) register(t *testing.T, prefix string) session {
	t.Helper()
	email := testutil.UniqueEmail(prefix)
	body := f.mustJSON(t, http.MethodPost, "/api/auth/register",
		jsonBody(t, map[string]string{"email": email, "password": "correct-horse", "display_name": strings.ToUpper(prefix)}),
		"", http.StatusCreated)
	user := body["user"].(map[string]any)
	return session{
		userID:  user["id"].(string),
		access:  body["access_token"].(string),
		refresh: body["refresh_token"].(string),
		email:   email,
	}
}

func (f *flow) login(t *testing.T, email, password string) response {
	t.Helper()
	return f.do(t, http.MethodPost, "/api/auth/login", jsonBody(t, map[string]string{"email": email, "password": password}), "")
}

func TestFlowHealthAndReadiness(t *testing.T) {
	f := newFlow(t)
	rec := f.do(t, http.MethodGet, "/healthz", "", "")
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = f.do(t, http.MethodGet, "/readyz", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	checks := rec.json(t)["checks"].(map[string]any)
	assert.Equal(t, "ok", checks["postgres"])
	assert.Equal(t, "ok", checks["redis"])
}

func TestFlowReadinessReportsUnavailableDatabase(t *testing.T) {
	f := newFlow(t)
	f.pool.Close()
	rec := f.do(t, http.MethodGet, "/readyz", "", "")
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	code, msg := rec.errorBody(t)
	assert.Equal(t, httpapi.CodeNotReady, code)
	assert.Contains(t, msg, "postgres")
	checks := rec.json(t)["checks"].(map[string]any)
	assert.Equal(t, "unavailable", checks["postgres"])
	assert.Equal(t, "ok", checks["redis"])
}

func TestFlowRegisterLoginProfile(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")

	f.expectError(t, http.MethodPost, "/api/auth/register",
		jsonBody(t, map[string]string{"email": strings.ToUpper(alice.email), "password": "another-pass", "display_name": "Dup"}),
		"", http.StatusConflict, httpapi.CodeEmailTaken)

	f.expectError(t, http.MethodPost, "/api/auth/register",
		jsonBody(t, map[string]string{"email": "not-an-email", "password": "another-pass", "display_name": "X"}),
		"", http.StatusBadRequest, httpapi.CodeValidation)

	me := f.mustJSON(t, http.MethodGet, "/api/me", "", alice.access, http.StatusOK)
	assert.Equal(t, alice.userID, me["id"])
	assert.Equal(t, alice.email, me["email"])
	assert.Equal(t, "ALICE", me["display_name"])
	assert.Regexp(t, `^#[0-9A-F]{6}$`, me["avatar_color"])

	patched := f.mustJSON(t, http.MethodPatch, "/api/me", `{"display_name":"  Alicia ","avatar_color":"#abcdef"}`, alice.access, http.StatusOK)
	assert.Equal(t, "Alicia", patched["display_name"])
	assert.Equal(t, "#ABCDEF", patched["avatar_color"])

	f.expectError(t, http.MethodPatch, "/api/me", `{"avatar_color":"blue"}`, alice.access, http.StatusBadRequest, httpapi.CodeValidation)
	me = f.mustJSON(t, http.MethodGet, "/api/me", "", alice.access, http.StatusOK)
	assert.Equal(t, "Alicia", me["display_name"])

	rec := f.login(t, strings.ToUpper(alice.email), "correct-horse")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	loginBody := rec.json(t)
	assert.Equal(t, alice.userID, loginBody["user"].(map[string]any)["id"])
	assert.NotEqual(t, alice.refresh, loginBody["refresh_token"])

	f.expectError(t, http.MethodGet, "/api/me", "", "", http.StatusUnauthorized, httpapi.CodeUnauthorized)
	f.expectError(t, http.MethodGet, "/api/me", "", "garbage", http.StatusUnauthorized, httpapi.CodeInvalidToken)
}

func TestFlowLoginRateLimit(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")

	for i := 1; i <= 5; i++ {
		rec := f.login(t, alice.email, "wrong-password")
		require.Equal(t, http.StatusUnauthorized, rec.Code, "attempt %d", i)
		code, _ := rec.errorBody(t)
		require.Equal(t, httpapi.CodeLoginFailed, code)
	}
	rec := f.login(t, alice.email, "wrong-password")
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	retryAfter, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	require.NoError(t, err)
	assert.True(t, retryAfter >= 1 && retryAfter <= 60, "retry after %d", retryAfter)
	code, _ := rec.errorBody(t)
	assert.Equal(t, httpapi.CodeRateLimited, code)

	rec = f.login(t, alice.email, "correct-horse")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "a correct password must not bypass the limiter")

	rec = f.login(t, bob.email, "correct-horse")
	assert.Equal(t, http.StatusOK, rec.Code, "other accounts from the same ip are unaffected")

	for range 4 {
		require.Equal(t, http.StatusUnauthorized, f.login(t, bob.email, "wrong-password").Code)
	}
	require.Equal(t, http.StatusOK, f.login(t, bob.email, "correct-horse").Code)
	for i := 1; i <= 5; i++ {
		require.Equal(t, http.StatusUnauthorized, f.login(t, bob.email, "wrong-password").Code, "attempt %d after successful login reset", i)
	}
	assert.Equal(t, http.StatusTooManyRequests, f.login(t, bob.email, "wrong-password").Code)
}

func TestFlowRefreshRotationAndReuseDetection(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")
	otherSession := f.login(t, alice.email, "correct-horse").json(t)

	refreshBody := func(token string) string { return jsonBody(t, map[string]string{"refresh_token": token}) }

	rotated := f.mustJSON(t, http.MethodPost, "/api/auth/refresh", refreshBody(alice.refresh), "", http.StatusOK)
	newRefresh := rotated["refresh_token"].(string)
	assert.NotEqual(t, alice.refresh, newRefresh)
	assert.NotContains(t, rotated, "user")
	f.mustJSON(t, http.MethodGet, "/api/me", "", rotated["access_token"].(string), http.StatusOK)

	var total, revoked int
	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT count(*), count(revoked_at) FROM refresh_tokens
		WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`,
		auth.HashToken(alice.refresh)).Scan(&total, &revoked))
	assert.Equal(t, 2, total)
	assert.Equal(t, 1, revoked)

	f.expectError(t, http.MethodPost, "/api/auth/refresh", refreshBody(alice.refresh), "", http.StatusUnauthorized, httpapi.CodeTokenReused)

	require.NoError(t, f.pool.QueryRow(context.Background(), `
		SELECT count(*), count(revoked_at) FROM refresh_tokens
		WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE token_hash = $1)`,
		auth.HashToken(alice.refresh)).Scan(&total, &revoked))
	assert.Equal(t, 2, total)
	assert.Equal(t, 2, revoked, "the whole family must be revoked in the database after reuse")

	f.expectError(t, http.MethodPost, "/api/auth/refresh", refreshBody(newRefresh), "", http.StatusUnauthorized, httpapi.CodeTokenReused)

	f.mustJSON(t, http.MethodPost, "/api/auth/refresh", refreshBody(otherSession["refresh_token"].(string)), "", http.StatusOK)

	f.expectError(t, http.MethodPost, "/api/auth/refresh", refreshBody("totally-unknown"), "", http.StatusUnauthorized, httpapi.CodeInvalidToken)
}

func TestFlowExpiredTokens(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")
	f.mustJSON(t, http.MethodGet, "/api/me", "", alice.access, http.StatusOK)

	f.clock.Advance(flowAccessTTL + time.Minute)
	f.expectError(t, http.MethodGet, "/api/me", "", alice.access, http.StatusUnauthorized, httpapi.CodeTokenExpired)

	refreshed := f.mustJSON(t, http.MethodPost, "/api/auth/refresh", jsonBody(t, map[string]string{"refresh_token": alice.refresh}), "", http.StatusOK)
	f.mustJSON(t, http.MethodGet, "/api/me", "", refreshed["access_token"].(string), http.StatusOK)

	f.clock.Advance(flowRefreshTTL + time.Minute)
	f.expectError(t, http.MethodPost, "/api/auth/refresh",
		jsonBody(t, map[string]string{"refresh_token": refreshed["refresh_token"].(string)}),
		"", http.StatusUnauthorized, httpapi.CodeTokenExpired)
}

func TestFlowLogoutCannotRevokeForeignToken(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	body := jsonBody(t, map[string]string{"refresh_token": alice.refresh})

	f.expectError(t, http.MethodPost, "/api/auth/logout", body, "", http.StatusUnauthorized, httpapi.CodeUnauthorized)

	f.mustJSON(t, http.MethodPost, "/api/auth/logout", body, bob.access, http.StatusNoContent)
	f.mustJSON(t, http.MethodPost, "/api/auth/refresh", body, "", http.StatusOK)
}

func TestFlowLogoutRevokesRefreshToken(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")
	body := jsonBody(t, map[string]string{"refresh_token": alice.refresh})

	f.mustJSON(t, http.MethodPost, "/api/auth/logout", body, alice.access, http.StatusNoContent)
	f.mustJSON(t, http.MethodPost, "/api/auth/logout", body, alice.access, http.StatusNoContent)
	f.expectError(t, http.MethodPost, "/api/auth/refresh", body, "", http.StatusUnauthorized, httpapi.CodeTokenReused)
}

func TestFlowBoards(t *testing.T) {
	f := newFlow(t)
	ctx := context.Background()
	alice := f.register(t, "alice")
	bob := f.register(t, "bob")
	carol := f.register(t, "carol")

	list := f.mustJSON(t, http.MethodGet, "/api/boards", "", alice.access, http.StatusOK)
	assert.Empty(t, list["boards"].([]any))

	f.expectError(t, http.MethodPost, "/api/boards", `{"title":"  "}`, alice.access, http.StatusBadRequest, httpapi.CodeValidation)

	created := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"  Sprint 1 "}`, alice.access, http.StatusCreated)
	boardID := created["id"].(string)
	_, err := uuid.Parse(boardID)
	require.NoError(t, err)
	assert.Equal(t, "Sprint 1", created["title"])
	assert.Equal(t, "owner", created["role"])
	assert.Equal(t, alice.userID, created["owner_id"])
	assert.Equal(t, float64(0), created["seq"])

	_, err = f.pool.Exec(ctx, `INSERT INTO board_members (board_id, user_id, role) VALUES ($1, $2, 'editor'), ($1, $3, 'viewer')`,
		boardID, bob.userID, carol.userID)
	require.NoError(t, err)

	bobList := f.mustJSON(t, http.MethodGet, "/api/boards", "", bob.access, http.StatusOK)["boards"].([]any)
	require.Len(t, bobList, 1)
	assert.Equal(t, "editor", bobList[0].(map[string]any)["role"])
	carolList := f.mustJSON(t, http.MethodGet, "/api/boards", "", carol.access, http.StatusOK)["boards"].([]any)
	require.Len(t, carolList, 1)
	assert.Equal(t, "viewer", carolList[0].(map[string]any)["role"])

	path := "/api/boards/" + boardID
	f.expectError(t, http.MethodPatch, path, `{"title":"Hacked"}`, bob.access, http.StatusForbidden, httpapi.CodeForbidden)
	f.expectError(t, http.MethodPatch, path, `{"title":"Hacked"}`, carol.access, http.StatusForbidden, httpapi.CodeForbidden)
	f.expectError(t, http.MethodDelete, path, "", bob.access, http.StatusForbidden, httpapi.CodeForbidden)
	f.expectError(t, http.MethodDelete, path, "", carol.access, http.StatusForbidden, httpapi.CodeForbidden)

	dave := f.register(t, "dave")
	f.expectError(t, http.MethodPatch, path, `{"title":"Hacked"}`, dave.access, http.StatusNotFound, httpapi.CodeNotFound)
	f.expectError(t, http.MethodDelete, path, "", dave.access, http.StatusNotFound, httpapi.CodeNotFound)
	f.expectError(t, http.MethodPatch, "/api/boards/"+uuid.NewString(), `{"title":"x"}`, alice.access, http.StatusNotFound, httpapi.CodeNotFound)
	f.expectError(t, http.MethodPatch, "/api/boards/not-a-uuid", `{"title":"x"}`, alice.access, http.StatusBadRequest, httpapi.CodeValidation)

	f.expectError(t, http.MethodPatch, path, `{"title":""}`, alice.access, http.StatusBadRequest, httpapi.CodeValidation)

	renamed := f.mustJSON(t, http.MethodPatch, path, `{"title":" Sprint 2 "}`, alice.access, http.StatusOK)
	assert.Equal(t, "Sprint 2", renamed["title"])
	assert.Equal(t, float64(1), renamed["seq"])
	assert.Equal(t, "owner", renamed["role"])

	aliceList := f.mustJSON(t, http.MethodGet, "/api/boards", "", alice.access, http.StatusOK)["boards"].([]any)
	require.Len(t, aliceList, 1)
	assert.Equal(t, "Sprint 2", aliceList[0].(map[string]any)["title"])

	var title string
	var seq int64
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT title, seq FROM boards WHERE id = $1`, boardID).Scan(&title, &seq))
	assert.Equal(t, "Sprint 2", title)
	assert.Equal(t, int64(1), seq)

	f.mustJSON(t, http.MethodDelete, path, "", alice.access, http.StatusNoContent)

	var deleted bool
	var events int
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT deleted_at IS NOT NULL, seq FROM boards WHERE id = $1`, boardID).Scan(&deleted, &seq))
	require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM board_events WHERE board_id = $1`, boardID).Scan(&events))
	assert.True(t, deleted)
	assert.Equal(t, int64(2), seq)
	assert.Equal(t, 2, events)

	for _, s := range []session{alice, bob, carol} {
		l := f.mustJSON(t, http.MethodGet, "/api/boards", "", s.access, http.StatusOK)["boards"].([]any)
		assert.Empty(t, l)
	}
	f.expectError(t, http.MethodPatch, path, `{"title":"Zombie"}`, alice.access, http.StatusNotFound, httpapi.CodeNotFound)
	f.expectError(t, http.MethodDelete, path, "", alice.access, http.StatusNotFound, httpapi.CodeNotFound)
}

func TestFlowConcurrentRenamesOverHTTP(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")
	created := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Race"}`, alice.access, http.StatusCreated)
	path := "/api/boards/" + created["id"].(string)

	const workers = 20
	var wg sync.WaitGroup
	seqs := make(chan float64, workers)
	bodies := make([]string, workers)
	for i := range bodies {
		bodies[i] = jsonBody(t, map[string]string{"title": "t" + strings.Repeat("x", i)})
	}
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := f.do(t, http.MethodPatch, path, bodies[i], alice.access)
			assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			seqs <- rec.json(t)["seq"].(float64)
		}()
	}
	wg.Wait()
	close(seqs)

	seen := map[float64]bool{}
	for s := range seqs {
		assert.False(t, seen[s], "seq %v returned twice", s)
		seen[s] = true
	}
	for i := 1; i <= workers; i++ {
		assert.True(t, seen[float64(i)], "seq %d missing", i)
	}
}

func TestFlowBodyLimitAndRouting(t *testing.T) {
	f := newFlow(t)
	f.expectError(t, http.MethodGet, "/api/unknown", "", "", http.StatusNotFound, httpapi.CodeNotFound)
	f.expectError(t, http.MethodGet, "/api/auth/login", "", "", http.StatusMethodNotAllowed, httpapi.CodeMethodNotAllowed)

	big := `{"email":"a@b.co","password":"` + strings.Repeat("a", 2<<20) + `"}`
	f.expectError(t, http.MethodPost, "/api/auth/login", big, "", http.StatusRequestEntityTooLarge, httpapi.CodePayloadTooLarge)
}

func TestFlowSnapshotAndEventLog(t *testing.T) {
	f := newFlow(t)
	alice := f.register(t, "alice")
	board := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Ops"}`, alice.access, http.StatusCreated)
	boardID := uuid.MustParse(board["id"].(string))
	userID := uuid.MustParse(alice.userID)

	proc := ops.NewProcessor(storage.New(f.pool))
	colID, cardID := uuid.New(), uuid.New()
	for _, op := range []struct {
		typ     string
		payload any
	}{
		{boardcontent.OpColumnCreate, boardcontent.ColumnCreate{ColumnID: colID, Title: "Todo"}},
		{boardcontent.OpCardCreate, boardcontent.CardCreate{CardID: cardID, ColumnID: colID, Title: "Card"}},
	} {
		o, err := boardcontent.NewOp(op.typ, uuid.New(), op.payload)
		require.NoError(t, err)
		out, err := proc.Apply(context.Background(), userID, boardID, o)
		require.NoError(t, err)
		require.Nil(t, out.Nack)
	}

	snap := f.mustJSON(t, http.MethodGet, "/api/boards/"+boardID.String(), "", alice.access, http.StatusOK)
	assert.EqualValues(t, 2, snap["seq"])
	assert.Len(t, snap["columns"], 1)
	assert.Len(t, snap["cards"], 1)
	assert.Len(t, snap["members"], 1)

	page := f.mustJSON(t, http.MethodGet, "/api/boards/"+boardID.String()+"/events?after_seq=0", "", alice.access, http.StatusOK)
	events := page["events"].([]any)
	require.Len(t, events, 2)
	assert.Equal(t, boardcontent.EventColumnCreated, events[0].(map[string]any)["type"])
	assert.Equal(t, boardcontent.EventCardCreated, events[1].(map[string]any)["type"])

	f.expectError(t, http.MethodGet, "/api/boards/"+boardID.String()+"/events?after_seq=3", "", alice.access, http.StatusBadRequest, httpapi.CodeValidation)
	_, err := f.pool.Exec(context.Background(), `DELETE FROM board_events WHERE board_id = $1 AND seq = 1`, boardID)
	require.NoError(t, err)
	f.expectError(t, http.MethodGet, "/api/boards/"+boardID.String()+"/events?after_seq=0", "", alice.access, http.StatusGone, httpapi.CodeGone)

	bob := f.register(t, "bob")
	f.expectError(t, http.MethodGet, "/api/boards/"+boardID.String(), "", bob.access, http.StatusNotFound, httpapi.CodeNotFound)
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
