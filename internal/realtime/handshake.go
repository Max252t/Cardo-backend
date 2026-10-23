package realtime

import (
	"context"
	"log/slog"

	"github.com/Max252t/Cardo-backend/internal/boards"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func (c *Client) catchUp(ctx context.Context, lastSeq *int64) (int64, error) {
	res, err := boards.CatchUp(ctx, c.hub.sync, c.member.UserID, c.boardID, lastSeq, boards.CatchUpLimits{MaxEvents: c.hub.cfg.MaxDeltaEvents})
	if err != nil {
		return 0, err
	}
	if res.IsSnapshot() {
		return res.Seq, c.sendSnapshot(*res.Snapshot)
	}
	return res.Seq, c.sendDelta(res.Events)
}

func (c *Client) sendDelta(events []domain.Event) error {
	chunk := c.hub.cfg.DeltaChunk
	for start := 0; start == 0 || start < len(events); start += chunk {
		end := min(start+chunk, len(events))
		batch := make([]protocol.Envelope, 0, end-start)
		for _, ev := range events[start:end] {
			batch = append(batch, protocol.EventEnvelope(ev))
		}
		data, err := protocol.Encode(protocol.TypeSyncDelta, protocol.SyncDelta{Events: batch})
		if err != nil {
			return err
		}
		c.enqueue(data)
	}
	return nil
}

func (c *Client) sendSnapshot(snap domain.BoardSnapshot) error {
	c.setRole(snap.Board.Role)
	data, err := protocol.Encode(protocol.TypeSyncSnapshot, protocol.FromSnapshot(snap))
	if err != nil {
		return err
	}
	c.enqueue(data)
	return nil
}

func (c *Client) sendPresenceList(ctx context.Context) {
	online, err := c.hub.presence.Online(ctx, c.boardID)
	if err != nil {
		c.hub.logger.Warn("presence list failed", c.logAttrs(slog.Any("error", err))...)
	}
	users := make([]protocol.PresenceUser, 0, len(online))
	for _, u := range online {
		users = append(users, protocol.FromPresenceUser(u))
	}
	c.sendMessage(protocol.TypePresenceList, protocol.PresenceList{Users: users})
}
