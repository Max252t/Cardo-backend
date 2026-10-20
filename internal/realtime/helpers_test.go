package realtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

const eventually = 5 * time.Second

type syncInner interface {
	Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error)
	Events(ctx context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error)
}

type syncHooks struct {
	inner          syncInner
	beforeSnapshot func() error
	afterSnapshot  func()
	beforeEvents   func(after int64) error
	afterEvents    func()
}

func (s syncHooks) Snapshot(ctx context.Context, userID, boardID uuid.UUID) (domain.BoardSnapshot, error) {
	if s.beforeSnapshot != nil {
		if err := s.beforeSnapshot(); err != nil {
			return domain.BoardSnapshot{}, err
		}
	}
	snap, err := s.inner.Snapshot(ctx, userID, boardID)
	if s.afterSnapshot != nil {
		s.afterSnapshot()
	}
	return snap, err
}

func (s syncHooks) Events(ctx context.Context, userID, boardID uuid.UUID, afterSeq int64) (domain.EventPage, error) {
	if s.beforeEvents != nil {
		if err := s.beforeEvents(afterSeq); err != nil {
			return domain.EventPage{}, err
		}
	}
	page, err := s.inner.Events(ctx, userID, boardID, afterSeq)
	if s.afterEvents != nil {
		s.afterEvents()
	}
	return page, err
}

func appendEvents(t testing.TB, env *rttest.Env, actor uuid.UUID, n int) []int64 {
	t.Helper()
	seqs := make([]int64, 0, n)
	for range n {
		ev, err := env.Board.Append(context.Background(), protocol.EventCardCreated, map[string]string{}, actor)
		require.NoError(t, err)
		seqs = append(seqs, ev.Seq)
	}
	return seqs
}

func decodeAs[T any](t testing.TB, env protocol.Envelope) T {
	t.Helper()
	var v T
	require.NoError(t, json.Unmarshal(env.Payload, &v))
	return v
}

func readEventSeqs(t testing.TB, c *rttest.Conn, eventType string, untilSeq int64) []int64 {
	t.Helper()
	var seqs []int64
	for {
		env := c.MustNext()
		if env.Type != eventType {
			continue
		}
		seqs = append(seqs, env.Seq)
		if env.Seq >= untilSeq {
			return seqs
		}
	}
}

func requireContiguous(t testing.TB, seqs []int64, first, last int64) {
	t.Helper()
	want := make([]int64, 0, last-first+1)
	for s := first; s <= last; s++ {
		want = append(want, s)
	}
	assert.Equal(t, want, seqs)
}

func waitConnections(t testing.TB, inst *rttest.Instance, boardID uuid.UUID, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return inst.Hub.ConnectionCount(boardID) == n }, eventually, 5*time.Millisecond)
}

func waitSubscribed(t testing.TB, inst *rttest.Instance, boardID uuid.UUID, want bool) {
	t.Helper()
	require.Eventually(t, func() bool {
		return inst.Broker.Subscribed(protocol.BoardChannel(boardID)) == want &&
			inst.Broker.Subscribed(protocol.PresenceChannel(boardID)) == want
	}, eventually, 5*time.Millisecond)
}

func errorCode(t testing.TB, env protocol.Envelope) string {
	t.Helper()
	require.Equal(t, protocol.TypeError, env.Type)
	return decodeAs[protocol.ErrorPayload](t, env).Code
}

func nackReason(t testing.TB, env protocol.Envelope) string {
	t.Helper()
	require.Equal(t, protocol.TypeNack, env.Type)
	return decodeAs[protocol.Nack](t, env).Reason
}

func newEditorEnv(t testing.TB, cfg func(*rttest.Env)) (*rttest.Env, uuid.UUID, *rttest.Instance) {
	t.Helper()
	env := rttest.NewEnv(t)
	if cfg != nil {
		cfg(env)
	}
	user := rttest.AddUser(env, "Ann", domain.RoleEditor)
	return env, user, env.StartInstance(t, "a")
}

func barrier(t testing.TB, c *rttest.Conn) {
	t.Helper()
	c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: "barrier"})
	for {
		env := c.MustNext()
		if env.Type == protocol.TypeError && decodeAs[protocol.ErrorPayload](t, env).Code == protocol.CodeInvalidToken {
			return
		}
	}
}
