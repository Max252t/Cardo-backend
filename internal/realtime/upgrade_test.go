package realtime_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
	"github.com/Max252t/Cardo-backend/internal/jwtauth"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func errorCodeOf(t testing.TB, body io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	var parsed httpapi.ErrorResponse
	require.NoError(t, json.Unmarshal(data, &parsed), string(data))
	return parsed.Error.Code
}

func dialRaw(t testing.TB, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	dialer := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	return dialer.Dial(url, header)
}

func TestUpgradeRejectionsCarryTheUnifiedErrorBody(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	stranger := uuid.New()

	tests := []struct {
		name   string
		url    string
		header http.Header
		status int
		code   string
	}{
		{"no authorization header", inst.URL(env.Board.ID), nil, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"basic scheme", inst.URL(env.Board.ID), http.Header{"Authorization": {"Basic dXNlcjpwYXNz"}}, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"bearer without token", inst.URL(env.Board.ID), http.Header{"Authorization": {"Bearer "}}, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"garbage token", inst.URL(env.Board.ID), http.Header{"Authorization": {"Bearer garbage"}}, http.StatusUnauthorized, httpapi.CodeInvalidToken},
		{"token only in the query string", inst.URL(env.Board.ID) + "?access_token=" + env.IssueToken(t, ann), nil, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"token only in a cookie", inst.URL(env.Board.ID), http.Header{"Cookie": {"access_token=" + env.IssueToken(t, ann)}}, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"token in the subprotocol header", inst.URL(env.Board.ID), http.Header{"Sec-WebSocket-Protocol": {env.IssueToken(t, ann)}}, http.StatusUnauthorized, httpapi.CodeUnauthorized},
		{"valid token of a non member", inst.URL(env.Board.ID), http.Header{"Authorization": {"Bearer " + env.IssueToken(t, stranger)}}, http.StatusNotFound, httpapi.CodeNotFound},
		{"valid member of an unknown board", inst.URL(uuid.New()), http.Header{"Authorization": {"Bearer " + env.IssueToken(t, ann)}}, http.StatusNotFound, httpapi.CodeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			conn, resp, err := dialRaw(t, tt.url, tt.header)
			require.Error(t, err)
			require.Nil(t, conn)
			require.NotNil(t, resp)
			assert.Equal(t, tt.status, resp.StatusCode)
			assert.Equal(t, tt.code, errorCodeOf(t, resp.Body))
			_ = resp.Body.Close()
		})
	}
	assert.Zero(t, inst.Hub.ConnectionCount(env.Board.ID))
}

func TestUpgradeRejectsBadBoardIDAndWrongToken(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	badURL := "ws" + inst.Server.URL[len("http"):] + "/ws/boards/not-a-uuid"
	conn, resp, err := dialRaw(t, badURL, http.Header{"Authorization": {"Bearer " + env.IssueToken(t, ann)}})
	require.Error(t, err)
	require.Nil(t, conn)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	_ = resp.Body.Close()
}

func TestUpgradeRejectsExpiredAndForeignTokens(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	expired, _, err := shortLivedTokens(env, time.Minute).Issue(ann)
	require.NoError(t, err)
	foreign, _, err := jwtauth.New(jwtauth.Config{
		Secret: []byte("another-secret-another-secret-12"), Issuer: "test", Audience: "test", TTL: time.Hour,
	}, env.Clock).Issue(ann)
	require.NoError(t, err)
	wrongAudience, _, err := jwtauth.New(jwtauth.Config{
		Secret: []byte("0123456789abcdef0123456789abcdef"), Issuer: "test", Audience: "elsewhere", TTL: time.Hour,
	}, env.Clock).Issue(ann)
	require.NoError(t, err)
	env.Clock.Advance(2 * time.Minute)

	for name, token := range map[string]string{"expired": expired, "other key": foreign, "other audience": wrongAudience} {
		conn, status, err := inst.TryDial(env.Board.ID, "Bearer "+token, nil)
		require.Error(t, err, name)
		require.Nil(t, conn, name)
		assert.Equal(t, http.StatusUnauthorized, status, name)
	}
	_, status, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusSwitchingProtocols, status)
}

func TestExpiredTokenResponseNamesTheExpiry(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	expired, _, err := shortLivedTokens(env, time.Minute).Issue(ann)
	require.NoError(t, err)
	env.Clock.Advance(2 * time.Minute)
	_, resp, dialErr := dialRaw(t, inst.URL(env.Board.ID), http.Header{"Authorization": {"Bearer " + expired}})
	require.Error(t, dialErr)
	assert.Equal(t, httpapi.CodeTokenExpired, errorCodeOf(t, resp.Body))
	_ = resp.Body.Close()
}

func TestOriginPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		allowed []string
		origin  string
		want    int
	}{
		{"no origin header is allowed without an allowlist", nil, "", http.StatusSwitchingProtocols},
		{"no origin header is allowed with an allowlist", []string{"https://app.example"}, "", http.StatusSwitchingProtocols},
		{"foreign origin without an allowlist", nil, "https://evil.example", http.StatusForbidden},
		{"allowlisted origin", []string{"https://app.example"}, "https://app.example", http.StatusSwitchingProtocols},
		{"allowlist ignores case", []string{"https://App.Example"}, "https://app.example", http.StatusSwitchingProtocols},
		{"allowlist ignores a trailing slash", []string{"https://app.example/"}, "https://app.example", http.StatusSwitchingProtocols},
		{"second entry of the allowlist", []string{"https://a.example", "https://b.example"}, "https://b.example", http.StatusSwitchingProtocols},
		{"origin not in the allowlist", []string{"https://app.example"}, "https://evil.example", http.StatusForbidden},
		{"allowlist does not match a different scheme", []string{"https://app.example"}, "http://app.example", http.StatusForbidden},
		{"allowlist does not match a subdomain", []string{"https://app.example"}, "https://evil.app.example", http.StatusForbidden},
		{"allowlist does not match a suffix attack", []string{"https://app.example"}, "https://app.example.evil.test", http.StatusForbidden},
		{"allowlist does not match a different port", []string{"https://app.example"}, "https://app.example:8443", http.StatusForbidden},
		{"null origin", []string{"https://app.example"}, "null", http.StatusForbidden},
		{"null origin without an allowlist", nil, "null", http.StatusForbidden},
		{"malformed origin without an allowlist", nil, "://", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{AllowedOrigins: tt.allowed} })
			header := http.Header{}
			if tt.origin != "" {
				header.Set("Origin", tt.origin)
			}
			conn, status, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), header)
			if tt.want == http.StatusSwitchingProtocols {
				require.NoError(t, err)
				_ = conn.Close()
			} else {
				require.Error(t, err)
			}
			assert.Equal(t, tt.want, status)
		})
	}
}

func TestSameHostOriginIsAllowedWithoutAnAllowlist(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	header := http.Header{"Origin": {inst.Server.URL}}
	conn, status, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), header)
	require.NoError(t, err)
	_ = conn.Close()
	assert.Equal(t, http.StatusSwitchingProtocols, status)
}

func TestRejectedOriginDoesNotRegisterAConnection(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	for range 5 {
		_, _, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, ann), http.Header{"Origin": {"https://evil.example"}})
		require.Error(t, err)
	}
	assert.Zero(t, inst.Hub.ConnectionCount(env.Board.ID))
	assert.False(t, inst.Broker.Subscribed(protocol.BoardChannel(env.Board.ID)))
}

func TestPlainHTTPRequestToTheSocketPathIsRejectedAfterAuthorization(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	url := inst.Server.URL + "/ws/boards/" + env.Board.ID.String()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	req.Header.Set("Authorization", "Bearer "+env.IssueToken(t, ann))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "an authorized request without upgrade headers is a bad request")
	assert.Zero(t, inst.Hub.ConnectionCount(env.Board.ID))

	post, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, nil)
	require.NoError(t, err)
	resp, err = http.DefaultClient.Do(post)
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
}

func TestMembershipIsCheckedBeforeTheUpgradeForEveryRole(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	inst := env.StartInstance(t, "a")
	for _, role := range []domain.Role{domain.RoleOwner, domain.RoleEditor, domain.RoleViewer} {
		user := rttest.AddUser(env, string(role), role)
		c := inst.Dial(t, user)
		snap := c.HandshakeSnapshot()
		assert.Equal(t, role, snap.Board.Role)
	}
	removed := rttest.AddUser(env, "Removed", domain.RoleEditor)
	env.Board.RemoveMember(removed)
	_, status, err := inst.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, removed), nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusNotFound, status)
}

func TestRejectedOriginReturnsTheUnifiedJSONError(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, inst.Server.URL+"/ws/boards/"+env.Board.ID.String(), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+env.IssueToken(t, ann))
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
	assert.Equal(t, "forbidden", errorCodeOf(t, resp.Body))
}
