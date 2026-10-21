package realtime_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/realtime"
	"github.com/Max252t/Cardo-backend/internal/realtime/rttest"
)

func TestSecondProtocolViolationEscalatesToClose4400(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	c.SendRaw([]byte(`not json`))
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	c.SendRaw([]byte(`{"v":1,"type":"nonsense"}`))
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestValidTrafficDoesNotForgiveEarlierViolations(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	c.SendRaw([]byte(`{"v":9,"type":"hello"}`))
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")
	c.SendRaw([]byte(`{"v":1,"type":"hello","payload":{"last_seq":-5}}`))
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
	assert.Equal(t, 1, env.Board.ApplyCount())
}

func TestConfiguredViolationLimitIsHonoured(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{MaxProtocolViolations: 4} })
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	for range 3 {
		c.SendRaw([]byte(`{}`))
		assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	}
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")
	c.SendRaw([]byte(`{}`))
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestNothingIsAppliedAfterTheConnectionIsClosedForViolations(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	c.SendRaw([]byte(`x`))
	c.SendRaw([]byte(`y`))
	for range 5 {
		opID := uuid.New()
		if err := c.WriteMessage(websocket.TextMessage, []byte(`{"v":1,"type":"op.card.create","op_id":"`+opID.String()+`","payload":{}}`)); err != nil {
			break
		}
	}
	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
	assert.Zero(t, env.Board.ApplyCount())
}

func TestViolationKinds(t *testing.T) {
	t.Parallel()
	opID := uuid.NewString()
	tests := []struct {
		name string
		raw  string
	}{
		{"not json", `{`},
		{"json array", `[]`},
		{"json string", `"hello"`},
		{"empty object", `{}`},
		{"missing version", `{"type":"hello"}`},
		{"version zero", `{"v":0,"type":"hello"}`},
		{"future version", `{"v":2,"type":"hello"}`},
		{"version as string", `{"v":"1","type":"hello"}`},
		{"missing type", `{"v":1}`},
		{"empty type", `{"v":1,"type":""}`},
		{"unknown type", `{"v":1,"type":"mystery"}`},
		{"unknown op type", `{"v":1,"type":"op.card.explode","op_id":"` + opID + `"}`},
		{"server only sync.ready", `{"v":1,"type":"sync.ready"}`},
		{"server only sync.snapshot", `{"v":1,"type":"sync.snapshot","payload":{}}`},
		{"server only presence.join", `{"v":1,"type":"presence.join","payload":{}}`},
		{"server only nack", `{"v":1,"type":"nack","op_id":"` + opID + `"}`},
		{"server only board event", `{"v":1,"type":"card.created","seq":5,"payload":{}}`},
		{"op without op_id", `{"v":1,"type":"op.card.archive","payload":{}}`},
		{"op with nil op_id", `{"v":1,"type":"op.card.archive","op_id":"00000000-0000-0000-0000-000000000000","payload":{}}`},
		{"op with invalid op_id", `{"v":1,"type":"op.card.archive","op_id":"nope","payload":{}}`},
		{"auth.refresh without payload", `{"v":1,"type":"auth.refresh"}`},
		{"auth.refresh with empty token", `{"v":1,"type":"auth.refresh","payload":{"access_token":""}}`},
		{"auth.refresh with wrong shape", `{"v":1,"type":"auth.refresh","payload":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{MaxProtocolViolations: 100} })
			c := inst.Dial(t, ann)
			c.HandshakeSnapshot()
			c.SendRaw([]byte(tt.raw))
			msg := c.MustNext()
			assert.Equal(t, protocol.CodeProtocolError, errorCode(t, msg))
			body := decodeAs[protocol.ErrorPayload](t, msg)
			assert.NotEmpty(t, body.Message)
			assert.NotContains(t, body.Message, opID, "error messages never echo client input")
			assert.Zero(t, env.Board.ApplyCount())
		})
	}
}

func TestBinaryFramesAreProtocolViolations(t *testing.T) {
	t.Parallel()
	_, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, []byte{0x01, 0x02}))
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	require.NoError(t, c.WriteMessage(websocket.BinaryMessage, []byte(`{"v":1,"type":"hello"}`)))
	assert.Equal(t, protocol.CodeProtocolError, errorCode(t, c.MustNext()))
	assert.Equal(t, protocol.CloseProtocolError, c.ExpectClose().Code)
}

func paddedRefresh(size int) []byte {
	prefix := `{"v":1,"type":"auth.refresh","payload":{"access_token":"`
	suffix := `"}}`
	return []byte(prefix + strings.Repeat("a", size-len(prefix)-len(suffix)) + suffix)
}

func TestMessageSizeLimitBoundary(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{1024, realtime.DefaultMaxMessageBytes} {
		t.Run("limit", func(t *testing.T) {
			t.Parallel()
			env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{MaxMessageBytes: int64(limit)} })
			c := inst.Dial(t, ann)
			c.HandshakeSnapshot()

			c.SendRaw(paddedRefresh(limit))
			assert.Equal(t, protocol.CodeInvalidToken, errorCode(t, c.MustNext()), "a message of exactly the limit is processed")

			c.SendRaw(paddedRefresh(limit + 1))
			_, err := c.Next()
			var closeErr *websocket.CloseError
			require.ErrorAs(t, err, &closeErr)
			assert.Equal(t, websocket.CloseMessageTooBig, closeErr.Code)
			waitConnections(t, inst, env.Board.ID, 0)
		})
	}
}

func TestDefaultMessageLimitIs64KB(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 64*1024, realtime.DefaultMaxMessageBytes)
}

func TestOversizedMessageDoesNotReachTheProcessor(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, nil)
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	opID := uuid.NewString()
	big := `{"v":1,"type":"op.card.create","op_id":"` + opID + `","payload":{"title":"` + strings.Repeat("x", 70<<10) + `"}}`
	c.SendRaw([]byte(big))
	_, err := c.Next()
	require.Error(t, err)
	waitConnections(t, inst, env.Board.ID, 0)
	assert.Zero(t, env.Board.ApplyCount())
}

