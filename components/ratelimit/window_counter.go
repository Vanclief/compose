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

func NewWindowCounter(windowSeconds, limit int) *WindowCounter {
	if windowSeconds == 0 {
		windowSeconds = DEFAULT_WINDOW
	}

	if limit == 0 {
		limit = DEFAULT_LIMIT
	}

	window := time.Duration(windowSeconds) * time.Second

	return &WindowCounter{
		usersCounter: map[string]*UserCounter{},
		window:       window,
		limit:        limit,
	}
}

func (rl *WindowCounter) Allow(userID string) bool {
	now := time.Now()
	if rl.now != nil {
		now = rl.now()
	}

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
