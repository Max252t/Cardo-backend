package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/httpapi"
)

func TestMemberAndInviteEndpointsEndToEnd(t *testing.T) {
	f := newBoardFlow(t)
	owner := f.register(t, "owner")
	bob := f.register(t, "bob")
	carol := f.register(t, "carol")
	board := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Team"}`, owner.access, http.StatusCreated)
	base := "/api/boards/" + board["id"].(string)

	added := f.mustJSON(t, http.MethodPost, base+"/members", jsonBody(t, map[string]string{"email": bob.email, "role": "viewer"}), owner.access, http.StatusCreated)
	assert.Equal(t, bob.userID, added["user_id"])
	assert.Equal(t, "viewer", added["role"])
	assert.NotEmpty(t, added["joined_at"])

	f.expectError(t, http.MethodPost, base+"/members", jsonBody(t, map[string]string{"email": bob.email, "role": "editor"}), owner.access, http.StatusConflict, httpapi.CodeAlreadyMember)
	f.expectError(t, http.MethodPost, base+"/members", jsonBody(t, map[string]string{"email": "ghost@example.com", "role": "editor"}), owner.access, http.StatusNotFound, httpapi.CodeNotFound)
	f.expectError(t, http.MethodPost, base+"/members", jsonBody(t, map[string]string{"email": carol.email, "role": "owner"}), owner.access, http.StatusBadRequest, httpapi.CodeValidation)
	f.expectError(t, http.MethodPost, base+"/members", jsonBody(t, map[string]string{"email": carol.email, "role": "viewer"}), bob.access, http.StatusForbidden, httpapi.CodeForbidden)
	f.expectError(t, http.MethodPost, base+"/members", jsonBody(t, map[string]string{"email": carol.email, "role": "viewer"}), carol.access, http.StatusNotFound, httpapi.CodeNotFound)

	list := f.mustJSON(t, http.MethodGet, base+"/members", "", bob.access, http.StatusOK)
	assert.Len(t, list["members"], 2)
	f.expectError(t, http.MethodGet, base+"/members", "", carol.access, http.StatusNotFound, httpapi.CodeNotFound)

	changed := f.mustJSON(t, http.MethodPatch, base+"/members/"+bob.userID, `{"role":"editor"}`, owner.access, http.StatusOK)
	assert.Equal(t, "editor", changed["role"])
	f.expectError(t, http.MethodPatch, base+"/members/"+owner.userID, `{"role":"viewer"}`, owner.access, http.StatusForbidden, httpapi.CodeForbidden)
	f.expectError(t, http.MethodPatch, base+"/members/"+bob.userID, `{"role":"viewer"}`, bob.access, http.StatusForbidden, httpapi.CodeForbidden)
	f.expectError(t, http.MethodPatch, base+"/members/not-a-uuid", `{"role":"viewer"}`, owner.access, http.StatusBadRequest, httpapi.CodeValidation)

	f.expectError(t, http.MethodDelete, base+"/members/"+owner.userID, "", owner.access, http.StatusForbidden, httpapi.CodeForbidden)
	f.mustJSON(t, http.MethodDelete, base+"/members/"+bob.userID, "", bob.access, http.StatusNoContent)
	f.expectError(t, http.MethodGet, base+"/members", "", bob.access, http.StatusNotFound, httpapi.CodeNotFound)

	events := f.mustJSON(t, http.MethodGet, base+"/events?after_seq=0", "", owner.access, http.StatusOK)
	var types []string
	for _, ev := range events["events"].([]any) {
		types = append(types, ev.(map[string]any)["type"].(string))
	}
	assert.Equal(t, []string{boardcontent.EventMemberAdded, boardcontent.EventMemberRoleChanged, boardcontent.EventMemberRemoved}, types)
	snap := f.mustJSON(t, http.MethodGet, base, "", owner.access, http.StatusOK)
	assert.Len(t, snap["members"], 1)
}

func TestInviteEndpointsAndTokenStaysOutOfLogs(t *testing.T) {
	f := newBoardFlow(t)
	owner := f.register(t, "owner")
	bob := f.register(t, "bob")
	carol := f.register(t, "carol")
	board := f.mustJSON(t, http.MethodPost, "/api/boards", `{"title":"Team"}`, owner.access, http.StatusCreated)
	base := "/api/boards/" + board["id"].(string)

	f.expectError(t, http.MethodPost, base+"/invites", `{"role":"viewer"}`, bob.access, http.StatusNotFound, httpapi.CodeNotFound)
	f.expectError(t, http.MethodPost, base+"/invites", `{"role":"owner"}`, owner.access, http.StatusBadRequest, httpapi.CodeValidation)
	f.expectError(t, http.MethodPost, base+"/invites", `{"role":"viewer","max_uses":1000}`, owner.access, http.StatusBadRequest, httpapi.CodeValidation)

	created := f.mustJSON(t, http.MethodPost, base+"/invites", `{"role":"editor","expires_in":3600,"max_uses":2}`, owner.access, http.StatusCreated)
	token := created["token"].(string)
	assert.GreaterOrEqual(t, len(token), 43)
	assert.Equal(t, "editor", created["role"])
	assert.EqualValues(t, 2, created["max_uses"])
	assert.NotEmpty(t, created["id"])
	assert.NotEmpty(t, created["expires_at"])

	listed := f.mustJSON(t, http.MethodGet, base+"/invites", "", owner.access, http.StatusOK)
	invites := listed["invites"].([]any)
	require.Len(t, invites, 1)
	assert.NotContains(t, invites[0], "token")
	assert.NotContains(t, invites[0], "token_hash")
	f.expectError(t, http.MethodGet, base+"/invites", "", bob.access, http.StatusNotFound, httpapi.CodeNotFound)

	accepted := f.mustJSON(t, http.MethodPost, "/api/invites/"+token+"/accept", "", bob.access, http.StatusOK)
	assert.Equal(t, board["id"], accepted["id"])
	assert.Equal(t, "editor", accepted["role"])
	again := f.mustJSON(t, http.MethodPost, "/api/invites/"+token+"/accept", "", bob.access, http.StatusOK)
	assert.Equal(t, "editor", again["role"])

	f.expectError(t, http.MethodPost, "/api/invites/"+token+"/accept", "", "", http.StatusUnauthorized, httpapi.CodeUnauthorized)
	f.expectError(t, http.MethodPost, "/api/invites/unknown-token/accept", "", carol.access, http.StatusNotFound, httpapi.CodeNotFound)

	f.mustJSON(t, http.MethodDelete, base+"/invites/"+created["id"].(string), "", owner.access, http.StatusNoContent)
	f.expectError(t, http.MethodPost, "/api/invites/"+token+"/accept", "", carol.access, http.StatusGone, httpapi.CodeInviteGone)
	f.expectError(t, http.MethodDelete, base+"/invites/00000000-0000-0000-0000-000000000000", "", owner.access, http.StatusNotFound, httpapi.CodeNotFound)

	logs := f.logs.String()
	assert.Contains(t, logs, "/api/invites/:token/accept")
	assert.False(t, strings.Contains(logs, token), "the invite token must never be logged")
}
