package realtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func TestMalformedMemberEventsNeitherCloseNorChangeAnything(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	owner := rttest.AddUser(env, "Owner", domain.RoleOwner)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	bobConn := inst.Dial(t, bob)
	bobConn.HandshakeSnapshot()

	bad := []struct {
		eventType string
		payload   string
	}{
		{boardcontent.EventMemberRemoved, `"garbage"`},
		{boardcontent.EventMemberRemoved, `{"user_id":"not-a-uuid"}`},
		{boardcontent.EventMemberRemoved, `{"user_id":5}`},
		{boardcontent.EventMemberRemoved, `{}`},
		{boardcontent.EventMemberRemoved, `{"user_id":"` + uuid.NewString() + `"}`},
		{boardcontent.EventMemberRoleChanged, `"garbage"`},
		{boardcontent.EventMemberRoleChanged, `{"user_id":"` + bob.String() + `","role":"admin"}`},
		{boardcontent.EventMemberRoleChanged, `{"user_id":"` + bob.String() + `","role":""}`},
		{boardcontent.EventMemberRoleChanged, `{"user_id":"` + bob.String() + `"}`},
		{boardcontent.EventMemberRoleChanged, `{"user_id":"` + bob.String() + `","role":"VIEWER"}`},
		{boardcontent.EventMemberRoleChanged, `{"user_id":"` + uuid.NewString() + `","role":"viewer"}`},
	}
	for _, b := range bad {
		_, err := env.Board.Append(context.Background(), b.eventType, json.RawMessage(b.payload), owner)
		require.NoError(t, err)
	}

	last := int64(len(bad))
	assert.Equal(t, last, env.Board.Seq())
	for {
		if msg := bobConn.MustNext(); msg.Seq >= last {
			break
		}
	}
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
	opID := sendOp(bobConn, boardcontent.OpCardCreate)
	assert.NotNil(t, awaitOwnEvent(bobConn, opID).OpID, "the editor can still write")
}
