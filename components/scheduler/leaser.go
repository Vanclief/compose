package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"strconv"
	"time"

	"github.com/vanclief/ez"
)

// Defaults of the lease intervals. New copies them into the Scheduler, so
// tests can shorten them.
const (
	// defaultLeaseTTL is how long a lease lasts in the store from an acquire
	// or a renewal, measured on the store's clock.
	defaultLeaseTTL = 30 * time.Second

	// defaultRenewEvery is how often the heartbeat renews the held leases.
	defaultRenewEvery = 2 * time.Second

	// defaultRenewTimeout bounds each Renew call.
	defaultRenewTimeout = 2 * time.Second

	// defaultCancelMargin is how long before its lease could lapse a run that
	// could not renew it is cancelled, so the job has that long to return
	// before the store lets another instance take the id.
	defaultCancelMargin = 10 * time.Second

	// defaultLeasePoll is how often RunNext and Supersede retry an acquire
	// that was refused or failed.
	defaultLeasePoll = time.Second
)

// ErrLeaseLost is the context cause seen by a job that was cancelled because
// the Scheduler could no longer be sure it holds the lease of its id: a
// renewal answered that the lease is no longer this run's, or no renewal
// succeeded for the lease's TTL minus a safety margin.
// errors.Is(context.Cause(ctx), ErrLeaseLost) and
// errors.Is(context.Cause(ctx), context.Canceled) both hold.
var ErrLeaseLost error = ez.New(ez.ECANCELED, "scheduler: lease lost", context.Canceled)

// Lease names one acquisition of an id. Owner is unique per acquisition.
type Lease struct {
	ID    string
	Owner string
	// Superseded is set in Renew's answer when an Acquire with supersede
	// marked the lease.
	Superseded bool
}

// Leaser keeps, across the instances of an app, which run holds each keyed
// job id, so that at most one run of an id is going across all of them and
// Supersede on one instance cancels the run on another. Set it with
// WithLeaser. The claims subpackage implements it on PostgreSQL.
//
// With a Leaser every invocation of the Scheduler, recurring ones included,
// holds the lease of its id while it runs. It acquires the lease after the
// recurring claim of a Claimer, if any, and before the permit of a WithLimit
// option. A RunOnce or recurring run whose acquire is refused or fails is
// skipped; a RunNext or Supersede run retries until it wins, its context ends
// or its job timeout plus the lease's TTL passes, and a Supersede run asks to
// supersede on every attempt. While a run holds its lease
// the Scheduler renews it every 2 seconds for 30 seconds, and cancels the run
// with ErrSuperseded when a renewal answers that the lease was marked
// superseded, and with ErrLeaseLost when a renewal answers that the lease is
// no longer the run's or when no renewal succeeded for 20 seconds. The lease
// is released when the run returns, unless a pending job of the id queued on
// the same instance runs next on it.
type Leaser interface {
	// Acquire takes id for owner when no lease on id is live, or when the live
	// lease is already owner's (a retry of an acquire whose answer was lost;
	// its superseded mark is kept). A fresh take clears the mark. When the
	// acquire is refused and supersede is true, it marks the live lease
	// superseded in the same step. ttl is measured on the store's clock.
	Acquire(ctx context.Context, id, owner string, ttl time.Duration, supersede bool) (bool, error)

	// Renew extends each lease that is still its owner's by ttl from the
	// store's clock and returns those, with Superseded set on the marked
	// ones. A lease missing from the result is lost.
	Renew(ctx context.Context, leases []Lease, ttl time.Duration) ([]Lease, error)

	// Release ends owner's lease on id. It never ends another owner's.
	Release(ctx context.Context, id, owner string) error
}

// hold is a lease this Scheduler holds for a busy id. Its fields other than
// id and owner are guarded by holdsMu.
type hold struct {
	id    string
	owner string

	cancel     context.CancelCauseFunc // cancels the id's current invocation
	timer      *time.Timer             // fires at deadline: the hold is lost
	deadline   time.Time               // last good acquire or renewal send time + TTL - margin
	lost       bool
	superseded bool
}

// newLeaseToken returns the random token that prefixes the owner of every
// lease this Scheduler acquires.
func newLeaseToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error since Go 1.24
	return hex.EncodeToString(b)
}

// leaseWaitFor returns how long a RunNext or Supersede run waits for its
// lease: jobTimeout plus ttl, saturated at the largest Duration.
func leaseWaitFor(jobTimeout, ttl time.Duration) time.Duration {
	if jobTimeout > math.MaxInt64-ttl {
		return math.MaxInt64
	}
	return jobTimeout + ttl
}

