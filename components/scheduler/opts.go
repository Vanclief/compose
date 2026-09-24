package scheduler

import (
	"time"

	"github.com/vanclief/compose/components/logger"
)

const (
	// DefaultShutdownTimeout is how long Start waits for running jobs after its
	// ctx is done, unless WithShutdownTimeout sets another value.
	DefaultShutdownTimeout = 60 * time.Second
	// DefaultJobTimeout is the per-job timeout applied when the job's ctx has no
	// deadline, unless WithJobTimeout sets another value.
	DefaultJobTimeout = time.Hour
)

// Option configures the Scheduler. Pass options to New.
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

// WithShutdownTimeout sets how long Start waits for jobs to drain after its ctx
// is done. d <= 0 is ignored.
func WithShutdownTimeout(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.shutdownTimeout = d
		}
	}
}

// WithJobTimeout sets the per-job timeout applied when the job's ctx has no
// deadline. The timer starts when the job body is about to run, after any
// permit from WithLimit was acquired, so time spent waiting for a permit does
// not count. d <= 0 is ignored.
func WithJobTimeout(d time.Duration) Option {
	return func(s *Scheduler) {
		if d > 0 {
			s.jobTimeout = d
		}
	}
}

// WithLimit caps at n the number of jobs whose id starts with prefix that run
// at the same time, across RunOnce, Supersede, RunNext and recurring jobs. A
// job over the cap waits for a permit, still counts in Active, and gives up
// without running if its context is cancelled while waiting. New rejects an
// empty prefix, n < 1, a prefix given twice, and a prefix that is a prefix of
// another limit's prefix.
func WithLimit(prefix string, n int) Option {
	return func(s *Scheduler) {
		l := limit{prefix: prefix}
		if n > 0 {
			l.permits = make(chan struct{}, n)
		}
		s.limits = append(s.limits, l)
	}
}
