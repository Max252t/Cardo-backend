package presence_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestRedisStoreFreshAndStaleBoundaries(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := context.Background()
	store := presence.NewRedisStore(client)
	board := uuid.New()
	base := time.UnixMilli(1_900_000_000_000)

	require.NoError(t, store.Put(ctx, board, []string{"old|a"}, base))
	require.NoError(t, store.Put(ctx, board, []string{"mid|a", "mid|b"}, base.Add(10*time.Second)))
	require.NoError(t, store.Put(ctx, board, []string{"new|a"}, base.Add(20*time.Second)))

	fresh, err := store.Fresh(ctx, board, base.Add(10*time.Second))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"new|a"}, fresh, "fresh is exclusive of the cutoff")

	fresh, err = store.Fresh(ctx, board, base.Add(10*time.Second-time.Millisecond))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"mid|a", "mid|b", "new|a"}, fresh)

	stale, err := store.Stale(ctx, board, base.Add(10*time.Second))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"old|a", "mid|a", "mid|b"}, stale, "stale is inclusive of the cutoff")

	stale, err = store.Stale(ctx, board, base.Add(-time.Second))
	require.NoError(t, err)
	assert.Empty(t, stale)
}

func TestRedisStorePutRefreshesTheScoreOfAnExistingMember(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := context.Background()
	store := presence.NewRedisStore(client)
	board := uuid.New()
	base := time.UnixMilli(1_900_000_000_000)

	require.NoError(t, store.Put(ctx, board, []string{"u|a"}, base))
	require.NoError(t, store.Put(ctx, board, []string{"u|a"}, base.Add(time.Minute)))
	stale, err := store.Stale(ctx, board, base.Add(30*time.Second))
	require.NoError(t, err)
	assert.Empty(t, stale)
	fresh, err := store.Fresh(ctx, board, base.Add(30*time.Second))
	require.NoError(t, err)
	assert.Equal(t, []string{"u|a"}, fresh)
}

