package ops

import (
	"errors"

	"github.com/Max252t/Cardo-backend/internal/domain"
	"github.com/Max252t/Cardo-backend/internal/protocol"
	"github.com/Max252t/Cardo-backend/internal/storage"
)

type rejection struct {
	reason    string
	message   string
	transient bool
}

func (r *rejection) Error() string {
	return r.reason + ": " + r.message
}

func reject(reason, message string) error {
	return &rejection{reason: reason, message: message}
}

func notFound(message string) error {
	return reject(protocol.ReasonNotFound, message)
}

func conflict(message string) error {
	return reject(protocol.ReasonVersionConflict, message)
}

func invalid(message string) error {
	return reject(protocol.ReasonValidation, message)
}

func forbidden(message string) error {
	return reject(protocol.ReasonForbidden, message)
}

func notMember() error {
	return &rejection{reason: protocol.ReasonForbidden, message: "not a member of this board", transient: true}
}

func opIDTaken() *rejection {
	return &rejection{reason: protocol.ReasonValidation, message: "op_id was already used by another operation", transient: true}
}

func idUnavailable(field string) error {
	return invalid(field + ": is not available")
}

func asRejection(err error) error {
	var vErr *domain.ValidationError
	if errors.As(err, &vErr) {
		return invalid(vErr.Error())
	}
	if errors.Is(err, storage.ErrDuplicateID) {
		return invalid("id: is not available")
	}
	if errors.Is(err, protocol.ErrUnknownType) {
		return invalid("unknown operation type")
	}
	return err
}

func missingAsNotFound(err error, message string) error {
	if errors.Is(err, domain.ErrNotFound) {
		return notFound(message)
	}
	return err
}
