package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const msgRoleForbidsChanges = "your role does not allow changing this board"

func (c *Client) handle(data []byte) {
	env, err := protocol.ParseEnvelope(data)
	if err != nil {
		c.violation(describeParseError(err))
		return
	}
	switch env.Type {
	case protocol.TypeHello:
		c.onHello(env)
	case protocol.TypeAuthRefresh:
		c.onRefresh(env)
	default:
		if boardcontent.IsOpType(env.Type) {
			c.onOp(env)
			return
		}
		c.violation("unknown message type")
	}
}

func describeParseError(err error) string {
	switch {
	case errors.Is(err, protocol.ErrUnsupportedVersion):
		return "unsupported protocol version"
	case errors.Is(err, protocol.ErrMalformed):
		return "malformed message"
	default:
		return "invalid message"
	}
}

func (c *Client) violation(message string) {
	c.violations++
	c.sendError(protocol.CodeProtocolError, message)
	if c.violations >= c.hub.cfg.MaxProtocolViolations {
		c.closeWith(protocol.CloseProtocolError, "repeated protocol violations")
	}
}

func (c *Client) sendError(code, message string) {
	c.sendMessage(protocol.TypeError, protocol.ErrorPayload{Code: code, Message: message})
}

func (c *Client) sendMessage(msgType string, payload any) {
	data, err := protocol.Encode(msgType, payload)
	if err != nil {
		c.hub.logger.Error("encode websocket message failed", c.logAttrs(slog.String("type", msgType), slog.Any("error", err))...)
		return
	}
	c.reply(data)
}

func (c *Client) sendNack(opID uuid.UUID, nack boardcontent.Nack) {
	env, err := protocol.NackEnvelope(opID, nack)
	if err != nil {
		c.hub.logger.Error("encode nack failed", c.logAttrs(slog.Any("error", err))...)
		return
	}
	c.enqueueJSON("marshal nack failed", env)
}

func (c *Client) localNack(opID uuid.UUID, reason, message string) {
	c.sendNack(opID, boardcontent.Nack{Reason: reason, AtSeq: c.latestSeq.Load(), Message: message})
}

func (c *Client) onHello(env protocol.Envelope) {
	var hello protocol.Hello
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &hello); err != nil {
			c.violation("malformed hello payload")
			return
		}
	}
	if hello.LastSeq != nil && *hello.LastSeq < 0 {
		c.violation("last_seq must not be negative")
		return
	}
	c.mu.Lock()
	if c.state != stateAwaitHello {
		c.mu.Unlock()
		c.violation("hello was already received")
		return
	}
	c.state = stateSyncing
	c.mu.Unlock()
	close(c.helloReceived)
	c.handshakes.Add(1)
	go func() {
		defer c.handshakes.Done()
		c.runHandshake(hello)
	}()
}

func (c *Client) onRefresh(env protocol.Envelope) {
	var req protocol.AuthRefresh
	if err := json.Unmarshal(env.Payload, &req); err != nil || req.AccessToken == "" {
		c.violation("malformed auth.refresh payload")
		return
	}
	claims, err := c.hub.tokens.Parse(req.AccessToken)
	if err != nil {
		c.sendError(protocol.CodeInvalidToken, "access token is invalid or expired")
		return
	}
	if claims.UserID != c.member.UserID {
		c.sendError(protocol.CodeInvalidToken, "access token belongs to another user")
		return
	}
	c.setExpiry(claims.ExpiresAt)
	c.sendMessage(protocol.TypeAuthRefreshed, protocol.AuthRefreshed{ExpiresAt: claims.ExpiresAt.UnixMilli()})
}

func (c *Client) isLive() bool { return c.opsOpen.Load() }

func (c *Client) allowOp() bool {
	return c.limiter.AllowN(c.hub.clock.Now(), 1)
}

