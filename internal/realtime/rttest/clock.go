package rttest

import (
	"sort"
	"sync"
	"time"

	"github.com/Max252t/Cardo-backend/internal/realtime"
)

type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) NewTicker(d time.Duration) realtime.Ticker {
	return &fakeTicker{c.add(d, d)}
}

func (c *FakeClock) NewTimer(d time.Duration) realtime.Timer {
	return c.add(d, 0)
}

func (c *FakeClock) add(d, period time.Duration) *fakeTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, when: c.now.Add(d), period: period, ch: make(chan time.Time, 1), active: true}
	c.timers = append(c.timers, t)
	return t
}

func (c *FakeClock) ActiveTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}
	return n
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var due []*fakeTimer
		for _, t := range c.timers {
			if t.active && !t.when.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			break
		}
		sort.Slice(due, func(i, j int) bool { return due[i].when.Before(due[j].when) })
		t := due[0]
		c.now = t.when
		select {
		case t.ch <- t.when:
		default:
		}
		if t.period > 0 {
			t.when = t.when.Add(t.period)
		} else {
			t.active = false
		}
	}
	c.now = target
	c.timers = c.compact()
	c.mu.Unlock()
}

func (c *FakeClock) compact() []*fakeTimer {
	live := c.timers[:0]
	for _, t := range c.timers {
		if t.active {
			live = append(live, t)
		}
	}
	return live
}

type fakeTimer struct {
	clock  *FakeClock
	when   time.Time
	period time.Duration
	ch     chan time.Time
	active bool
}

func (t *fakeTimer) C() <-chan time.Time {
	return t.ch
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

type fakeTicker struct {
	*fakeTimer
}

func (t *fakeTicker) Stop() {
	t.fakeTimer.Stop()
}