// invokeLeased runs job under the lease of id and returns the hold of that
// lease, or nil when none is held. Without a Leaser it is invoke. h is the
// hold kept from the previous invocation of the id, already pointed at this
// invocation; when it is nil the lease is acquired in mode first, and a
// refused or failed acquire skips the job. A job whose ctx is cancelled by
// the time the lease is held is not run.
func (s *Scheduler) invokeLeased(ctx context.Context, id string, cancel context.CancelCauseFunc, job Job, mode admitMode, h *hold) *hold {
	if s.leaser == nil {
		s.invoke(ctx, id, job)
		return nil
	}

	if h == nil {
		if ctx.Err() != nil {
			s.log.Debug().Str("job_id", id).Msg("Scheduler job cancelled before lease")
			return nil
		}
		h = s.acquire(ctx, id, cancel, mode)
		if h == nil {
			return nil
		}
	}

	if ctx.Err() != nil {
		s.log.Debug().Str("job_id", id).Msg("Scheduler job cancelled before it ran")
		return h
	}

	s.invoke(ctx, id, job)
	return h
}

// acquire takes the lease of id for a new owner and registers its hold, or
// returns nil when it is not won. In admitSkip mode it asks once. Otherwise
// it asks every poll interval until it wins, ctx ends, or leaseWaitFor
// passes, and in admitSupersede mode every attempt asks to supersede, so the
// run holding the id is cancelled until this one gets the lease or gives up.
// An acquire that may have committed without its answer reaching here,
// because it failed, is released, best effort, when the run gives up, and a
// wait that ends at its bound releases whether or not an attempt failed. The
// first failed attempt of a wait is logged at warn level and later ones at
// debug level; the error of the attempt made as ctx ends goes on the debug
// line of the cancellation.
func (s *Scheduler) acquire(ctx context.Context, id string, cancel context.CancelCauseFunc, mode admitMode) *hold {
	owner := s.leaseToken + "/" + strconv.FormatUint(s.leaseSeq.Add(1), 10)

	if mode == admitSkip {
		sent := time.Now()
		won, err := s.tryAcquire(ctx, id, owner, false)
		if err != nil && ctx.Err() != nil {
			s.log.Debug().Str("job_id", id).Err(err).Msg("Scheduler job cancelled during lease")
			s.releaseLease(ctx, id, owner)
			return nil
		}
		if err != nil {
			s.log.Error().Str("job_id", id).Err(err).Msg("Scheduler lease failed, skipping")
			s.releaseLease(ctx, id, owner)
			return nil
		}
		if !won {
			s.log.Debug().Str("job_id", id).Msg("Job lease refused, skipping")
			return nil
		}
		return s.addHold(id, owner, cancel, sent)
	}

	supersede := mode == admitSupersede
	failed := false
	bound := time.NewTimer(leaseWaitFor(s.jobTimeout, s.leaseTTL))
	defer bound.Stop()

	for {
		sent := time.Now()
		won, err := s.tryAcquire(ctx, id, owner, supersede)
		if err != nil {
			if ctx.Err() == nil && !failed {
				s.log.Warn().Str("job_id", id).Err(err).Msg("Scheduler lease failed, retrying")
			} else if ctx.Err() == nil {
				s.log.Debug().Str("job_id", id).Err(err).Msg("Scheduler lease failed, retrying")
			}
			failed = true
		} else if won {
			return s.addHold(id, owner, cancel, sent)
		}

		// Checked before the poll timer is armed: with ctx done and the timer
		// due, the select below could pick the timer and ask once more.
		if ctx.Err() != nil {
			s.log.Debug().Str("job_id", id).Err(err).Msg("Scheduler job cancelled while waiting for its lease")
			if failed {
				s.releaseLease(ctx, id, owner)
			}
			return nil
		}

		poll := time.NewTimer(s.leasePoll)
		select {
		case <-poll.C:
		case <-ctx.Done():
			poll.Stop()
			s.log.Debug().Str("job_id", id).Err(err).Msg("Scheduler job cancelled while waiting for its lease")
			if failed {
				s.releaseLease(ctx, id, owner)
			}
			return nil
		case <-bound.C:
			poll.Stop()
			s.log.Error().Str("job_id", id).Msg("Scheduler gave up waiting for the job's lease")
			s.releaseLease(ctx, id, owner)
			return nil
		}
	}
}

