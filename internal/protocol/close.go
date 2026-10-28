package protocol

const (
	CloseNormal         = 1000
	CloseGoingAway      = 1001
	CloseWSProtocol     = 1002
	CloseNoStatus       = 1005
	CloseAbnormal       = 1006
	CloseMessageTooBig  = 1009
	CloseInternalError  = 1011
	CloseTryAgainLater  = 1013
	CloseInvalidToken   = 4001
	CloseAccessRevoked  = 4003
	CloseBoardDeleted   = 4004
	CloseRateLimited    = 4008
	CloseSlowClient     = 4009
	CloseProtocolError  = 4400
	closeReasonMaxBytes = 120
)

func CloseReason(reason string) string {
	if len(reason) > closeReasonMaxBytes {
		return reason[:closeReasonMaxBytes]
	}
	return reason
}
