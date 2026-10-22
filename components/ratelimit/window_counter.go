package ratelimit

import (
	"sync"
	"time"
)

const (
	DEFAULT_WINDOW = 60
	DEFAULT_LIMIT  = 60
)

// WindowCounter implements a simple window counter
type WindowCounter struct {
	mu           sync.Mutex
	usersCounter map[string]*UserCounter
	window       time.Duration
	limit        int

	// lastSweep is when expired counters were last evicted
	lastSweep time.Time

	// now replaces time.Now in tests
	now func() time.Time
}

type UserCounter struct {
	firstRequest time.Time
	currentCount int
}

// NewWindowCounter returns a limiter that allows limit requests per key every
// windowSeconds. Non-positive values fall back to DEFAULT_WINDOW and DEFAULT_LIMIT.
func NewWindowCounter(windowSeconds, limit int) *WindowCounter {
	if windowSeconds <= 0 {
		windowSeconds = DEFAULT_WINDOW
	}

	if limit <= 0 {
		limit = DEFAULT_LIMIT
	}

	window := time.Duration(windowSeconds) * time.Second

	return &WindowCounter{
		usersCounter: map[string]*UserCounter{},
		window:       window,
		limit:        limit,
	}
}

// Allow charges one request to userID and reports whether it fits in the current window.
func (rl *WindowCounter) Allow(userID string) bool {
	now := rl.clock()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	if now.Sub(rl.lastSweep) >= rl.window {
		rl.sweep(now)
	}

	// A missing or expired counter starts a fresh window
	userCounter, ok := rl.usersCounter[userID]
	if !ok || now.Sub(userCounter.firstRequest) >= rl.window {
		rl.usersCounter[userID] = &UserCounter{firstRequest: now, currentCount: 1}
		return true
	}

	// If the counter is below the limit, increment the counter
	if userCounter.currentCount < rl.limit {
		userCounter.currentCount++
		return true
	}

	return false
}

// Exhausted reports whether userID has spent its limit in the current window
// without charging it. It never creates or extends a counter, so peeking with a
// caller-supplied key cannot grow the map or prolong a lockout.
func (rl *WindowCounter) Exhausted(userID string) bool {
	now := rl.clock()

	rl.mu.Lock()
	defer rl.mu.Unlock()

	userCounter, ok := rl.usersCounter[userID]
	if !ok || now.Sub(userCounter.firstRequest) >= rl.window {
		return false
	}

	return userCounter.currentCount >= rl.limit
}

func (rl *WindowCounter) clock() time.Time {
	if rl.now != nil {
		return rl.now()
	}

	return time.Now()
}

// sweep deletes the counters whose window has expired, at most once per window,
// so memory stays bounded by the keys seen in roughly the last two windows.
// The caller holds mu.
func (rl *WindowCounter) sweep(now time.Time) {
	rl.lastSweep = now

	for userID, userCounter := range rl.usersCounter {
		if now.Sub(userCounter.firstRequest) >= rl.window {
			delete(rl.usersCounter, userID)
		}
	}
}
