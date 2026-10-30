package protocol_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"

	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func TestChannels(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	kind, got := protocol.ParseChannel(protocol.BoardChannel(id))
	assert.Equal(t, protocol.ChannelBoard, kind)
	assert.Equal(t, id, got)
	kind, got = protocol.ParseChannel(protocol.PresenceChannel(id))
	assert.Equal(t, protocol.ChannelPresence, kind)
	assert.Equal(t, id, got)
	for _, bad := range []string{"", "board:", "board:x", "other:" + id.String()} {
		kind, _ = protocol.ParseChannel(bad)
		assert.Equal(t, protocol.ChannelUnknown, kind, bad)
	}
}

func TestParseEnvelopeValidation(t *testing.T) {
	t.Parallel()
	_, err := protocol.ParseEnvelope([]byte(`{"v":2,"type":"hello"}`))
	assert.ErrorIs(t, err, protocol.ErrUnsupportedVersion)
	_, err = protocol.ParseEnvelope([]byte(`{"v":1}`))
	assert.ErrorIs(t, err, protocol.ErrMalformed)
	_, err = protocol.ParseEnvelope([]byte(`not json`))
	assert.ErrorIs(t, err, protocol.ErrMalformed)
	env, err := protocol.ParseEnvelope([]byte(`{"v":1,"type":"hello","payload":{"last_seq":null}}`))
	assert.NoError(t, err)
	assert.Equal(t, protocol.TypeHello, env.Type)
}
