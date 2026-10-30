package presence_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/presencestore"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/testutil"
)

func TestMain(m *testing.M) {
	testutil.RunMain(m)
}

func TestRedisStoreBacksTheServiceAcrossInstances(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := presencestore.NewRedisStore(client)
	clock := &fakeClock{now: time.Now()}
	board := uuid.New()
	rec := watchPresence(t, client, board)
	a := presence.New(store, &recorder{}, clock, "a")
	b := presence.New(store, &recorder{}, clock, "b")
	u := domain.PresenceUser{ID: uuid.New(), Name: "Ann", AvatarColor: "#445566"}

	require.NoError(t, a.Join(ctx, board, u))
	require.NoError(t, b.Join(ctx, board, u))
	assert.Equal(t, []string{protocol.TypePresenceJoin}, rec.types())

	online, err := a.Online(ctx, board)
	require.NoError(t, err)
	assert.Equal(t, []domain.PresenceUser{u}, online)

	require.NoError(t, a.Leave(ctx, board, u.ID))
	assert.Len(t, rec.types(), 1)
	require.NoError(t, b.Leave(ctx, board, u.ID))
	assert.Equal(t, []string{protocol.TypePresenceJoin, protocol.TypePresenceLeave}, rec.types())

	online, err = a.Online(ctx, board)
	require.NoError(t, err)
	assert.Empty(t, online)
}

func TestRedisStoreSweepsStaleMembers(t *testing.T) {
	client := testutil.NewRedis(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store := presencestore.NewRedisStore(client)
	clock := &fakeClock{now: time.Now()}
	board := uuid.New()
	rec := watchPresence(t, client, board)
	svc := presence.New(store, &recorder{}, clock, "a")
	u := domain.PresenceUser{ID: uuid.New(), Name: "Ann", AvatarColor: "#445566"}

	require.NoError(t, svc.Join(ctx, board, u))
	clock.Advance(presence.DefaultTTL + time.Second)
	online, err := svc.Online(ctx, board)
	require.NoError(t, err)
	assert.Empty(t, online)

	require.NoError(t, svc.Sweep(ctx, board))
	assert.Equal(t, []string{protocol.TypePresenceJoin, protocol.TypePresenceLeave}, rec.types())
}