func TestRedisStoreRemoveAndBoardIsolation(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := context.Background()
	store := presence.NewRedisStore(client)
	boardA, boardB := uuid.New(), uuid.New()
	now := time.UnixMilli(1_900_000_000_000)

	require.NoError(t, store.Put(ctx, boardA, []string{"x|a", "y|a"}, now))
	require.NoError(t, store.Put(ctx, boardB, []string{"x|a"}, now))
	require.NoError(t, store.Remove(ctx, boardA, "x|a", "never-existed"))
	require.NoError(t, store.Remove(ctx, boardA))

	freshA, err := store.Fresh(ctx, boardA, now.Add(-time.Second))
	require.NoError(t, err)
	assert.Equal(t, []string{"y|a"}, freshA)
	freshB, err := store.Fresh(ctx, boardB, now.Add(-time.Second))
	require.NoError(t, err)
	assert.Equal(t, []string{"x|a"}, freshB)

	empty, err := store.Fresh(ctx, uuid.New(), now)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestRedisStoreKeysCarryAnExpiry(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := context.Background()
	store := presence.NewRedisStore(client)
	board := uuid.New()
	u := protocol.PresenceUser{ID: uuid.New(), Name: "Ann", AvatarColor: "#112233"}

	require.NoError(t, store.Put(ctx, board, []string{u.ID.String() + "|a"}, time.Now()))
	require.NoError(t, store.SaveProfiles(ctx, board, []protocol.PresenceUser{u}))
	for _, key := range []string{"presence:" + board.String(), "presence:" + board.String() + ":users"} {
		ttl, err := client.TTL(ctx, key).Result()
		require.NoError(t, err)
		assert.Greater(t, ttl, time.Duration(0), key)
		assert.LessOrEqual(t, ttl, presence.DefaultKeyTTL, key)
	}
}

func TestRedisStoreProfiles(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := context.Background()
	store := presence.NewRedisStore(client)
	board := uuid.New()
	ann := protocol.PresenceUser{ID: uuid.New(), Name: "Ann", AvatarColor: "#112233"}
	bob := protocol.PresenceUser{ID: uuid.New(), Name: "Bob", AvatarColor: "#445566"}

	require.NoError(t, store.SaveProfiles(ctx, board, []protocol.PresenceUser{ann, bob}))
	got, err := store.Profiles(ctx, board, []uuid.UUID{ann.ID, bob.ID, uuid.New()})
	require.NoError(t, err)
	assert.ElementsMatch(t, []protocol.PresenceUser{ann, bob}, got, "unknown ids are skipped")

	renamed := ann
	renamed.Name = "Annie"
	require.NoError(t, store.SaveProfiles(ctx, board, []protocol.PresenceUser{renamed}))
	got, err = store.Profiles(ctx, board, []uuid.UUID{ann.ID})
	require.NoError(t, err)
	assert.Equal(t, []protocol.PresenceUser{renamed}, got)

	require.NoError(t, store.DeleteProfiles(ctx, board, ann.ID, uuid.New()))
	got, err = store.Profiles(ctx, board, []uuid.UUID{ann.ID, bob.ID})
	require.NoError(t, err)
	assert.Equal(t, []protocol.PresenceUser{bob}, got)

	other, err := store.Profiles(ctx, uuid.New(), []uuid.UUID{bob.ID})
	require.NoError(t, err)
	assert.Empty(t, other, "profiles are scoped to the board")
}

func TestRedisStoreSweepsADeadInstanceButKeepsTheSameUserOnAnotherInstance(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store := presence.NewRedisStore(client)
	clock := &fakeClock{now: time.Now()}
	rec := &recorder{}
	dead := presence.New(store, rec, clock, "dead")
	alive := presence.New(store, rec, clock, "alive")
	board := uuid.New()
	shared := protocol.PresenceUser{ID: uuid.New(), Name: "Shared", AvatarColor: "#112233"}
	onlyDead := protocol.PresenceUser{ID: uuid.New(), Name: "Dead", AvatarColor: "#445566"}

	require.NoError(t, dead.Join(ctx, board, shared))
	require.NoError(t, dead.Join(ctx, board, onlyDead))
	require.NoError(t, alive.Join(ctx, board, shared))
	assert.Equal(t, []string{protocol.TypePresenceJoin, protocol.TypePresenceJoin}, rec.types())

	clock.Advance(30 * time.Second)
	require.NoError(t, alive.Touch(ctx, board, []protocol.PresenceUser{shared}))
	clock.Advance(30 * time.Second)

	require.NoError(t, alive.Sweep(ctx, board))
	assert.Equal(t, []string{protocol.TypePresenceJoin, protocol.TypePresenceJoin, protocol.TypePresenceLeave}, rec.types())
	online, err := alive.Online(ctx, board)
	require.NoError(t, err)
	assert.Equal(t, []protocol.PresenceUser{shared}, online)

	require.NoError(t, alive.Sweep(ctx, board))
	assert.Len(t, rec.types(), 3, "sweeping twice does not announce twice")
}

func TestRedisServiceOnlineIsSortedByName(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx := context.Background()
	store := presence.NewRedisStore(client)
	clock := &fakeClock{now: time.Now()}
	svc := presence.New(store, &recorder{}, clock, "a")
	board := uuid.New()
	for _, name := range []string{"Zed", "Amy", "Max"} {
		require.NoError(t, svc.Join(ctx, board, protocol.PresenceUser{ID: uuid.New(), Name: name, AvatarColor: "#000000"}))
	}
	online, err := svc.Online(ctx, board)
	require.NoError(t, err)
	names := make([]string, 0, len(online))
	for _, u := range online {
		names = append(names, u.Name)
	}
	assert.Equal(t, []string{"Amy", "Max", "Zed"}, names)
}
