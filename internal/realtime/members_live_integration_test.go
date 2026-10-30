package realtime_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/members"
	"github.com/Max252t/Cardo-backend/internal/ops"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
	"github.com/Max252t/Cardo-backend/internal/storage"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

type liveWorld struct {
	t       *testing.T
	env     *rttest.Env
	store   *storage.Store
	members *members.Service
	a, b    *rttest.Instance
	boardID uuid.UUID
	owner   domain.User
}

func newLiveWorld(t *testing.T) *liveWorld {
	t.Helper()
	store := testutil.NewStore(t)
	owner := testutil.CreateUser(t, store, testutil.UniqueEmail("owner"))
	boardID, err := uuid.NewV7()
	require.NoError(t, err)
	_, err = store.CreateBoardWithOwner(context.Background(), domain.Board{ID: boardID, Title: "Live", OwnerID: owner.ID})
	require.NoError(t, err)

	env := rttest.NewEnv(t)
	publisher := env.NewPublisher(t)
	env.Ops = ops.NewProcessor(store, ops.WithPublisher(publisher))
	env.Sync = boards.NewService(store, boards.WithPublisher(publisher))
	env.Members = store
	return &liveWorld{
		t:       t,
		env:     env,
		store:   store,
		members: members.NewService(store, members.WithPublisher(publisher)),
		a:       env.StartInstance(t, "a"),
		b:       env.StartInstance(t, "b"),
		boardID: boardID,
		owner:   owner,
	}
}

func (w *liveWorld) user(prefix string) domain.User {
	return testutil.CreateUser(w.t, w.store, testutil.UniqueEmail(prefix))
}

func (w *liveWorld) join(u domain.User, role domain.Role) {
	w.t.Helper()
	_, err := w.members.Add(context.Background(), w.owner.ID, w.boardID, u.Email, role)
	require.NoError(w.t, err)
}

func (w *liveWorld) connect(inst *rttest.Instance, u domain.User) *rttest.Conn {
	w.t.Helper()
	c := w.env.DialBoard(w.t, inst, w.boardID, u.ID)
	c.HandshakeSnapshot()
	return c
}

func (w *liveWorld) operate(c *rttest.Conn) uuid.UUID {
	w.t.Helper()
	opID := uuid.New()
	c.SendJSON(boardcontent.OpColumnCreate, &opID, boardcontent.ColumnCreate{ColumnID: uuid.New(), Title: "probe"})
	return opID
}

func outcomeOf(c *rttest.Conn, opID uuid.UUID) (confirmed bool, nackReason string) {
	for {
		env := c.MustNext()
		if env.OpID == nil || *env.OpID != opID {
			continue
		}
		if env.Type == protocol.TypeNack {
			var n boardcontent.Nack
			if err := json.Unmarshal(env.Payload, &n); err != nil {
				return false, "undecodable"
			}
			return false, n.Reason
		}
		return true, ""
	}
}

func (w *liveWorld) closeCodes(conns ...*rttest.Conn) []int {
	codes := make([]int, len(conns))
	var wg sync.WaitGroup
	for i, c := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = c.ExpectClose().Code
		}()
	}
	wg.Wait()
	return codes
}

