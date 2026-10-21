package ratelimit

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

type fakeClock struct {
	at time.Time
}

func (c *fakeClock) now() time.Time {
	return c.at
}

func newTestWindowCounter(windowSeconds, limit int) (*WindowCounter, *fakeClock) {
	clock := &fakeClock{at: time.Unix(1_700_000_000, 0)}

	rl := NewWindowCounter(windowSeconds, limit)
	rl.now = clock.now

	return rl, clock
}

func hasEntry(rl *WindowCounter, userID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	_, ok := rl.usersCounter[userID]

	return ok
}

func countEntries(rl *WindowCounter) int {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	return len(rl.usersCounter)
}

func TestWindowCounterAllowsUpToLimit(t *testing.T) {
	rl, _ := newTestWindowCounter(60, 3)

	for range 3 {
		assert.True(t, rl.Allow("user"))
	}
	assert.False(t, rl.Allow("user"))

	// Keys are counted independently
	assert.True(t, rl.Allow("other"))
}

func TestWindowCounterResetsAfterWindow(t *testing.T) {
	rl, clock := newTestWindowCounter(60, 2)

	assert.True(t, rl.Allow("user"))
	assert.True(t, rl.Allow("user"))
	assert.False(t, rl.Allow("user"))

	clock.at = clock.at.Add(59 * time.Second)
	assert.False(t, rl.Allow("user"))

	clock.at = clock.at.Add(time.Second)
	assert.True(t, rl.Allow("user"))
	assert.True(t, rl.Allow("user"))
	assert.False(t, rl.Allow("user"))
}

func TestWindowCounterEvictsExpiredKeys(t *testing.T) {
	rl, clock := newTestWindowCounter(60, 5)

	const keys = 1000
	for i := range keys {
		assert.True(t, rl.Allow("ip-"+strconv.Itoa(i)))
	}
	assert.Equal(t, keys, countEntries(rl))

	// Within the window nothing is evicted
	clock.at = clock.at.Add(30 * time.Second)
	assert.True(t, rl.Allow("recent"))
	assert.Equal(t, keys+1, countEntries(rl))

	// Past the window only the counter that is still live survives, plus the new one
	clock.at = clock.at.Add(31 * time.Second)
	assert.True(t, rl.Allow("fresh"))
	assert.Equal(t, 2, countEntries(rl))

	assert.False(t, hasEntry(rl, "ip-0"))
	assert.True(t, hasEntry(rl, "recent"))
	assert.True(t, hasEntry(rl, "fresh"))
}

func TestWindowCounterConcurrentAllow(t *testing.T) {
	rl := NewWindowCounter(60, 100)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 200 {
				rl.Allow("user-" + strconv.Itoa((i+j)%10))
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, 10, countEntries(rl))
}

func TestWindowCounterConcurrentExpiryKeepsOneQuota(t *testing.T) {
	const limit = 5
	const callers = 50

	rl, clock := newTestWindowCounter(60, limit)

	assert.True(t, rl.Allow("user"))
	clock.at = clock.at.Add(61 * time.Second)

	// Every caller sees the expired counter at the same instant: one of them
	// sweeps it while the others reset or reload it
	var start sync.WaitGroup
	var done sync.WaitGroup
	var allowed atomic.Int32

	start.Add(1)
	for range callers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if rl.Allow("user") {
				allowed.Add(1)
			}
		}()
	}

	start.Done()
	done.Wait()

	assert.Equal(t, int32(limit), allowed.Load())
	assert.Equal(t, 1, countEntries(rl))
}
