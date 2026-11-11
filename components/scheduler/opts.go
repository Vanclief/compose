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

// WithClaimer makes recurring jobs run on one instance per minute boundary:
// before a due recurring job runs, the Scheduler claims the boundary from c
// and skips the run unless the claim is won (see Claimer). Run every
// instance in the same time zone. Every and Hourly compare only the minute of
// the hour in local time, so zones whose UTC offsets differ by whole hours
// agree, but zones whose offsets differ by a fraction of an hour (+5:30,
// +5:45, -3:30) select different instants for a schedule that fraction does
// not fit, such as Hourly or Every(time.Hour), and each instance then wins its
// own boundaries. Daily compares in its own location, the same on every
// instance unless it is time.Local.
//
// The claim's lease is the job timeout plus one minute (capped at the largest
// Duration), counted from the claim, while the job timeout starts when the
// job body is about to run. A run outlives its lease, and can overlap a run
// of a later boundary on another instance, when the claim's answer, the wait
// for a WithLimit permit and the time the job takes to return once its job
// timeout cancels it add up to more than that minute, when the job ignores
// its context, or when it runs under a Start ctx with a deadline, which
// replaces the job timeout. Nil => no claims, every instance runs every due
// boundary.
func WithClaimer(c Claimer) Option {
	return func(s *Scheduler) {
		s.claimer = c
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