func TestRateLimitedOperationsAreNackedWithoutReachingTheProcessor(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{OpsPerSecond: 1, OpsBurst: 1} })
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	first := sendOp(c, protocol.OpCardCreate)
	ev := c.Expect("card.create")
	require.NotNil(t, ev.OpID)
	assert.Equal(t, first, *ev.OpID)

	second := sendOp(c, protocol.OpCardCreate)
	nack := c.MustNext()
	assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, nack))
	require.NotNil(t, nack.OpID)
	assert.Equal(t, second, *nack.OpID)
	body := decodeAs[protocol.Nack](t, nack)
	assert.Equal(t, int64(1), body.AtSeq)
	assert.NotEmpty(t, body.Message)
	assert.Equal(t, 1, env.Board.ApplyCount())
	assert.Equal(t, int64(1), env.Board.Seq())
}

func TestRateLimiterRefillsWithTheClock(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{OpsPerSecond: 2, OpsBurst: 2, RateAbuseLimit: 1000} })
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()

	for range 2 {
		sendOp(c, protocol.OpCardCreate)
		c.Expect("card.create")
	}
	sendOp(c, protocol.OpCardCreate)
	assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()))

	env.Clock.Advance(500 * time.Millisecond)
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")
	sendOp(c, protocol.OpCardCreate)
	assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()))

	env.Clock.Advance(10 * time.Second)
	for range 2 {
		sendOp(c, protocol.OpCardCreate)
		c.Expect("card.create")
	}
	sendOp(c, protocol.OpCardCreate)
	assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()), "the bucket never holds more than the burst")
	assert.Equal(t, 5, env.Board.ApplyCount())
}

func TestSustainedRateLimitAbuseClosesWith4008(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{OpsPerSecond: 1, OpsBurst: 1, RateAbuseLimit: 5, RateAbuseWindow: 10 * time.Second}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")

	for range 5 {
		sendOp(c, protocol.OpCardCreate)
		assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()))
	}
	assert.Equal(t, protocol.CloseRateLimited, c.ExpectClose().Code)
	waitConnections(t, inst, env.Board.ID, 0)
	assert.Equal(t, 1, env.Board.ApplyCount())
}

func TestRateLimitStrikesExpireWithTheAbuseWindow(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{OpsPerSecond: 1, OpsBurst: 1, RateAbuseLimit: 3, RateAbuseWindow: 10 * time.Second}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")

	for range 2 {
		sendOp(c, protocol.OpCardCreate)
		assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()))
	}
	env.Clock.Advance(11 * time.Second)
	sendOp(c, protocol.OpCardCreate)
	c.Expect("card.create")
	for range 2 {
		sendOp(c, protocol.OpCardCreate)
		assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()), "strikes of the previous window are forgotten")
	}
	sendOp(c, protocol.OpCardCreate)
	assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()))
	assert.Equal(t, protocol.CloseRateLimited, c.ExpectClose().Code)
}

func TestRateLimitIsPerConnection(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) { e.Config = realtime.Config{OpsPerSecond: 1, OpsBurst: 1, RateAbuseLimit: 1000} })
	phone := inst.Dial(t, ann)
	phone.HandshakeSnapshot()
	laptop := inst.Dial(t, ann)
	laptop.HandshakeSnapshot()

	awaitOwnEvent(phone, sendOp(phone, protocol.OpCardCreate))
	sendOp(phone, protocol.OpCardCreate)
	assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, drainNack(phone)))

	awaitOwnEvent(laptop, sendOp(laptop, protocol.OpCardCreate))
	assert.Equal(t, 2, env.Board.ApplyCount())
}

func drainNack(c *rttest.Conn) protocol.Envelope {
	return drainUntil(c, protocol.TypeNack)
}

