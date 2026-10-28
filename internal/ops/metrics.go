package ops

import (
	"errors"
	"time"

	"github.com/Max252t/Cardo-backend/internal/boardcontent"
	"github.com/Max252t/Cardo-backend/internal/domain"
)

const (
	ResultApplied  = "applied"
	ResultReplayed = "replayed"
	ResultInternal = "internal"
	TypeUnknown    = "unknown"
)

type Metrics interface {
	ObserveOp(opType, result string, d time.Duration)
}

type nopMetrics struct{}

func (nopMetrics) ObserveOp(string, string, time.Duration) {}

func WithMetrics(m Metrics) Option {
	return func(pr *Processor) {
		if m != nil {
			pr.metrics = m
		}
	}
}

func metricType(opType string) string {
	if boardcontent.IsOpType(opType) {
		return opType
	}
	return TypeUnknown
}

func metricResult(out boardcontent.OpOutcome, err error) string {
	var vErr *domain.ValidationError
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return boardcontent.ReasonNotFound
	case errors.Is(err, domain.ErrForbidden):
		return boardcontent.ReasonForbidden
	case errors.As(err, &vErr):
		return boardcontent.ReasonValidation
	case err != nil:
		return ResultInternal
	case out.Nack != nil:
		return out.Nack.Reason
	case out.Replayed:
		return ResultReplayed
	}
	return ResultApplied
}
