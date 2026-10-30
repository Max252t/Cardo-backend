package main

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectorIgnoresSendsWhileNotRecording(t *testing.T) {
	c := newCollector()
	id := uuid.New()
	c.opSent(id, 0, time.Now())
	assert.Zero(t, c.sentOps.Load())
	c.eventReceived(id, time.Now(), 1)
	all, _, _ := c.distributions()
	assert.Zero(t, all.Count)
}

func TestCollectorIgnoresEventsForOperationsItNeverRecorded(t *testing.T) {
	c := newCollector()
	c.recording.Store(true)
	c.eventReceived(uuid.New(), time.Now(), 1)
	all, self, peer := c.distributions()
	assert.Zero(t, all.Count)
	assert.Zero(t, self.Count)
	assert.Zero(t, peer.Count)
	assert.Zero(t, c.delivery(3).MissingEvents)
}

func TestCollectorSplitsLatencyBySenderAndPeers(t *testing.T) {
	c := newCollector()
	c.recording.Store(true)
	id := uuid.New()
	sent := time.Now()
	c.opSent(id, 2, sent)
	c.eventReceived(id, sent.Add(10*time.Millisecond), 2)
	c.eventReceived(id, sent.Add(30*time.Millisecond), 0)
	c.eventReceived(id, sent.Add(50*time.Millisecond), 1)

	all, self, peer := c.distributions()
	assert.Equal(t, 3, all.Count)
	assert.Equal(t, 1, self.Count)
	assert.Equal(t, 2, peer.Count)
	assert.InDelta(t, 10, self.P50Ms, 0.001)
	assert.InDelta(t, 30, peer.MinMs, 0.001)
	assert.InDelta(t, 50, peer.MaxMs, 0.001)
	assert.InDelta(t, 50, all.P99Ms, 0.001)
}

func TestCollectorDeliveryCountsMissingAndNackedOperations(t *testing.T) {
	c := newCollector()
	c.recording.Store(true)
	now := time.Now()
	full, partial, nacked := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{full, partial, nacked} {
		c.opSent(id, 0, now)
	}
	for receiver := range 3 {
		c.eventReceived(full, now.Add(time.Millisecond), receiver)
	}
	c.eventReceived(partial, now.Add(time.Millisecond), 0)
	c.nack(nacked, "not_found")
	c.nack(uuid.New(), "version_conflict")

	d := c.delivery(3)
	assert.Equal(t, int64(3), d.OpsSent)
	assert.Equal(t, 1, d.OpsComplete)
	assert.Equal(t, 1, d.OpsNacked)
	assert.Equal(t, 2, d.MissingEvents)
	assert.Equal(t, map[string]int{"not_found": 1, "version_conflict": 1}, c.nackCounts())
}

func TestCollectorNackCountsAreACopy(t *testing.T) {
	c := newCollector()
	c.nack(uuid.New(), "x")
	counts := c.nackCounts()
	counts["x"] = 100
	assert.Equal(t, 1, c.nackCounts()["x"])
}

func TestCollectorKeepsOnlyTheFirstFiftyFailures(t *testing.T) {
	c := newCollector()
	for i := range 80 {
		c.failure(fmt.Sprintf("failure %d", i))
	}
	list := c.failureList()
	require.Len(t, list, 50)
	assert.Equal(t, "failure 0", list[0])
	list[0] = "changed"
	assert.Equal(t, "failure 0", c.failureList()[0])
}

func TestCollectorTracksTheHighestSequenceUnderConcurrency(t *testing.T) {
	c := newCollector()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				c.noteSeq(int64(w*500 + i))
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(3999), c.maxSeq.Load())
	c.noteSeq(5)
	assert.Equal(t, int64(3999), c.maxSeq.Load())
}

func TestCollectorIsSafeUnderConcurrentSendsAndReceives(t *testing.T) {
	c := newCollector()
	c.recording.Store(true)
	const senders, perSender, receivers = 4, 200, 3
	var wg sync.WaitGroup
	for s := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perSender {
				id := uuid.New()
				now := time.Now()
				c.opSent(id, s, now)
				var inner sync.WaitGroup
				for r := range receivers {
					inner.Add(1)
					go func() {
						defer inner.Done()
						c.eventReceived(id, time.Now(), r)
					}()
				}
				inner.Wait()
			}
		}()
	}
	wg.Wait()
	d := c.delivery(receivers)
	assert.Equal(t, int64(senders*perSender), d.OpsSent)
	assert.Equal(t, senders*perSender, d.OpsComplete)
	assert.Zero(t, d.MissingEvents)
	all, _, _ := c.distributions()
	assert.Equal(t, senders*perSender*receivers, all.Count)
	assert.GreaterOrEqual(t, all.MinMs, 0.0)
}
