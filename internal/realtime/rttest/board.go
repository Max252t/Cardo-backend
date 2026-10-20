package rttest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
)

const eventsPageLimit = 1000

type EventPublisher interface {
	Publish(ctx context.Context, events []domain.Event) error
}

type Board struct {
	ID        uuid.UUID
	publisher EventPublisher
	now       func() time.Time

	mu         sync.Mutex
	title      string
	seq        int64
	oldest     int64
	events     []domain.Event
	members    map[uuid.UUID]domain.Member
	processed  map[uuid.UUID]domain.Event
	deleted    bool
	failNext   error
	applyCount int
}

func NewBoard(id uuid.UUID, publisher EventPublisher) *Board {
	return &Board{
		ID:        id,
		publisher: publisher,
		now:       time.Now,
		title:     "Test board",
		members:   make(map[uuid.UUID]domain.Member),
		processed: make(map[uuid.UUID]domain.Event),
	}
}

func (b *Board) AddMember(userID uuid.UUID, name string, role domain.Role) domain.Member {
	b.mu.Lock()
	defer b.mu.Unlock()
	m := domain.Member{UserID: userID, DisplayName: name, AvatarColor: "#336699", Role: role, JoinedAt: b.now()}
	b.members[userID] = m
	return m
}

func (b *Board) RemoveMember(userID uuid.UUID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.members, userID)
}

func (b *Board) FailNextApply(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failNext = err
}

func (b *Board) Seq() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

func (b *Board) ApplyCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.applyCount
}

func (b *Board) Truncate(upToSeq int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	kept := b.events[:0]
	for _, ev := range b.events {
		if ev.Seq > upToSeq {
			kept = append(kept, ev)
		}
	}
	b.events = kept
	b.oldest = upToSeq
}

func (b *Board) appendLocked(eventType string, payload json.RawMessage, opID *uuid.UUID, actor uuid.UUID) domain.Event {
	b.seq++
	ev := domain.Event{
		BoardID:   b.ID,
		Seq:       b.seq,
		Type:      eventType,
		Payload:   payload,
		OpID:      opID,
		ActorID:   &actor,
		CreatedAt: b.now(),
	}
	b.events = append(b.events, ev)
	return ev
}

func (b *Board) Append(ctx context.Context, eventType string, payload any, actor uuid.UUID) (domain.Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return domain.Event{}, err
	}
	b.mu.Lock()
	ev := b.appendLocked(eventType, data, nil, actor)
	b.mu.Unlock()
	return ev, b.publish(ctx, ev)
}

func (b *Board) Delete(ctx context.Context, actor uuid.UUID) (domain.Event, error) {
	data, err := json.Marshal(protocol.BoardDeleted{ActorID: actor})
	if err != nil {
		return domain.Event{}, err
	}
	b.mu.Lock()
	ev := b.appendLocked(domain.EventBoardDeleted, data, nil, actor)
	b.deleted = true
	b.mu.Unlock()
	return ev, b.publish(ctx, ev)
}

func (b *Board) publish(ctx context.Context, ev domain.Event) error {
	if b.publisher == nil {
		return nil
	}
	return b.publisher.Publish(ctx, []domain.Event{ev})
}

func (b *Board) BoardMember(_ context.Context, boardID, userID uuid.UUID) (domain.Member, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.members[userID]
	if !ok || b.deleted || boardID != b.ID {
		return domain.Member{}, domain.ErrNotFound
	}
	return m, nil
}

func (b *Board) Apply(ctx context.Context, userID, _ uuid.UUID, op protocol.Op) (protocol.OpOutcome, error) {
	b.mu.Lock()
	b.applyCount++
	if err := b.failNext; err != nil {
		b.failNext = nil
		b.mu.Unlock()
		return protocol.OpOutcome{}, err
	}
	m, member := b.members[userID]
	switch {
	case b.deleted || !member:
		b.mu.Unlock()
		return protocol.OpOutcome{}, domain.ErrNotFound
	case !m.Role.AtLeast(domain.RoleEditor):
		nack := protocol.Nack{Reason: protocol.ReasonForbidden, AtSeq: b.seq, Message: "forbidden"}
		b.mu.Unlock()
		return protocol.OpOutcome{Nack: &nack}, nil
	}
	if prev, done := b.processed[op.OpID]; done {
		b.mu.Unlock()
		return protocol.OpOutcome{Event: &prev, Replayed: true}, nil
	}
	opID := op.OpID
	ev := b.appendLocked(strings.TrimPrefix(op.Type, "op."), op.Payload, &opID, userID)
	b.processed[op.OpID] = ev
	b.mu.Unlock()
	return protocol.OpOutcome{Event: &ev}, b.publish(ctx, ev)
}

func (b *Board) Snapshot(_ context.Context, userID, _ uuid.UUID) (domain.BoardSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	m, ok := b.members[userID]
	if !ok || b.deleted {
		return domain.BoardSnapshot{}, domain.ErrNotFound
	}
	members := make([]domain.Member, 0, len(b.members))
	for _, mm := range b.members {
		members = append(members, mm)
	}
	return domain.BoardSnapshot{
		Board:   domain.BoardWithRole{Board: domain.Board{ID: b.ID, Title: b.title, Seq: b.seq}, Role: m.Role},
		Columns: []domain.Column{},
		Cards:   []domain.Card{},
		Members: members,
		Seq:     b.seq,
	}, nil
}

func (b *Board) Events(_ context.Context, userID, _ uuid.UUID, afterSeq int64) (domain.EventPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.members[userID]; !ok || b.deleted {
		return domain.EventPage{}, domain.ErrNotFound
	}
	if afterSeq > b.seq {
		return domain.EventPage{}, domain.NewValidationError("after_seq", "is greater than the current board seq")
	}
	if afterSeq == b.seq {
		return domain.EventPage{Events: []domain.Event{}, Seq: b.seq}, nil
	}
	if afterSeq < b.oldest {
		return domain.EventPage{}, domain.ErrGone
	}
	var out []domain.Event
	for _, ev := range b.events {
		if ev.Seq > afterSeq {
			out = append(out, ev)
		}
	}
	page := domain.EventPage{Events: out, Seq: b.seq}
	if len(out) > eventsPageLimit {
		page.Events = out[:eventsPageLimit]
		page.HasMore = true
	}
	return page, nil
}
