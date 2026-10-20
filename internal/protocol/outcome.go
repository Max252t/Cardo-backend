package protocol

import "github.com/Max252t/Cardo-backend/internal/domain"

type OpOutcome struct {
	Event    *domain.Event
	Extra    []domain.Event
	Nack     *Nack
	Replayed bool
}

func (o OpOutcome) Events() []domain.Event {
	if o.Event == nil {
		return nil
	}
	events := make([]domain.Event, 0, len(o.Extra)+1)
	events = append(events, o.Extra...)
	return append(events, *o.Event)
}