func TestForbiddenOperationsStillConsumeRateLimitTokens(t *testing.T) {
	t.Parallel()
	env := rttest.NewEnv(t)
	env.Config = realtime.Config{OpsPerSecond: 1, OpsBurst: 1, RateAbuseLimit: 1000}
	viewer := rttest.AddUser(env, "Vic", domain.RoleViewer)
	inst := env.StartInstance(t, "a")
	c := inst.Dial(t, viewer)
	c.HandshakeSnapshot()

	sendOp(c, protocol.OpCardCreate)
	assert.Equal(t, protocol.ReasonForbidden, nackReason(t, c.MustNext()))
	sendOp(c, protocol.OpCardCreate)
	assert.Equal(t, protocol.ReasonRateLimited, nackReason(t, c.MustNext()))
	assert.Zero(t, env.Board.ApplyCount())
}

type watched struct {
	c     *rttest.Conn
	pings chan struct{}
	msgs  chan protocol.Envelope
	done  chan error
}

func watch(t testing.TB, c *rttest.Conn, respond bool) *watched {
	t.Helper()
	w := &watched{
		c:     c,
		pings: make(chan struct{}, 64),
		msgs:  make(chan protocol.Envelope, 256),
		done:  make(chan error, 1),
	}
	c.SetPingHandler(func(data string) error {
		w.pings <- struct{}{}
		if !respond {
			return nil
		}
		_ = c.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
		return nil
	})
	go func() {
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				w.done <- err
				return
			}
			var env protocol.Envelope
			if json.Unmarshal(data, &env) == nil {
				w.msgs <- env
			}
		}
	}()
	return w
}

func (w *watched) expectPing(t testing.TB) {
	t.Helper()
	select {
	case <-w.pings:
	case <-time.After(eventually):
		t.Fatal("no ping received")
	}
}

func (w *watched) noPing() bool {
	select {
	case <-w.pings:
		return false
	default:
		return true
	}
}

func (w *watched) barrier(t testing.TB) {
	t.Helper()
	w.c.SendJSON(protocol.TypeAuthRefresh, nil, protocol.AuthRefresh{AccessToken: "barrier"})
	deadline := time.After(eventually)
	for {
		select {
		case env := <-w.msgs:
			if env.Type == protocol.TypeError {
				return
			}
		case err := <-w.done:
			t.Fatalf("connection closed while waiting for the barrier: %v", err)
		case <-deadline:
			t.Fatal("barrier reply not received")
		}
	}
}

func TestPingsAreSentEveryIntervalAndAnsweredClientsStayConnected(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PingInterval: 30 * time.Second, PongWait: 60 * time.Second, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	w := watch(t, c, true)
	waitTimers(t, env, 2)

	env.Clock.Advance(29 * time.Second)
	w.barrier(t)
	assert.True(t, w.noPing(), "no ping before the interval elapsed")

	for range 8 {
		env.Clock.Advance(30 * time.Second)
		w.expectPing(t)
		w.barrier(t)
	}
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
}

func awaitOwnEvent(c *rttest.Conn, opID uuid.UUID) protocol.Envelope {
	for {
		env := c.MustNext()
		if env.OpID != nil && *env.OpID == opID && env.Type != protocol.TypeNack {
			return env
		}
	}
}

func TestAnyClientTrafficCountsAsLivenessEvenWithoutPongs(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PongWait: 600 * time.Millisecond, PingInterval: 1000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	w := watch(t, c, false)

	for range 8 {
		time.Sleep(150 * time.Millisecond)
		w.barrier(t)
	}
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
}

func TestSilentClientIsDroppedAfterThePongWaitWithANormalClose(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PongWait: 300 * time.Millisecond, PingInterval: 1000 * time.Hour, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	w := watch(t, c, false)

	select {
	case err := <-w.done:
		var closeErr *websocket.CloseError
		require.ErrorAs(t, err, &closeErr)
		assert.Equal(t, protocol.CloseNormal, closeErr.Code)
	case <-time.After(eventually):
		t.Fatal("connection was not closed after the pong wait")
	}
	waitConnections(t, inst, env.Board.ID, 0)
}

func TestAnsweredPingsKeepTheConnectionPastThePongWait(t *testing.T) {
	t.Parallel()
	env, ann, inst := newEditorEnv(t, func(e *rttest.Env) {
		e.Config = realtime.Config{PongWait: 400 * time.Millisecond, PingInterval: 100 * time.Millisecond, HelloTimeout: 1000 * time.Hour}
	})
	c := inst.Dial(t, ann)
	c.HandshakeSnapshot()
	w := watch(t, c, true)
	waitTimers(t, env, 2)

	for range 12 {
		env.Clock.Advance(100 * time.Millisecond)
		w.expectPing(t)
		time.Sleep(100 * time.Millisecond)
	}
	assert.Equal(t, 1, inst.Hub.ConnectionCount(env.Board.ID))
}