// tryAcquire asks the Leaser once for id, bounded by claimTimeout.
func (s *Scheduler) tryAcquire(ctx context.Context, id, owner string, supersede bool) (bool, error) {
	acquireCtx, cancel := context.WithTimeout(ctx, claimTimeout)
	defer cancel()
	return s.leaser.Acquire(acquireCtx, id, owner, s.leaseTTL, supersede)
}

// releaseLease releases owner's lease on id on a context that keeps ctx's
// values but not its cancellation, bounded by claimTimeout. A failure is
// logged: the lease then lapses with its TTL.
func (s *Scheduler) releaseLease(ctx context.Context, id, owner string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
	defer cancel()

	err := s.leaser.Release(releaseCtx, id, owner)
	if err != nil {
		s.log.Error().Str("job_id", id).Err(err).Msg("Scheduler lease release failed")
	}
}

// addHold registers the hold of a lease acquired by a call sent at sent, arms
// its timer, and starts the heartbeat when none is running.
func (s *Scheduler) addHold(id, owner string, cancel context.CancelCauseFunc, sent time.Time) *hold {
	h := &hold{id: id, owner: owner, cancel: cancel}

	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()

	h.deadline = sent.Add(s.leaseTTL - s.cancelMargin)
	h.timer = time.AfterFunc(time.Until(h.deadline), func() {
		s.expireHold(h)
	})
	s.holds[owner] = h
	if !s.heartbeating {
		s.heartbeating = true
		go s.heartbeat()
	}
	return h
}

// expireHold marks h lost and cancels its current invocation with
// ErrLeaseLost, unless h was released or its deadline is still ahead: a
// renewal applied while the timer fired moved it, and reset the timer to fire
// again at the new deadline.
func (s *Scheduler) expireHold(h *hold) {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()

	if s.holds[h.owner] != h {
		return
	}
	if time.Now().Before(h.deadline) {
		return
	}
	h.lost = true
	h.cancel(ErrLeaseLost)
}

// pointHold points h at the next invocation of its id, cancelled with cancel,
// and reports true, unless h is lost or superseded.
func (s *Scheduler) pointHold(h *hold, cancel context.CancelCauseFunc) bool {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()

	if h.lost || h.superseded {
		return false
	}
	h.cancel = cancel
	return true
}

// dropHold unregisters h, stops its timer, and releases its lease in the
// store.
func (s *Scheduler) dropHold(ctx context.Context, h *hold) {
	s.holdsMu.Lock()
	delete(s.holds, h.owner)
	h.timer.Stop()
	s.holdsMu.Unlock()

	s.releaseLease(ctx, h.id, h.owner)
}

// heartbeat renews the held leases every renewEvery and applies each answer
// to the holds that were renewed and are still registered. It exits on a tick
// that finds no hold.
func (s *Scheduler) heartbeat() {
	ticker := time.NewTicker(s.renewEvery)
	defer ticker.Stop()

	for range ticker.C {
		s.holdsMu.Lock()
		if len(s.holds) == 0 {
			s.heartbeating = false
			s.holdsMu.Unlock()
			return
		}
		snapshot := make([]*hold, 0, len(s.holds))
		leases := make([]Lease, 0, len(s.holds))
		for _, h := range s.holds {
			snapshot = append(snapshot, h)
			leases = append(leases, Lease{ID: h.id, Owner: h.owner})
		}
		s.holdsMu.Unlock()

		sent := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), s.renewTimeout)
		renewed, err := s.leaser.Renew(ctx, leases, s.leaseTTL)
		cancel()
		if err != nil {
			s.log.Warn().Int("leases", len(leases)).Err(err).Msg("Scheduler lease renewal failed")
			continue
		}

		s.applyRenewal(snapshot, renewed, sent)
	}
}

// applyRenewal applies the answer of a renewal sent at sent to the holds of
// snapshot that are still registered, matched by owner.
func (s *Scheduler) applyRenewal(snapshot []*hold, renewed []Lease, sent time.Time) {
	byOwner := make(map[string]Lease, len(renewed))
	for _, l := range renewed {
		byOwner[l.Owner] = l
	}

	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()

	for _, h := range snapshot {
		if s.holds[h.owner] != h {
			continue
		}
		l, ok := byOwner[h.owner]
		if !ok {
			h.lost = true
			h.cancel(ErrLeaseLost)
			continue
		}
		if !h.lost {
			h.deadline = sent.Add(s.leaseTTL - s.cancelMargin)
			h.timer.Reset(time.Until(h.deadline))
		}
		if l.Superseded {
			h.superseded = true
			h.cancel(ErrSuperseded)
		}
	}
}
