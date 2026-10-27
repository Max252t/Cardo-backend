package httpapi_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/members"
)

func TestInviteTokenNeverReachesTheLogsOnAnyPath(t *testing.T) {
	f := newBoardFlow(t)
	owner, bob, carol, dave := f.register(t, "owner"), f.register(t, "bob"), f.register(t, "carol"), f.register(t, "dave")
	board := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Secrets"}`, owner.access, http.StatusCreated)
	base := "/api/boards/" + board["id"].(string)

	create := func(body string) (token, id string) {
		created := f.mustJSON(t, http.MethodPost, base+"/invites", body, owner.access, http.StatusCreated)
		return created["token"].(string), created["id"].(string)
	}
	active, _ := create(`{"role":"editor","max_uses":2}`)
	revoked, revokedID := create(`{"role":"viewer"}`)
	expired, expiredID := create(`{"role":"viewer"}`)
	exhausted, _ := create(`{"role":"viewer"}`)
	onDeleted, _ := create(`{"role":"viewer"}`)

	f.mustJSON(t, http.MethodDelete, base+"/invites/"+revokedID, "", owner.access, http.StatusNoContent)
	_, err := f.pool.Exec(context.Background(), `UPDATE invites SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiredID)
	require.NoError(t, err)
	f.mustJSON(t, http.MethodPost, "/api/invites/"+exhausted+"/accept", "", bob.access, http.StatusOK)

	accept := func(token, path string, who session, method string) {
		f.do(t, method, strings.Replace(path, "{token}", token, 1), "", who.access)
	}
	tokens := []string{active, revoked, expired, exhausted, onDeleted}
	for _, token := range tokens {
		accept(token, "/api/invites/{token}/accept", carol, http.MethodPost)
		accept(token, "/api/invites/{token}/accept", carol, http.MethodPost)
		accept(token, "/api/invites/{token}/accept", dave, http.MethodPost)
		accept(token, "/api/invites/{token}/accept", session{}, http.MethodPost)
		accept(token, "/api/invites/{token}/accept", dave, http.MethodGet)
		accept(token, "/api/invites/{token}/accept", dave, http.MethodDelete)
		accept(token, "/api/invites/{token}/accept/extra", dave, http.MethodPost)
		accept(token, "/api/invites/{token}", dave, http.MethodPost)
		accept(token, "/api/invites/{token}/accept/", dave, http.MethodPost)
		accept(token, "/api/invites/{token}/accept?next=elsewhere", dave, http.MethodPost)
	}
	f.mustJSON(t, http.MethodDelete, base, "", owner.access, http.StatusNoContent)
	for _, token := range tokens {
		accept(token, "/api/invites/{token}/accept", dave, http.MethodPost)
	}

	logs := f.logs.String()
	require.NotEmpty(t, logs)
	for _, token := range tokens {
		assert.NotContains(t, logs, token, "an invite token must never be logged")
		assert.NotContains(t, logs, members.HashToken(token), "nor its hash")
	}
	assert.Contains(t, logs, "/api/invites/:token/accept", "matched requests are logged by route pattern")
}

func TestInviteTokenIsOnlyEverReturnedByTheCreateCall(t *testing.T) {
	f := newBoardFlow(t)
	owner, bob := f.register(t, "owner"), f.register(t, "bob")
	board := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Tokens"}`, owner.access, http.StatusCreated)
	base := "/api/boards/" + board["id"].(string)

	var tokens []string
	for range 3 {
		created := f.mustJSON(t, http.MethodPost, base+"/invites", `{"role":"viewer","max_uses":5}`, owner.access, http.StatusCreated)
		tokens = append(tokens, created["token"].(string))
		assert.NotContains(t, created, "token_hash")
	}
	f.mustJSON(t, http.MethodPost, "/api/invites/"+tokens[0]+"/accept", "", bob.access, http.StatusOK)

	reads := []string{base + "/invites", base + "/members", base, base + "/events?after_seq=0", "/api/boards", "/api/me"}
	for _, path := range reads {
		rec := f.do(t, http.MethodGet, path, "", owner.access)
		require.Equal(t, http.StatusOK, rec.Code, path)
		raw := rec.Body.String()
		for _, token := range tokens {
			assert.NotContains(t, raw, token, path)
			assert.NotContains(t, raw, members.HashToken(token), path)
		}
		assert.NotContains(t, raw, "token_hash", path)
		assert.NotContains(t, raw, `"token"`, path)
	}

	listed := f.mustJSON(t, http.MethodGet, base+"/invites", "", owner.access, http.StatusOK)["invites"].([]any)
	require.Len(t, listed, 3)
	for _, item := range listed {
		inv := item.(map[string]any)
		assert.ElementsMatch(t, []string{"id", "role", "expires_at", "max_uses", "uses"}, mapKeys(inv))
	}
	bobAccept := f.do(t, http.MethodPost, "/api/invites/"+tokens[0]+"/accept", "", bob.access)
	assert.NotContains(t, bobAccept.Body.String(), tokens[0])
}

func TestInviteErrorResponsesDoNotEchoTheToken(t *testing.T) {
	f := newBoardFlow(t)
	owner, bob := f.register(t, "owner"), f.register(t, "bob")
	board := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Echo"}`, owner.access, http.StatusCreated)
	base := "/api/boards/" + board["id"].(string)
	created := f.mustJSON(t, http.MethodPost, base+"/invites", `{"role":"viewer"}`, owner.access, http.StatusCreated)
	token := created["token"].(string)
	f.mustJSON(t, http.MethodDelete, base+"/invites/"+created["id"].(string), "", owner.access, http.StatusNoContent)

	for _, candidate := range []string{token, "totally-unknown-token-value"} {
		for _, bearer := range []string{bob.access, "", "garbage"} {
			rec := f.do(t, http.MethodPost, "/api/invites/"+candidate+"/accept", "", bearer)
			assert.NotContains(t, rec.Body.String(), candidate)
			for name, values := range rec.Header() {
				for _, v := range values {
					assert.NotContains(t, v, candidate, "header %s", name)
				}
			}
		}
	}
}