func TestRemovalClosesEveryDeviceOnEveryInstanceWithinASecondAndSparesOthers(t *testing.T) {
	w := newLiveWorld(t)
	bob, cat := w.user("bob"), w.user("cat")
	w.join(bob, domain.RoleEditor)
	w.join(cat, domain.RoleViewer)

	ownerConn := w.connect(w.a, w.owner)
	bobPhone := w.connect(w.a, bob)
	bobTablet := w.connect(w.b, bob)
	bobLaptop := w.connect(w.b, bob)
	catConn := w.connect(w.b, cat)
	waitConnections(t, w.a, w.boardID, 2)
	waitConnections(t, w.b, w.boardID, 3)

	start := time.Now()
	require.NoError(t, w.members.Remove(context.Background(), w.owner.ID, w.boardID, bob.ID))
	codes := w.closeCodes(bobPhone, bobTablet, bobLaptop)
	elapsed := time.Since(start)

	assert.Equal(t, []int{protocol.CloseAccessRevoked, protocol.CloseAccessRevoked, protocol.CloseAccessRevoked}, codes)
	assert.Less(t, elapsed, time.Second, "removal must reach every instance within a second")
	waitConnections(t, w.a, w.boardID, 1)
	waitConnections(t, w.b, w.boardID, 1)

	for _, c := range []*rttest.Conn{ownerConn, catConn} {
		removed := decodeAs[boardcontent.MemberRemoved](t, drainUntil(c, boardcontent.EventMemberRemoved))
		assert.Equal(t, bob.ID, removed.UserID)
		assert.Equal(t, w.owner.ID, removed.ActorID)
	}
	opID := w.operate(ownerConn)
	confirmed, reason := outcomeOf(ownerConn, opID)
	assert.True(t, confirmed, reason)
	assert.Equal(t, 1, w.a.Hub.ConnectionCount(w.boardID))
	assert.Equal(t, 1, w.b.Hub.ConnectionCount(w.boardID))

	_, status, err := w.a.TryDial(w.boardID, "Bearer "+w.env.IssueToken(t, bob.ID), nil)
	require.Error(t, err)
	assert.Equal(t, 404, status, "a removed member is rejected before the upgrade on every instance")
	_, status, err = w.b.TryDial(w.boardID, "Bearer "+w.env.IssueToken(t, bob.ID), nil)
	require.Error(t, err)
	assert.Equal(t, 404, status)
}

func TestSelfLeaveClosesTheLeaversConnectionsOnly(t *testing.T) {
	w := newLiveWorld(t)
	bob, cat := w.user("bob"), w.user("cat")
	w.join(bob, domain.RoleEditor)
	w.join(cat, domain.RoleViewer)
	bobPhone := w.connect(w.a, bob)
	bobTablet := w.connect(w.b, bob)
	catConn := w.connect(w.a, cat)

	start := time.Now()
	require.NoError(t, w.members.Remove(context.Background(), bob.ID, w.boardID, bob.ID))
	assert.Equal(t, []int{protocol.CloseAccessRevoked, protocol.CloseAccessRevoked}, w.closeCodes(bobPhone, bobTablet))
	assert.Less(t, time.Since(start), time.Second)

	removed := decodeAs[boardcontent.MemberRemoved](t, drainUntil(catConn, boardcontent.EventMemberRemoved))
	assert.Equal(t, bob.ID, removed.UserID)
	assert.Equal(t, bob.ID, removed.ActorID)
	waitConnections(t, w.a, w.boardID, 1)
}

func TestOpsRightAfterARemovalNeverLandInTheLog(t *testing.T) {
	w := newLiveWorld(t)
	bob := w.user("bob")
	w.join(bob, domain.RoleEditor)
	bobConn := w.connect(w.a, bob)

	require.NoError(t, w.members.Remove(context.Background(), w.owner.ID, w.boardID, bob.ID))
	for range 5 {
		opID := uuid.New()
		_ = bobConn.WriteJSON(map[string]any{"v": 1, "type": boardcontent.OpColumnCreate, "op_id": opID, "payload": map[string]any{"column_id": uuid.New(), "title": "late"}})
	}
	bobConn.ExpectClose()

	page, err := w.store.EventsAfter(context.Background(), w.boardID, w.owner.ID, 0, 100)
	require.NoError(t, err)
	removedAt := int64(0)
	for _, ev := range page.Events {
		if ev.Type == boardcontent.EventMemberRemoved {
			removedAt = ev.Seq
		}
		if removedAt != 0 {
			assert.NotEqual(t, boardcontent.EventColumnCreated, ev.Type, "nothing by the removed member after the removal")
		}
	}
	assert.NotZero(t, removedAt)
}

