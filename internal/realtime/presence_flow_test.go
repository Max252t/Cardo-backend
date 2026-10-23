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
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func presenceProbe(t testing.TB, env *rttest.Env) *presence.Service {
	t.Helper()
	broker := env.Network.NewBroker()
	t.Cleanup(func() { _ = broker.Close() })
	return presence.New(env.Presence, broker, env.Clock, "probe")
}

func onlineNames(t testing.TB, svc *presence.Service, boardID uuid.UUID) []string {
	t.Helper()
	users, err := svc.Online(context.Background(), boardID)
	require.NoError(t, err)
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Name)
	}
	return names
}

func nextPresence(t testing.TB, c *rttest.Conn) protocol.Envelope {
	t.Helper()
	for {
		msg := c.MustNext()
		if msg.Type == protocol.TypePresenceJoin || msg.Type == protocol.TypePresenceLeave {
			return msg
		}
	}
}

func TestPresenceListShowsEveryOnlineUserOnceAcrossInstancesAndDevices(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	cat := rttest.AddUser(env, "Cat", domain.RoleViewer)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	for _, inst := range []struct {
		i    *rttest.Instance
		user uuid.UUID
	}{{a, ann}, {a, ann}, {b, ann}, {b, bob}} {
		inst.i.Dial(t, inst.user).HandshakeSnapshot()
	}

	late := a.Dial(t, cat)
	late.Hello(nil)
	late.Expect(protocol.TypeSyncSnapshot)
	users := presenceList(t, late.Expect(protocol.TypePresenceList))
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Name)
	}
	assert.Equal(t, []string{"Ann", "Bob", "Cat"}, names)
	late.Expect(protocol.TypeSyncReady)
}

func TestPresenceJoinIsAnnouncedOncePerUserAndNeverEchoedToThatUser(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()

	bobPhone := b.Dial(t, bob)
	bobPhone.HandshakeSnapshot()
	join := decodeAs[protocol.PresenceJoin](t, annConn.Expect(protocol.TypePresenceJoin))
	assert.Equal(t, bob, join.User.ID)

	bobTablet := a.Dial(t, bob)
	bobTablet.HandshakeSnapshot()
	annSecond := b.Dial(t, ann)
	annSecond.HandshakeSnapshot()

	opID := sendOp(annConn, boardcontent.OpCardCreate)
	for name, c := range map[string]*rttest.Conn{"ann": annConn, "bob phone": bobPhone, "bob tablet": bobTablet, "ann second": annSecond} {
		for {
			msg := c.MustNext()
			require.NotEqual(t, protocol.TypePresenceJoin, msg.Type, "%s must not see another join", name)
			require.NotEqual(t, protocol.TypePresenceLeave, msg.Type, name)
			if msg.OpID != nil && *msg.OpID == opID {
				break
			}
		}
	}
}

func TestPresenceLeaveIsAnnouncedOnlyWhenTheLastDeviceGoesAndRejoinAnnouncesAgain(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	watcher := a.Dial(t, ann)
	watcher.HandshakeSnapshot()
	first := a.Dial(t, bob)
	first.HandshakeSnapshot()
	second := b.Dial(t, bob)
	second.HandshakeSnapshot()
	assert.Equal(t, protocol.TypePresenceJoin, nextPresence(t, watcher).Type)

	require.NoError(t, first.Close())
	waitConnections(t, a, env.Board.ID, 1)
	opID := sendOp(second, boardcontent.OpCardCreate)
	for {
		msg := watcher.MustNext()
		require.NotEqual(t, protocol.TypePresenceLeave, msg.Type, "bob is still connected through the other instance")
		if msg.OpID != nil && *msg.OpID == opID {
			break
		}
	}

	require.NoError(t, second.Close())
	leave := nextPresence(t, watcher)
	assert.Equal(t, protocol.TypePresenceLeave, leave.Type)
	assert.Equal(t, bob, decodeAs[protocol.PresenceLeave](t, leave).UserID)

	again := b.Dial(t, bob)
	again.HandshakeSnapshot()
	rejoin := nextPresence(t, watcher)
	assert.Equal(t, protocol.TypePresenceJoin, rejoin.Type)
	assert.Equal(t, bob, decodeAs[protocol.PresenceJoin](t, rejoin).User.ID)
}

