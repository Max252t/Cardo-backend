package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type opRecord struct {
	sender int
	sentAt time.Time
	recv   atomic.Int32
	nacked atomic.Bool
}

type collector struct {
	recording atomic.Bool
	ops       sync.Map
	sentOps   atomic.Int64
	maxSeq    atomic.Int64

	mu       sync.Mutex
	all      []time.Duration
	self     []time.Duration
	peer     []time.Duration
	nacks    map[string]int
	failures []string
}

func newCollector() *collector {
	return &collector{nacks: make(map[string]int)}
}

func (c *collector) noteSeq(seq int64) {
	for {
		cur := c.maxSeq.Load()
		if seq <= cur || c.maxSeq.CompareAndSwap(cur, seq) {
			return
		}
	}
}

func (c *collector) opSent(id uuid.UUID, sender int, at time.Time) {
	if !c.recording.Load() {
		return
	}
	c.sentOps.Add(1)
	c.ops.Store(id, &opRecord{sender: sender, sentAt: at})
}

func (c *collector) eventReceived(id uuid.UUID, at time.Time, receiver int) {
	v, ok := c.ops.Load(id)
	if !ok {
		return
	}
	rec := v.(*opRecord)
	rec.recv.Add(1)
	lat := at.Sub(rec.sentAt)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.all = append(c.all, lat)
	if rec.sender == receiver {
		c.self = append(c.self, lat)
		return
	}
	c.peer = append(c.peer, lat)
}

func (c *collector) nack(id uuid.UUID, reason string) {
	if v, ok := c.ops.Load(id); ok {
		v.(*opRecord).nacked.Store(true)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nacks[reason]++
}

func (c *collector) failure(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.failures) < 50 {
		c.failures = append(c.failures, msg)
	}
}

type deliverySummary struct {
	OpsSent       int64
	OpsNacked     int
	OpsComplete   int
	MissingEvents int
}

func (c *collector) delivery(receivers int) deliverySummary {
	var s deliverySummary
	s.OpsSent = c.sentOps.Load()
	c.ops.Range(func(_, v any) bool {
		rec := v.(*opRecord)
		if rec.nacked.Load() {
			s.OpsNacked++
			return true
		}
		got := int(rec.recv.Load())
		if got >= receivers {
			s.OpsComplete++
			return true
		}
		s.MissingEvents += receivers - got
		return true
	})
	return s
}

func (c *collector) distributions() (all, self, peer Dist) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return summarize(c.all), summarize(c.self), summarize(c.peer)
}

func (c *collector) nackCounts() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.nacks))
	for k, v := range c.nacks {
		out[k] = v
	}
	return out
}

func (c *collector) failureList() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.failures...)
}
