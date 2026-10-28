package realtime

import (
	"time"

	"github.com/Max252t/Cardo-backend/internal/clock"
)

type SystemClock struct {
	clock.System
}

func (SystemClock) NewTicker(d time.Duration) Ticker {
	return systemTicker{time.NewTicker(d)}
}

func (SystemClock) NewTimer(d time.Duration) Timer {
	return systemTimer{time.NewTimer(d)}
}

type systemTicker struct {
	*time.Ticker
}

func (t systemTicker) C() <-chan time.Time {
	return t.Ticker.C
}

type systemTimer struct {
	*time.Timer
}

func (t systemTimer) C() <-chan time.Time {
	return t.Timer.C
}
