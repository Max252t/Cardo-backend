package realtime_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func appendMemberEvent(t *testing.T, env *rttest.Env, actor uuid.UUID, eventType string, payload any) {
	t.Helper()
	_, err := env.Board.Append(context.Background(), eventType, payload, actor)
	require.NoError(t, err)
}

func TestMemberRemovedClosesOnlyThatUsersConnectionsOnEveryInstance(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	owner := rttest.AddUser(env, "Owner", domain.RoleOwner)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	ownerConn := a.Dial(t, owner)
	ownerConn.HandshakeSnapshot()
	bobPhone := a.Dial(t, bob)
	bobPhone.HandshakeSnapshot()
	bobTablet := b.Dial(t, bob)
	bobTablet.HandshakeSnapshot()

	env.Board.RemoveMember(bob)
	appendMemberEvent(t, env, owner, boardcontent.EventMemberRemoved, boardcontent.MemberRemoved{UserID: bob, ActorID: owner})

	start := time.Now()
	for _, c := range []*rttest.Conn{bobPhone, bobTablet} {
		assert.Equal(t, protocol.CloseAccessRevoked, c.ExpectClose().Code)
	}
	assert.Less(t, time.Since(start), time.Second)

	ev := drainUntil(ownerConn, boardcontent.EventMemberRemoved)
	assert.Equal(t, boardcontent.EventMemberRemoved, ev.Type)
	sendOp(ownerConn, boardcontent.OpCardCreate)
	drainUntil(ownerConn, "card.create")

	_, status, err := a.TryDial(env.Board.ID, "Bearer "+env.IssueToken(t, bob), nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusNotFound, status, "a removed member is rejected before the upgrade")
}

func TestMemberRoleChangeUpdatesTheCachedRoleOfLiveConnections(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	owner := rttest.AddUser(env, "Owner", domain.RoleOwner)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	inst := env.StartInstance(t, "a")
	bobConn := inst.Dial(t, bob)
	bobConn.HandshakeSnapshot()

	sendOp(bobConn, boardcontent.OpCardCreate)
	bobConn.Expect("card.create")

	env.Board.SetRole(bob, domain.RoleViewer)
	appendMemberEvent(t, env, owner, boardcontent.EventMemberRoleChanged, boardcontent.MemberRoleChanged{UserID: bob, Role: domain.RoleViewer, ActorID: owner})
	drainUntil(bobConn, boardcontent.EventMemberRoleChanged)
	require.Eventually(t, func() bool {
		sendOp(bobConn, boardcontent.OpCardCreate)
		for {
			msg := bobConn.MustNext()
			if msg.Type == protocol.TypeNack {
				var nack boardcontent.Nack
				require.NoError(t, json.Unmarshal(msg.Payload, &nack))
				return nack.Reason == boardcontent.ReasonForbidden
			}
			if msg.Type == "card.create" {
				return false
			}
		}
	}, eventually, 20*time.Millisecond)

	env.Board.SetRole(bob, domain.RoleEditor)
	appendMemberEvent(t, env, owner, boardcontent.EventMemberRoleChanged, boardcontent.MemberRoleChanged{UserID: bob, Role: domain.RoleEditor, ActorID: owner})
	drainUntil(bobConn, boardcontent.EventMemberRoleChanged)
	require.Eventually(t, func() bool {
		sendOp(bobConn, boardcontent.OpCardCreate)
		for {
			msg := bobConn.MustNext()
			if msg.Type == protocol.TypeNack {
				return false
			}
			if msg.Type == "card.create" {
				return true
			}
		}
	}, eventually, 20*time.Millisecond)
}

func TestMemberAddedIsBroadcastLikeAnyOtherEvent(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	owner := rttest.AddUser(env, "Owner", domain.RoleOwner)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, owner)
	c.HandshakeSnapshot()

	newcomer := uuid.New()
	appendMemberEvent(t, env, owner, boardcontent.EventMemberAdded, boardcontent.MemberAdded{UserID: newcomer, DisplayName: "New", AvatarColor: "#112233", Role: domain.RoleViewer, ActorID: owner})
	ev := c.Expect(boardcontent.EventMemberAdded)
	assert.Equal(t, int64(1), ev.Seq)
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
}
