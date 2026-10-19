package ops

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type result struct {
	primary domain.Event
	extra   []domain.Event
}

type applier struct {
	btx   *storage.BoardTx
	actor uuid.UUID
	opID  uuid.UUID
	res   result
}

func (a *applier) authorize(ctx context.Context) error {
	role, err := a.btx.MemberRole(ctx, a.actor)
	if errors.Is(err, domain.ErrNotFound) {
		return forbidden("not a member of this board")
	}
	if err != nil {
		return err
	}
	if !role.AtLeast(domain.RoleEditor) {
		return forbidden("editor role is required")
	}
	return nil
}

func (a *applier) run(ctx context.Context, op protocol.Op) error {
	payload, err := protocol.DecodePayload(op)
	if err != nil {
		return asRejection(err)
	}
	switch p := payload.(type) {
	case *protocol.CardCreate:
		err = a.cardCreate(ctx, p)
	case *protocol.CardEdit:
		err = a.cardEdit(ctx, p)
	case *protocol.CardMove:
		err = a.cardMove(ctx, p)
	case *protocol.CardArchive:
		err = a.cardArchive(ctx, p)
	case *protocol.ColumnCreate:
		err = a.columnCreate(ctx, p)
	case *protocol.ColumnRename:
		err = a.columnRename(ctx, p)
	case *protocol.ColumnMove:
		err = a.columnMove(ctx, p)
	case *protocol.ColumnArchive:
		err = a.columnArchive(ctx, p)
	default:
		return invalid("unsupported operation type")
	}
	return asRejection(err)
}

func (a *applier) emit(ctx context.Context, eventType string, payload any) error {
	ev, err := a.appendEvent(ctx, eventType, payload, &a.opID)
	if err != nil {
		return err
	}
	a.res.primary = ev
	return nil
}

func (a *applier) emitExtra(ctx context.Context, eventType string, payload any) error {
	ev, err := a.appendEvent(ctx, eventType, payload, nil)
	if err != nil {
		return err
	}
	a.res.extra = append(a.res.extra, ev)
	return nil
}

func (a *applier) appendEvent(ctx context.Context, eventType string, payload any, opID *uuid.UUID) (domain.Event, error) {
	return a.btx.AppendEvent(ctx, storage.EventInput{
		Type:    eventType,
		Payload: payload,
		OpID:    opID,
		ActorID: &a.actor,
	})
}

func requireID(field string, id uuid.UUID) error {
	if id == uuid.Nil {
		return invalid(field + ": is required")
	}
	return nil
}

func requireVersion(field string, v *int64) error {
	if v == nil {
		return invalid(field + ": is required")
	}
	if *v < 1 {
		return invalid(field + ": must be positive")
	}
	return nil
}

func requireUnusedID(ctx context.Context, field string, id uuid.UUID, exists func(context.Context, uuid.UUID) (bool, error)) error {
	used, err := exists(ctx, id)
	if err != nil {
		return err
	}
	if used {
		return invalid(field + ": is already in use")
	}
	return nil
}
