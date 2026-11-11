package scheduler

import (
	"context"
	"time"
)

const (
	// claimTimeout bounds each Claim and Release call the Scheduler makes.
	claimTimeout = 10 * time.Second

	// leaseMargin is added to the job timeout to make the lease of a claim.
	// The lease starts at the claim, before the job timeout does, and a
	// cancelled job still has to return: the margin covers both.
	leaseMargin = time.Minute
)

// Claimer decides which instance of an app runs a recurring job at a minute
// boundary, so that a job registered with Add on every instance runs once per
// boundary instead of once per instance. Set it with WithClaimer. The claims
// subpackage implements it on PostgreSQL.
//
// The Scheduler claims only recurring runs. At a due boundary it first applies
// its own rule, skipping the boundary while the id is busy in this process
// without claiming. Otherwise it admits the run, which makes the id busy, and
// the run calls Claim before the job body. While the claim is in flight the
// id is busy as for a running job: RunOnce returns false, RunNext and
// Supersede queue behind it, and Active counts the id. Supersede and shutdown
// cancel the context passed to Claim. A store can still answer that the claim
// is won, and the claims subpackage's always answers a claim it has started
// (bounded by the 10 second claim timeout); the job is then not run and the
// claim is released. A won claim runs the job, and Release is called when the
// job returns, after a panic too, and before a pending job of the id runs. A
// refused claim skips the job, and so does a claim error, which is logged at
// error level unless the run was cancelled: missing a boundary is safer than
// every instance running it. After a late wake the Scheduler claims, once,
// the latest boundary of the caught-up range that the job's schedule matches.
// RunOnce, Supersede and RunNext run on the instance they are called on and
// are never claimed.
//
// During shutdown the claim is released when the job returns and its release
// finishes within the shutdown timeout, and the process waits for Start to
// return.
type Claimer interface {
	// Claim reports whether this instance may run job id for the minute
	// boundary tick. It returns false when tick or a later boundary of id was
	// already claimed by any instance, this one included (after its clock
	// stepped back, or a restart on a host whose clock is behind), or when a
	// claimed run of id is still going: not released and its lease not
	// expired. lease is how long a claim blocks later boundaries when its
	// instance never releases it; the Scheduler passes its job timeout
	// (WithJobTimeout) plus one minute, capped at the largest Duration. An
	// error means the store could not answer or refused tick.
	Claim(ctx context.Context, id string, tick time.Time, lease time.Duration) (bool, error)

	// Release ends the claim on tick for id, so it no longer blocks later
	// boundaries. It must not end a claim on any other tick, so a late
	// release cannot free a newer claim.
	Release(ctx context.Context, id string, tick time.Time) error
}
