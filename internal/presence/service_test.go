package presence_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/presence"
	"github.com/Max252t/Cardo-backend/internal/presencestore"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type published struct {
	Channel string
	Type    string
	Payload json.RawMessage
}

type recorder struct {
	mu   sync.Mutex
	sent []published
}

func (r *recorder) Publish(_ context.Context, channel string, payload []byte) error {
	var env protocol.Envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, published{Channel: channel, Type: env.Type, Payload: env.Payload})
	return nil
}

func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.sent))
	for _, p := range r.sent {
		out = append(out, p.Type)
	}
	return out
}

type fixture struct {
	clock *fakeClock
	store *presencestore.MemoryStore
	rec   *recorder
	a, b  *presence.Service
	board uuid.UUID
}

func newFixture() *fixture {
	clock := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	store := presencestore.NewMemoryStore()
	rec := &recorder{}
	return &fixture{
		clock: clock,
		store: store,
		rec:   rec,
		a:     presence.New(store, rec, clock, "a"),
		b:     presence.New(store, rec, clock, "b"),
		board: uuid.New(),
	}
}

func user(name string) domain.PresenceUser {
	return domain.PresenceUser{ID: uuid.New(), Name: name, AvatarColor: "#112233"}
}

func TestJoinAnnouncesOnlyFirstConnectionAcrossInstances(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := context.Background()
	u := user("Ann")

	require.NoError(t, f.a.Join(ctx, f.board, u))
	require.NoError(t, f.b.Join(ctx, f.board, u))
	assert.Equal(t, []string{protocol.TypePresenceJoin}, f.rec.types())
	assert.Equal(t, protocol.PresenceChannel(f.board), f.rec.sent[0].Channel)

	var join protocol.PresenceJoin
	require.NoError(t, json.Unmarshal(f.rec.sent[0].Payload, &join))
	assert.Equal(t, u, join.User.ToDomain())

	online, err := f.b.Online(ctx, f.board)
	require.NoError(t, err)
	assert.Equal(t, []domain.PresenceUser{u}, online)
}

func TestLeaveAnnouncesOnlyWhenNoInstanceKeepsTheUser(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := context.Background()
	u := user("Ann")
	require.NoError(t, f.a.Join(ctx, f.board, u))
	require.NoError(t, f.b.Join(ctx, f.board, u))

	require.NoError(t, f.a.Leave(ctx, f.board, u.ID))
	assert.Equal(t, []string{protocol.TypePresenceJoin}, f.rec.types())
	online, err := f.a.Online(ctx, f.board)
	require.NoError(t, err)
	assert.Len(t, online, 1)

	require.NoError(t, f.b.Leave(ctx, f.board, u.ID))
	assert.Equal(t, []string{protocol.TypePresenceJoin, protocol.TypePresenceLeave}, f.rec.types())
	var leave protocol.PresenceLeave
	require.NoError(t, json.Unmarshal(f.rec.sent[1].Payload, &leave))
	assert.Equal(t, u.ID, leave.UserID)
	online, err = f.a.Online(ctx, f.board)
	require.NoError(t, err)
	assert.Empty(t, online)
}

func TestTouchKeepsEntriesFreshAndStaleOnesAreSweptWithLeave(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := context.Background()
	alive, gone := user("Alive"), user("Gone")
	require.NoError(t, f.a.Join(ctx, f.board, alive))
	require.NoError(t, f.b.Join(ctx, f.board, gone))

	f.clock.Advance(30 * time.Second)
	require.NoError(t, f.a.Touch(ctx, f.board, []domain.PresenceUser{alive}))
	f.clock.Advance(20 * time.Second)

	online, err := f.a.Online(ctx, f.board)
	require.NoError(t, err)
	assert.Equal(t, []domain.PresenceUser{alive}, online)

	require.NoError(t, f.a.Sweep(ctx, f.board))
	assert.Equal(t, []string{protocol.TypePresenceJoin, protocol.TypePresenceJoin, protocol.TypePresenceLeave}, f.rec.types())
	var leave protocol.PresenceLeave
	require.NoError(t, json.Unmarshal(f.rec.sent[2].Payload, &leave))
	assert.Equal(t, gone.ID, leave.UserID)

	require.NoError(t, f.a.Sweep(ctx, f.board))
	assert.Len(t, f.rec.types(), 3)
}

