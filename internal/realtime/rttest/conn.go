package rttest

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const readTimeout = 5 * time.Second

type Conn struct {
	*websocket.Conn
	t testing.TB
}

func (c *Conn) SendJSON(msgType string, opID *uuid.UUID, payload any) {
	c.t.Helper()
	env, err := protocol.NewEnvelope(msgType, payload)
	if err != nil {
		c.t.Fatalf("build envelope: %v", err)
	}
	env.OpID = opID
	data, err := json.Marshal(env)
	if err != nil {
		c.t.Fatalf("marshal envelope: %v", err)
	}
	c.SendRaw(data)
}

func (c *Conn) SendRaw(data []byte) {
	c.t.Helper()
	if err := c.WriteMessage(websocket.TextMessage, data); err != nil {
		c.t.Fatalf("write websocket message: %v", err)
	}
}

func (c *Conn) Hello(lastSeq *int64) {
	c.t.Helper()
	c.SendJSON(protocol.TypeHello, nil, protocol.Hello{LastSeq: lastSeq, Client: "test/1.0"})
}

func (c *Conn) Next() (protocol.Envelope, error) {
	if err := c.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
		return protocol.Envelope{}, err
	}
	_, data, err := c.ReadMessage()
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	err = json.Unmarshal(data, &env)
	return env, err
}

func (c *Conn) MustNext() protocol.Envelope {
	c.t.Helper()
	env, err := c.Next()
	if err != nil {
		c.t.Fatalf("read websocket message: %v", err)
	}
	return env
}

func (c *Conn) Expect(msgType string) protocol.Envelope {
	c.t.Helper()
	env := c.MustNext()
	if env.Type != msgType {
		c.t.Fatalf("expected %q, got %q (%s)", msgType, env.Type, string(env.Payload))
	}
	return env
}

func (c *Conn) HandshakeSnapshot() protocol.Snapshot {
	c.t.Helper()
	c.Hello(nil)
	env := c.Expect(protocol.TypeSyncSnapshot)
	var snap protocol.Snapshot
	if err := json.Unmarshal(env.Payload, &snap); err != nil {
		c.t.Fatalf("decode snapshot: %v", err)
	}
	c.Expect(protocol.TypePresenceList)
	c.Expect(protocol.TypeSyncReady)
	return snap
}

func (c *Conn) ExpectClose() *websocket.CloseError {
	deadline := time.Now().Add(readTimeout)
	c.t.Helper()
	for {
		_ = c.SetReadDeadline(deadline)
		_, _, err := c.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		if !errors.As(err, &closeErr) {
			c.t.Fatalf("expected close error, got %v", err)
		}
		return closeErr
	}
}
