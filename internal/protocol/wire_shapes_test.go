package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func TestSessionMessageShapesAreStable(t *testing.T) {
	t.Parallel()
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	seq := int64(7)
	tests := []struct {
		name    string
		msgType string
		payload any
		want    string
	}{
		{"hello", protocol.TypeHello, protocol.Hello{LastSeq: &seq, Client: "flutter/1.0"}, `{"v":1,"type":"hello","payload":{"last_seq":7,"client":"flutter/1.0"}}`},
		{"hello without seq", protocol.TypeHello, protocol.Hello{}, `{"v":1,"type":"hello","payload":{"last_seq":null}}`},
		{"auth.refreshed", protocol.TypeAuthRefreshed, protocol.AuthRefreshed{ExpiresAt: 1710000000000}, `{"v":1,"type":"auth.refreshed","payload":{"expires_at":1710000000000}}`},
		{"presence.join", protocol.TypePresenceJoin, protocol.PresenceJoin{User: protocol.PresenceUser{ID: id, Name: "Ann", AvatarColor: "#112233"}}, `{"v":1,"type":"presence.join","payload":{"user":{"id":"11111111-1111-1111-1111-111111111111","name":"Ann","avatar_color":"#112233"}}}`},
		{"presence.leave", protocol.TypePresenceLeave, protocol.PresenceLeave{UserID: id}, `{"v":1,"type":"presence.leave","payload":{"user_id":"11111111-1111-1111-1111-111111111111"}}`},
		{"presence.list", protocol.TypePresenceList, protocol.PresenceList{Users: []protocol.PresenceUser{}}, `{"v":1,"type":"presence.list","payload":{"users":[]}}`},
		{"error", protocol.TypeError, protocol.ErrorPayload{Code: protocol.CodeProtocolError, Message: "m"}, `{"v":1,"type":"error","payload":{"code":"protocol_error","message":"m"}}`},
		{"sync.ready", protocol.TypeSyncReady, nil, `{"v":1,"type":"sync.ready"}`},
	}
	for _, tt := range tests {
		data, err := protocol.Encode(tt.msgType, tt.payload)
		require.NoError(t, err, tt.name)
		assert.Equal(t, tt.want, string(data), tt.name)
	}
}

func TestNackAndEventEnvelopeShapesAreStable(t *testing.T) {
	t.Parallel()
	opID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	env, err := protocol.NackEnvelope(opID, boardcontent.Nack{Reason: boardcontent.ReasonForbidden, AtSeq: 3, Message: "no"})
	require.NoError(t, err)
	data, err := json.Marshal(env)
	require.NoError(t, err)
	assert.Equal(t, `{"v":1,"type":"nack","op_id":"44444444-4444-4444-4444-444444444444","payload":{"reason":"forbidden","at_seq":3,"message":"no"}}`, string(data))
}
