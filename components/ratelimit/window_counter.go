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
	usersCounter sync.Map
	window       time.Duration
	limit        int
}

type UserCounter struct {
	firstRequest time.Time
	mu           sync.Mutex
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
		window: window,
		limit:  limit,
	}
}

func (rl *WindowCounter) Allow(userID string) bool {
	now := time.Now()

	value, _ := rl.usersCounter.LoadOrStore(userID, &UserCounter{firstRequest: now})
	userCounter := value.(*UserCounter)

	userCounter.mu.Lock()
	defer userCounter.mu.Unlock()

	// If window has passed, reset the counter
	elapsed := now.Sub(userCounter.firstRequest)
	if elapsed >= rl.window {
		userCounter.currentCount = 1
		userCounter.firstRequest = now
		return true
	}

	// If the counter is below the limit, increment the counter
	if userCounter.currentCount < rl.limit {
		userCounter.currentCount++
		return true
	}

	return false
}
