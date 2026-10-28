package realtime

const (
	ResyncDelta    = "delta"
	ResyncSnapshot = "snapshot"
)

type Metrics interface {
	ConnectionOpened()
	ConnectionClosed(code int)
	ConnectionRefused(code int)
	Resync(kind string)
	OpRejected(opType, reason string)
}

type nopMetrics struct{}

func (nopMetrics) ConnectionOpened()         {}
func (nopMetrics) ConnectionClosed(int)      {}
func (nopMetrics) ConnectionRefused(int)     {}
func (nopMetrics) Resync(string)             {}
func (nopMetrics) OpRejected(string, string) {}