func TestDowngradedEditorIsNackedOnTheNextOpWithoutWaitingForTheEvent(t *testing.T) {
	w := newLiveWorld(t)
	bob := w.user("bob")
	w.join(bob, domain.RoleEditor)
	bobA := w.connect(w.a, bob)
	bobB := w.connect(w.b, bob)

	for _, c := range []*rttest.Conn{bobA, bobB} {
		confirmed, reason := outcomeOf(c, w.operate(c))
		require.True(t, confirmed, reason)
	}
	_, err := w.members.ChangeRole(context.Background(), w.owner.ID, w.boardID, bob.ID, domain.RoleViewer)
	require.NoError(t, err)

	for _, c := range []*rttest.Conn{bobA, bobB} {
		confirmed, reason := outcomeOf(c, w.operate(c))
		assert.False(t, confirmed, "a downgraded editor must not be able to write")
		assert.Equal(t, boardcontent.ReasonForbidden, reason)
	}

	page, err := w.store.EventsAfter(context.Background(), w.boardID, w.owner.ID, 0, 100)
	require.NoError(t, err)
	last := page.Events[len(page.Events)-1]
	assert.Equal(t, boardcontent.EventMemberRoleChanged, last.Type, "the rejected ops left no events behind")
}

func TestUpgradedViewerCanOperateOnceTheRoleEventIsApplied(t *testing.T) {
	w := newLiveWorld(t)
	bob := w.user("bob")
	w.join(bob, domain.RoleViewer)
	bobA := w.connect(w.a, bob)
	bobB := w.connect(w.b, bob)

	confirmed, reason := outcomeOf(bobA, w.operate(bobA))
	require.False(t, confirmed)
	assert.Equal(t, boardcontent.ReasonForbidden, reason)

	_, err := w.members.ChangeRole(context.Background(), w.owner.ID, w.boardID, bob.ID, domain.RoleEditor)
	require.NoError(t, err)

	for _, c := range []*rttest.Conn{bobA, bobB} {
		require.Eventually(t, func() bool {
			ok, _ := outcomeOf(c, w.operate(c))
			return ok
		}, eventually, 20*time.Millisecond, "an upgraded viewer can write on every instance")
	}
}

func TestRoleChangesOnlyAffectTheTargetsConnections(t *testing.T) {
	w := newLiveWorld(t)
	bob, cat := w.user("bob"), w.user("cat")
	w.join(bob, domain.RoleEditor)
	w.join(cat, domain.RoleEditor)
	bobConn := w.connect(w.a, bob)
	catConn := w.connect(w.b, cat)
	ownerConn := w.connect(w.a, w.owner)

	_, err := w.members.ChangeRole(context.Background(), w.owner.ID, w.boardID, bob.ID, domain.RoleViewer)
	require.NoError(t, err)
	drainUntil(bobConn, boardcontent.EventMemberRoleChanged)
	drainUntil(catConn, boardcontent.EventMemberRoleChanged)

	confirmed, reason := outcomeOf(bobConn, w.operate(bobConn))
	assert.False(t, confirmed)
	assert.Equal(t, boardcontent.ReasonForbidden, reason)
	for _, c := range []*rttest.Conn{catConn, ownerConn} {
		confirmed, reason := outcomeOf(c, w.operate(c))
		assert.True(t, confirmed, reason)
	}
}

func TestInviteAcceptanceIsBroadcastAndTheNewcomerCanConnectAndOperate(t *testing.T) {
	w := newLiveWorld(t)
	ownerA := w.connect(w.a, w.owner)
	ownerB := w.connect(w.b, w.owner)
	bob := w.user("bob")
	invite, err := w.members.CreateInvite(context.Background(), w.owner.ID, w.boardID, members.InviteInput{Role: domain.RoleEditor})
	require.NoError(t, err)

	_, status, dialErr := w.a.TryDial(w.boardID, "Bearer "+w.env.IssueToken(t, bob.ID), nil)
	require.Error(t, dialErr)
	assert.Equal(t, 404, status, "before accepting there is no access")

	_, err = w.members.Accept(context.Background(), bob.ID, invite.Token)
	require.NoError(t, err)
	for _, c := range []*rttest.Conn{ownerA, ownerB} {
		added := decodeAs[boardcontent.MemberAdded](t, drainUntil(c, boardcontent.EventMemberAdded))
		assert.Equal(t, bob.ID, added.UserID)
		assert.Equal(t, bob.ID, added.ActorID)
		assert.Equal(t, domain.RoleEditor, added.Role)
		assert.Equal(t, bob.DisplayName, added.DisplayName)
	}

	bobConn := w.env.DialBoard(t, w.b, w.boardID, bob.ID)
	snap := bobConn.HandshakeSnapshot()
	assert.Equal(t, domain.RoleEditor, snap.Board.Role)
	assert.Len(t, snap.Members, 2)
	confirmed, reason := outcomeOf(bobConn, w.operate(bobConn))
	assert.True(t, confirmed, reason)
}

