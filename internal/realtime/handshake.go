package realtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

func (c *Client) catchUp(ctx context.Context, lastSeq *int64) (int64, error) {
	if lastSeq != nil {
		events, ok, err := c.collectDelta(ctx, *lastSeq)
		if err != nil {
			return 0, err
		}
		if ok {
			return c.sendDelta(*lastSeq, events)
		}
	}
	return c.sendSnapshot(ctx)
}

func (c *Client) collectDelta(ctx context.Context, lastSeq int64) ([]domain.Event, bool, error) {
	var events []domain.Event
	after := lastSeq
	for {
		page, err := c.hub.sync.Events(ctx, c.member.UserID, c.boardID, after)
		var vErr *domain.ValidationError
		switch {
		case errors.Is(err, domain.ErrGone), errors.As(err, &vErr):
			return nil, false, nil
		case err != nil:
			return nil, false, fmt.Errorf("load events after %d: %w", after, err)
		}
		events = append(events, page.Events...)
		if len(events) > c.hub.cfg.MaxDeltaEvents {
			return nil, false, nil
		}
		if !page.HasMore {
			return events, true, nil
		}
		if len(page.Events) == 0 {
			return nil, false, nil
		}
		after = page.Events[len(page.Events)-1].Seq
	}
}

func (c *Client) sendDelta(lastSeq int64, events []domain.Event) (int64, error) {
	sentSeq := lastSeq
	chunk := c.hub.cfg.DeltaChunk
	for start := 0; start == 0 || start < len(events); start += chunk {
		end := min(start+chunk, len(events))
		batch := make([]protocol.Envelope, 0, end-start)
		for _, ev := range events[start:end] {
			batch = append(batch, protocol.EventEnvelope(ev))
			sentSeq = max(sentSeq, ev.Seq)
		}
		data, err := protocol.Encode(protocol.TypeSyncDelta, protocol.SyncDelta{Events: batch})
		if err != nil {
			return 0, err
		}
		c.enqueue(data)
	}
	return sentSeq, nil
}

func (c *Client) sendSnapshot(ctx context.Context) (int64, error) {
	snap, err := c.hub.sync.Snapshot(ctx, c.member.UserID, c.boardID)
	if err != nil {
		return 0, fmt.Errorf("load snapshot: %w", err)
	}
	c.setRole(snap.Board.Role)
	data, err := protocol.Encode(protocol.TypeSyncSnapshot, protocol.FromSnapshot(snap))
	if err != nil {
		return 0, err
	}
	c.enqueue(data)
	return snap.Seq, nil
}

func (c *Client) sendPresenceList(ctx context.Context) {
	users, err := c.hub.presence.Online(ctx, c.boardID)
	if err != nil {
		c.hub.logger.Warn("presence list failed", c.logAttrs(slog.Any("error", err))...)
		users = nil
	}
	if users == nil {
		users = []protocol.PresenceUser{}
	}
	c.sendMessage(protocol.TypePresenceList, protocol.PresenceList{Users: users})
}