func (c *Client) rateAbuse() bool {
	now := c.hub.clock.Now()
	if now.Sub(c.rateWindowStart) > c.hub.cfg.RateAbuseWindow {
		c.rateWindowStart = now
		c.rateStrikes = 0
	}
	c.rateStrikes++
	return c.rateStrikes >= c.hub.cfg.RateAbuseLimit
}

func (c *Client) onOp(env protocol.Envelope) {
	op, err := env.AsOp()
	if err != nil {
		c.violation(describeParseError(err))
		return
	}
	if !c.isLive() {
		c.violation("operations are accepted only after sync.ready")
		return
	}
	if !c.allowOp() {
		c.localNack(op.OpID, boardcontent.ReasonRateLimited, "too many operations, slow down")
		if c.rateAbuse() {
			c.closeWith(protocol.CloseRateLimited, "rate limit exceeded")
		}
		return
	}
	if !c.Role().AtLeast(domain.RoleEditor) {
		c.localNack(op.OpID, boardcontent.ReasonForbidden, msgRoleForbidsChanges)
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.cfg.OpTimeout)
	defer cancel()
	out, err := c.hub.ops.Apply(ctx, c.member.UserID, c.boardID, op)
	c.finishOp(op, out, err)
}

func (c *Client) finishOp(op boardcontent.Op, out boardcontent.OpOutcome, err error) {
	switch {
	case err == nil:
		c.deliverOutcome(op, out)
	case c.ctx.Err() != nil:
	case errors.Is(err, domain.ErrNotFound):
		c.closeWith(protocol.CloseBoardDeleted, "board not found")
	case errors.Is(err, domain.ErrForbidden):
		c.localNack(op.OpID, boardcontent.ReasonForbidden, msgRoleForbidsChanges)
	default:
		var vErr *domain.ValidationError
		if errors.As(err, &vErr) {
			c.localNack(op.OpID, boardcontent.ReasonValidation, vErr.Error())
			return
		}
		c.hub.logger.Error("apply operation failed", c.logAttrs(
			slog.String("op_id", op.OpID.String()),
			slog.String("op_type", op.Type),
			slog.Any("error", err))...)
		c.localNack(op.OpID, boardcontent.ReasonInternal, "temporary server error, retry with the same op_id")
	}
}

func (c *Client) deliverOutcome(op boardcontent.Op, out boardcontent.OpOutcome) {
	switch {
	case out.Nack != nil:
		c.sendNack(op.OpID, *out.Nack)
	case out.Replayed && out.Event != nil:
		c.enqueueJSON("marshal replayed event failed", protocol.EventEnvelope(*out.Event))
	}
}

func (c *Client) runHandshake(hello protocol.Hello) {
	ctx, cancel := context.WithTimeout(c.ctx, c.hub.cfg.SyncTimeout)
	defer cancel()
	sentSeq, err := c.catchUp(ctx, hello.LastSeq)
	if err != nil {
		c.failHandshake(err)
		return
	}
	c.sendPresenceList(ctx)
	ready, err := protocol.Encode(protocol.TypeSyncReady, nil)
	if err != nil {
		c.failHandshake(err)
		return
	}
	if err := c.goLive(ctx, sentSeq, ready); err != nil {
		c.failHandshake(err)
	}
}

func (c *Client) failHandshake(err error) {
	switch {
	case c.ctx.Err() != nil, errors.Is(err, errClientClosing):
	case errors.Is(err, domain.ErrNotFound):
		c.closeWith(protocol.CloseBoardDeleted, "board not found")
	case errors.Is(err, domain.ErrForbidden):
		c.closeWith(protocol.CloseAccessRevoked, "access revoked")
	default:
		c.hub.logger.Error("websocket handshake failed", c.logAttrs(slog.Any("error", err))...)
		c.sendError(boardcontent.ReasonInternal, "synchronization failed, reconnect")
		c.closeWith(websocket.CloseInternalServerErr, "sync failed")
	}
}

func (c *Client) enqueueJSON(failureMsg string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		c.hub.logger.Error(failureMsg, c.logAttrs(slog.Any("error", err))...)
		return
	}
	c.reply(data)
}
