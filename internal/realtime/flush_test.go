package realtime_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func TestHandshakeBacklogLargerThanTheSendBufferIsFlushedWithoutDisconnecting(t *testing.T) {
	t.Parallel()
	const backlog = 60
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{SendBuffer: 8}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	env.Sync = gatedSync{
		SyncSource: env.Board,
		afterRead: func() {
			for range backlog {
				_, err := env.Board.Append(context.Background(), boardcontent.EventCardCreated, map[string]string{}, ann)
				assert.NoError(t, err)
			}
			time.Sleep(300 * time.Millisecond)
		},
	}
	inst := env.StartInstance(t, "a")

	c := inst.Dial(t, ann)
	c.Hello(nil)
	c.Expect(protocol.TypeSyncSnapshot)
	c.Expect(protocol.TypePresenceList)
	for want := int64(1); want <= backlog; want++ {
		ev := c.Expect(boardcontent.EventCardCreated)
		require.Equal(t, want, ev.Seq)
	}
	c.Expect(protocol.TypeSyncReady)
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
}

func TestOpSentRightAfterSyncReadyIsAcceptedEvenWithALargeBacklog(t *testing.T) {
	t.Parallel()
	const backlog = 80
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{SendBuffer: 8}
	ann := rttest.AddUser(env, "Ann", domain.RoleEditor)
	env.Sync = gatedSync{
		SyncSource: env.Board,
		afterRead: func() {
			for range backlog {
				_, err := env.Board.Append(context.Background(), boardcontent.EventCardCreated, map[string]string{}, ann)
				assert.NoError(t, err)
			}
			time.Sleep(300 * time.Millisecond)
		},
	}
	inst := env.StartInstance(t, "a")

	c := inst.Dial(t, ann)
	c.Hello(nil)
	c.Expect(protocol.TypeSyncSnapshot)
	c.Expect(protocol.TypePresenceList)
	for {
		msg := c.MustNext()
		if msg.Type == protocol.TypeSyncReady {
			break
		}
		require.Equal(t, boardcontent.EventCardCreated, msg.Type)
	}

	opID := sendOp(c, boardcontent.OpCardCreate)
	var lastSeq int64
	for {
		msg := c.MustNext()
		require.NotEqual(t, protocol.TypeError, msg.Type, string(msg.Payload))
		require.NotEqual(t, protocol.TypeNack, msg.Type, string(msg.Payload))
		if msg.Seq > 0 {
			require.Greater(t, msg.Seq, lastSeq, "events stay ordered")
			lastSeq = msg.Seq
		}
		if msg.OpID != nil && *msg.OpID == opID {
			break
		}
	}
	assert.Equal(t, int64(backlog+1), lastSeq)
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
}