func TestHeartbeatKeepsLiveUsersOnlineAcrossManyTTLs(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PresenceInterval: 15 * time.Second, PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	a := env.StartInstance(t, "a")
	b := env.StartInstance(t, "b")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Hub.Run(ctx)
	go b.Hub.Run(ctx)

	annConn := a.Dial(t, ann)
	annConn.HandshakeSnapshot()
	bobConn := b.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	annConn.Expect(protocol.TypePresenceJoin)
	probe := presenceProbe(t, env)
	waitTimers(t, env, 6)

	for range 8 {
		env.Clock.Advance(15 * time.Second)
		now := env.Clock.Now()
		require.Eventually(t, func() bool {
			fresh, err := env.Presence.Fresh(ctx, env.Board.ID, now.Add(-time.Millisecond))
			return err == nil && len(fresh) == 2
		}, eventually, 5*time.Millisecond)
	}
	assert.Equal(t, []string{"Ann", "Bob"}, onlineNames(t, probe, env.Board.ID))

	opID := sendOp(bobConn, boardcontent.OpCardCreate)
	for {
		msg := annConn.MustNext()
		require.NotEqual(t, protocol.TypePresenceLeave, msg.Type, "live users are never swept")
		if msg.OpID != nil && *msg.OpID == opID {
			break
		}
	}
}

func TestEntriesOfADeadInstanceAreSweptAndAnnouncedAsLeave(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PresenceInterval: time.Minute, PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	ghostID := uuid.New()
	live := env.StartInstance(t, "live")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go live.Hub.Run(ctx)

	annConn := live.Dial(t, ann)
	annConn.HandshakeSnapshot()
	waitTimers(t, env, 3)

	deadBroker := env.Network.NewBroker()
	dead := presence.New(env.Presence, deadBroker, env.Clock, "dead")
	require.NoError(t, dead.Join(ctx, env.Board.ID, protocol.PresenceUser{ID: ghostID, Name: "Ghost", AvatarColor: "#112233"}))
	require.NoError(t, deadBroker.Close())
	join := nextPresence(t, annConn)
	require.Equal(t, protocol.TypePresenceJoin, join.Type)
	assert.Equal(t, ghostID, decodeAs[protocol.PresenceJoin](t, join).User.ID)

	probe := presenceProbe(t, env)
	assert.Equal(t, []string{"Ann", "Ghost"}, onlineNames(t, probe, env.Board.ID))

	env.Clock.Advance(time.Minute)
	leave := nextPresence(t, annConn)
	require.Equal(t, protocol.TypePresenceLeave, leave.Type)
	assert.Equal(t, ghostID, decodeAs[protocol.PresenceLeave](t, leave).UserID)
	assert.Equal(t, []string{"Ann"}, onlineNames(t, probe, env.Board.ID))

	late := live.Dial(t, ann)
	late.Hello(nil)
	late.Expect(protocol.TypeSyncSnapshot)
	users := presenceList(t, late.Expect(protocol.TypePresenceList))
	require.Len(t, users, 1)
	assert.Equal(t, ann, users[0].ID)
}

func TestGhostOfADeadInstanceDoesNotHideAUserWhoIsStillConnectedElsewhere(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{PresenceInterval: time.Minute, PingInterval: 1000 * time.Hour, PongWait: 2000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	bob := rttest.AddUser(env, "Bob", domain.RoleEditor)
	live := env.StartInstance(t, "live")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go live.Hub.Run(ctx)

	watcher := live.Dial(t, ann)
	watcher.HandshakeSnapshot()
	bobConn := live.Dial(t, bob)
	bobConn.HandshakeSnapshot()
	require.Equal(t, protocol.TypePresenceJoin, nextPresence(t, watcher).Type)
	waitTimers(t, env, 5)

	deadBroker := env.Network.NewBroker()
	dead := presence.New(env.Presence, deadBroker, env.Clock, "dead")
	require.NoError(t, dead.Join(ctx, env.Board.ID, protocol.PresenceUser{ID: bob, Name: "Bob", AvatarColor: "#112233"}))
	require.NoError(t, deadBroker.Close())

	env.Clock.Advance(time.Minute)
	opID := sendOp(bobConn, boardcontent.OpCardCreate)
	for {
		msg := watcher.MustNext()
		require.NotEqual(t, protocol.TypePresenceLeave, msg.Type, "bob is still connected to a live instance")
		if msg.OpID != nil && *msg.OpID == opID {
			break
		}
	}
	probe := presenceProbe(t, env)
	assert.Equal(t, []string{"Ann", "Bob"}, onlineNames(t, probe, env.Board.ID))
}

func TestRunStopsWhenItsContextIsCancelled(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	inst := env.StartInstance(t, "a")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		inst.Hub.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(eventually):
		t.Fatal("Run did not stop")
	}
}

func TestClosingTheLastConnectionRemovesPresenceFromTheStore(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	probe := presenceProbe(t, env)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	assert.Equal(t, []string{"Ann"}, onlineNames(t, probe, env.Board.ID))

	require.NoError(t, c.Close())
	require.Eventually(t, func() bool { return len(onlineNames(t, probe, env.Board.ID)) == 0 }, eventually, 5*time.Millisecond)
}
