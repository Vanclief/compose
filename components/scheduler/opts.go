package scheduler

import (
	"time"

	"github.com/vanclief/compose/components/logger"
)

const (
	DefaultShutdownTimeout = 60 * time.Second
	DefaultJobTimeout      = time.Hour
)

// Option configures the Scheduler.
type Option func(*Scheduler)

// WithLogger sets the logger. Nil => Noop logger.
func WithLogger(l logger.Logger) Option {
	return func(s *Scheduler) {
		if l == nil {
			s.log = logger.Noop{} // must not be nil; avoids nil deref
			return
		}
		s.log = l
	}
}

// WithShutdownTimeout sets how long Start() waits for jobs to drain after ctx cancel.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.shutdownTimeout = d
		}
	}
}

// WithJobTimeout sets the default per-job timeout if ctx has no deadline.
func WithJobTimeout(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.jobTimeout = d
		}
	}
}