func TestSweepDoesNotAnnounceUserStillPresentElsewhere(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := context.Background()
	u := user("Ann")
	require.NoError(t, f.a.Join(ctx, f.board, u))
	require.NoError(t, f.b.Join(ctx, f.board, u))

	f.clock.Advance(50 * time.Second)
	require.NoError(t, f.b.Touch(ctx, f.board, []domain.PresenceUser{u}))
	require.NoError(t, f.b.Sweep(ctx, f.board))
	assert.Equal(t, []string{protocol.TypePresenceJoin}, f.rec.types())
}

func TestOnlineIsSortedAndDeduplicated(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := context.Background()
	zed, amy := user("Zed"), user("Amy")
	require.NoError(t, f.a.Join(ctx, f.board, zed))
	require.NoError(t, f.a.Join(ctx, f.board, amy))
	require.NoError(t, f.b.Join(ctx, f.board, amy))

	online, err := f.a.Online(ctx, f.board)
	require.NoError(t, err)
	assert.Equal(t, []domain.PresenceUser{amy, zed}, online)
}

func TestTouchWithoutUsersIsNoop(t *testing.T) {
	t.Parallel()
	f := newFixture()
	require.NoError(t, f.a.Touch(context.Background(), f.board, nil))
}

func TestConcurrentJoinsAndLeavesAcrossInstancesAnnounceAlternately(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := context.Background()
	u := user("Ann")
	instances := []*presence.Service{f.a, f.b, presence.New(f.store, f.rec, f.clock, "c"), presence.New(f.store, f.rec, f.clock, "d")}

	var wg sync.WaitGroup
	for _, svc := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				assert.NoError(t, svc.Join(ctx, f.board, u))
				assert.NoError(t, svc.Leave(ctx, f.board, u.ID))
			}
		}()
	}
	wg.Wait()

	types := f.rec.types()
	require.NotEmpty(t, types)
	for i, typ := range types {
		want := protocol.TypePresenceJoin
		if i%2 == 1 {
			want = protocol.TypePresenceLeave
		}
		require.Equal(t, want, typ, "announcement %d breaks the join/leave alternation", i)
	}
	assert.Equal(t, protocol.TypePresenceLeave, types[len(types)-1])
	online, err := f.a.Online(ctx, f.board)
	require.NoError(t, err)
	assert.Empty(t, online)
}

func TestSweepAnnouncesDepartedUsersOnceWithTheirIDs(t *testing.T) {
	t.Parallel()
	f := newFixture()
	ctx := context.Background()
	ann, bob := user("Ann"), user("Bob")
	require.NoError(t, f.a.Join(ctx, f.board, ann))
	require.NoError(t, f.b.Join(ctx, f.board, bob))
	f.clock.Advance(50 * time.Second)

	require.NoError(t, f.a.Sweep(ctx, f.board))
	require.NoError(t, f.b.Sweep(ctx, f.board))
	types := f.rec.types()
	assert.Equal(t, []string{protocol.TypePresenceJoin, protocol.TypePresenceJoin, protocol.TypePresenceLeave, protocol.TypePresenceLeave}, types)
	left := map[uuid.UUID]bool{}
	for _, p := range f.rec.sent[2:] {
		var leave protocol.PresenceLeave
		require.NoError(t, json.Unmarshal(p.Payload, &leave))
		left[leave.UserID] = true
	}
	assert.Equal(t, map[uuid.UUID]bool{ann.ID: true, bob.ID: true}, left)
	online, err := f.a.Online(ctx, f.board)
	require.NoError(t, err)
	assert.Empty(t, online)
}

func (r *recorder) Joined(ctx context.Context, boardID uuid.UUID, u domain.PresenceUser) error {
	data, err := protocol.Encode(protocol.TypePresenceJoin, protocol.PresenceJoin{User: protocol.FromPresenceUser(u)})
	if err != nil {
		return err
	}
	return r.Publish(ctx, protocol.PresenceChannel(boardID), data)
}

func (r *recorder) Left(ctx context.Context, boardID, userID uuid.UUID) error {
	data, err := protocol.Encode(protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: userID})
	if err != nil {
		return err
	}
	return r.Publish(ctx, protocol.PresenceChannel(boardID), data)
}
