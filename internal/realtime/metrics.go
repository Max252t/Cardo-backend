package realtime

const (
	ResyncDelta    = "delta"
	ResyncSnapshot = "snapshot"
	CloseAbnormal  = 1006
)

type Metrics interface {
	ConnectionOpened()
	ConnectionClosed(code int)
	Resync(kind string)
}

type nopMetrics struct{}

func (nopMetrics) ConnectionOpened()    {}
func (nopMetrics) ConnectionClosed(int) {}
func (nopMetrics) Resync(string)        {}
