package realtime

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestBoardGateSerializesTheSameBoardOnly(t *testing.T) {
	t.Parallel()
	var gates boardGates
	first, second := uuid.New(), uuid.New()

	unlock := gates.lock(first)
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		gates.lock(second)()
	}()
	select {
	case <-otherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("a different board must not wait for a held gate")
	}

	sameDone := make(chan struct{})
	go func() {
		defer close(sameDone)
		gates.lock(first)()
	}()
	select {
	case <-sameDone:
		t.Fatal("the same board must wait for the held gate")
	case <-time.After(100 * time.Millisecond):
	}
	unlock()
	select {
	case <-sameDone:
	case <-time.After(5 * time.Second):
		t.Fatal("waiter was not released")
	}
}

func TestBoardGateReleasesBookkeepingWhenIdle(t *testing.T) {
	t.Parallel()
	var gates boardGates
	id := uuid.New()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gates.lock(id)()
		}()
	}
	wg.Wait()
	gates.mu.Lock()
	defer gates.mu.Unlock()
	assert.Empty(t, gates.gates)
}