func TestRemovedMemberCanComeBackThroughANewInvite(t *testing.T) {
	w := newLiveWorld(t)
	bob := w.user("bob")
	w.join(bob, domain.RoleEditor)
	first := w.connect(w.a, bob)
	require.NoError(t, w.members.Remove(context.Background(), w.owner.ID, w.boardID, bob.ID))
	assert.Equal(t, protocol.CloseAccessRevoked, first.ExpectClose().Code)

	invite, err := w.members.CreateInvite(context.Background(), w.owner.ID, w.boardID, members.InviteInput{Role: domain.RoleViewer})
	require.NoError(t, err)
	_, err = w.members.Accept(context.Background(), bob.ID, invite.Token)
	require.NoError(t, err)

	again := w.connect(w.a, bob)
	confirmed, reason := outcomeOf(again, w.operate(again))
	assert.False(t, confirmed, "the new membership carries the invite role")
	assert.Equal(t, boardcontent.ReasonForbidden, reason)
}

func TestReconnectingClientCatchesUpOnMemberEventsThroughTheDelta(t *testing.T) {
	w := newLiveWorld(t)
	bob := w.user("bob")
	w.connect(w.a, w.owner)
	w.join(bob, domain.RoleViewer)
	_, err := w.members.ChangeRole(context.Background(), w.owner.ID, w.boardID, bob.ID, domain.RoleEditor)
	require.NoError(t, err)
	require.NoError(t, w.members.Remove(context.Background(), w.owner.ID, w.boardID, bob.ID))

	late := w.env.DialBoard(t, w.b, w.boardID, w.owner.ID)
	late.Hello(int64Ptr(0))
	delta := decodeAs[protocol.SyncDelta](t, late.Expect(protocol.TypeSyncDelta))
	var types []string
	for _, ev := range delta.Events {
		types = append(types, ev.Type)
	}
	assert.Equal(t, []string{boardcontent.EventMemberAdded, boardcontent.EventMemberRoleChanged, boardcontent.EventMemberRemoved}, types)
}

func TestConcurrentRemovalsAndOpsKeepTheLiveRosterConsistent(t *testing.T) {
	w := newLiveWorld(t)
	users := make([]domain.User, 4)
	conns := make([]*rttest.Conn, len(users))
	for i := range users {
		users[i] = w.user("u")
		w.join(users[i], domain.RoleEditor)
		inst := w.a
		if i%2 == 1 {
			inst = w.b
		}
		conns[i] = w.connect(inst, users[i])
	}
	var wg sync.WaitGroup
	for i := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opID := uuid.New()
			_ = conns[i].WriteJSON(map[string]any{"v": 1, "type": boardcontent.OpColumnCreate, "op_id": opID, "payload": map[string]any{"column_id": uuid.New(), "title": "race"}})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, w.members.Remove(context.Background(), w.owner.ID, w.boardID, users[i].ID))
		}()
	}
	wg.Wait()
	for _, c := range conns {
		assert.Equal(t, protocol.CloseAccessRevoked, c.ExpectClose().Code)
	}
	waitConnections(t, w.a, w.boardID, 0)
	waitConnections(t, w.b, w.boardID, 0)

	page, err := w.store.EventsAfter(context.Background(), w.boardID, w.owner.ID, 0, 100)
	require.NoError(t, err)
	removedAt := map[uuid.UUID]int64{}
	for _, ev := range page.Events {
		if ev.Type == boardcontent.EventMemberRemoved {
			var p boardcontent.MemberRemoved
			require.NoError(t, json.Unmarshal(ev.Payload, &p))
			removedAt[p.UserID] = ev.Seq
		}
	}
	require.Len(t, removedAt, len(users))
	for _, ev := range page.Events {
		if ev.Type != boardcontent.EventColumnCreated || ev.ActorID == nil {
			continue
		}
		if at, removed := removedAt[*ev.ActorID]; removed {
			assert.Less(t, ev.Seq, at, "an op by a removed member committed after the removal")
		}
	}
}
