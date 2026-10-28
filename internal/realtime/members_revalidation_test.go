package realtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/pubsub"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func dropEventType(eventType string) func(pubsub.Message) bool {
	return func(m pubsub.Message) bool {
		kind, _ := protocol.ParseChannel(m.Channel)
		if kind != protocol.ChannelBoard {
			return false
		}
		env, err := protocol.ParseEnvelope(m.Payload)
		return err == nil && env.Type == eventType
	}
}

type revalidation struct {
	env     *rttest.Env
	inst    *rttest.Instance
	owner   uuid.UUID
	bob     uuid.UUID
	bobConn *rttest.Conn
}

func newRevalidation(t *testing.T, bobRole domain.Role) *revalidation {
	t.Helper()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PresenceInterval: 15 * time.Second, PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	owner := rttest.AddUser(env, "Owner", domain.RoleOwner)
	bob := rttest.AddUser(env, "Bob", bobRole)
	inst := env.StartInstance(t, "a")
	bobConn := inst.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	return &revalidation{env: env, inst: inst, owner: owner, bob: bob, bobConn: bobConn}
}

func (r *revalidation) loseRemovalOfBob(t *testing.T) {
	t.Helper()
	r.env.Network.SetDropFilter(dropEventType(boardcontent.EventMemberRemoved))
	r.env.Board.RemoveMember(r.bob)
	appendMemberEvent(t, r.env, r.owner, boardcontent.EventMemberRemoved, boardcontent.MemberRemoved{UserID: r.bob, ActorID: r.owner})
	barrier(t, r.bobConn)
	require.Equal(t, 1, r.inst.Hub.ConnectionCount(r.env.Board.ID), "the socket of the removed member is still registered")
}

func TestHeartbeatClosesARemovedMemberWhoseRemovalEventWasLost(t *testing.T) {
	t.Parallel()
	r := newRevalidation(t, domain.RoleEditor)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.inst.Hub.Run(ctx)
	waitTimers(t, r.env, 3)

	r.loseRemovalOfBob(t)
	r.env.Clock.Advance(15 * time.Second)
	assert.Equal(t, protocol.CloseAccessRevoked, r.bobConn.ExpectClose().Code)
	waitConnections(t, r.inst, r.env.Board.ID, 0)
}

func TestAuthRefreshClosesARemovedMemberWhoseRemovalEventWasLost(t *testing.T) {
	t.Parallel()
	r := newRevalidation(t, domain.RoleEditor)
	r.loseRemovalOfBob(t)

	r.bobConn.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: r.env.IssueToken(t, r.bob)})
	assert.Equal(t, protocol.CloseAccessRevoked, r.bobConn.ExpectClose().Code)
}

func TestForbiddenOpClosesARemovedMemberWhoseRemovalEventWasLost(t *testing.T) {
	t.Parallel()
	r := newRevalidation(t, domain.RoleEditor)
	r.loseRemovalOfBob(t)

	sendOp(r.bobConn, boardcontent.OpCardCreate)
	assert.Equal(t, protocol.CloseAccessRevoked, r.bobConn.ExpectClose().Code)
}

func TestLostRoleUpgradeDoesNotKeepAMemberReadOnly(t *testing.T) {
	t.Parallel()
	r := newRevalidation(t, domain.RoleViewer)
	r.env.Network.SetDropFilter(dropEventType(boardcontent.EventMemberRoleChanged))
	r.env.Board.SetRole(r.bob, domain.RoleEditor)
	appendMemberEvent(t, r.env, r.owner, boardcontent.EventMemberRoleChanged, boardcontent.MemberRoleChanged{UserID: r.bob, Role: domain.RoleEditor, ActorID: r.owner})
	barrier(t, r.bobConn)

	opID := sendOp(r.bobConn, boardcontent.OpCardCreate)
	ev := awaitOwnEvent(r.bobConn, opID)
	assert.Equal(t, "card.create", ev.Type)
}

func TestHeartbeatRefreshesTheCachedRoleOfLiveConnections(t *testing.T) {
	t.Parallel()
	r := newRevalidation(t, domain.RoleViewer)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.inst.Hub.Run(ctx)
	waitTimers(t, r.env, 3)

	r.env.Network.SetDropFilter(dropEventType(boardcontent.EventMemberRoleChanged))
	r.env.Board.SetRole(r.bob, domain.RoleEditor)
	appendMemberEvent(t, r.env, r.owner, boardcontent.EventMemberRoleChanged, boardcontent.MemberRoleChanged{UserID: r.bob, Role: domain.RoleEditor, ActorID: r.owner})
	barrier(t, r.bobConn)
	r.env.Clock.Advance(15 * time.Second)

	require.Eventually(t, func() bool {
		return r.inst.Hub.Role(r.env.Board.ID, r.bob) == domain.RoleEditor
	}, eventually, 5*time.Millisecond)
}

func TestCachedViewerRoleIsStillNackedWhenTheDatabaseConfirmsIt(t *testing.T) {
	t.Parallel()
	r := newRevalidation(t, domain.RoleViewer)

	sendOp(r.bobConn, boardcontent.OpCardCreate)
	nack := r.bobConn.Expect(protocol.TypeNack)
	assert.NotNil(t, nack.OpID)
	assert.Zero(t, r.env.Board.ApplyCount())
}
