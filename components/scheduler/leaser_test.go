package scheduler

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/ez"
)

// acquireCall is one Acquire call and its answer.
type acquireCall struct {
	id        string
	owner     string
	supersede bool
	won       bool
	err       error
}

// leaseCall is one Release call.
type leaseCall struct {
	id    string
	owner string
}

type fakeLease struct {
	owner      string
	superseded bool
	lapsed     bool // taken by any owner, as a lease whose TTL passed
}

// fakeLeaser keeps leases in memory for every Scheduler it is given to, as a
// shared store would, without a clock: a lease lasts until it is released,
// dropped or marked lapsed by the test.
type fakeLeaser struct {
	// onAcquire runs before Acquire answers, without the lock; an error it
	// returns is the answer.
	onAcquire func(ctx context.Context, id, owner string, supersede bool) error
	// beforeRenew runs before Renew computes its answer, and afterRenew
	// after it computed it and before it returns it, both without the lock.
	beforeRenew func(ctx context.Context, leases []Lease)
	afterRenew  func(ctx context.Context, leases []Lease)
	// onRelease runs before Release changes anything, without the lock.
	onRelease func(id, owner string)

	mu         sync.Mutex
	leases     map[string]*fakeLease
	acquires   []acquireCall
	releases   []leaseCall
	renewCalls int
	renewErr   error
}

func newFakeLeaser() *fakeLeaser {
	return &fakeLeaser{leases: make(map[string]*fakeLease)}
}

func (f *fakeLeaser) Acquire(ctx context.Context, id, owner string, ttl time.Duration, supersede bool) (bool, error) {
	var err error
	if f.onAcquire != nil {
		err = f.onAcquire(ctx, id, owner, supersede)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	call := acquireCall{id: id, owner: owner, supersede: supersede, err: err}
	if err != nil {
		f.acquires = append(f.acquires, call)
		return false, err
	}

	l := f.leases[id]
	switch {
	case l == nil:
		f.leases[id] = &fakeLease{owner: owner}
		call.won = true
	case l.owner == owner:
		l.lapsed = false
		call.won = true
	case l.lapsed:
		f.leases[id] = &fakeLease{owner: owner}
		call.won = true
	case supersede:
		l.superseded = true
	}
	f.acquires = append(f.acquires, call)
	return call.won, nil
}

func (f *fakeLeaser) Renew(ctx context.Context, leases []Lease, ttl time.Duration) ([]Lease, error) {
	if f.beforeRenew != nil {
		f.beforeRenew(ctx, leases)
	}

	f.mu.Lock()
	f.renewCalls++
	err := f.renewErr
	var renewed []Lease
	for _, l := range leases {
		held := f.leases[l.ID]
		if held != nil && held.owner == l.Owner {
			held.lapsed = false
			renewed = append(renewed, Lease{ID: l.ID, Owner: l.Owner, Superseded: held.superseded})
		}
	}
	f.mu.Unlock()

	if f.afterRenew != nil {
		f.afterRenew(ctx, leases)
	}
	if err != nil {
		return nil, err
	}
	return renewed, nil
}

func (f *fakeLeaser) Release(ctx context.Context, id, owner string) error {
	if f.onRelease != nil {
		f.onRelease(id, owner)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.releases = append(f.releases, leaseCall{id: id, owner: owner})
	l := f.leases[id]
	if l != nil && l.owner == owner {
		delete(f.leases, id)
	}
	return nil
}

// set makes owner hold id.
func (f *fakeLeaser) set(id, owner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases[id] = &fakeLease{owner: owner}
}

// drop deletes the lease of id, as a store that lost it would.
func (f *fakeLeaser) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.leases, id)
}

// holder returns the owner of id's lease, or "".
func (f *fakeLeaser) holder(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := f.leases[id]
	if l == nil {
		return ""
	}
	return l.owner
}

func (f *fakeLeaser) setRenewErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewErr = err
}

func (f *fakeLeaser) getAcquires() []acquireCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acquireCall(nil), f.acquires...)
}

// acquiresBy returns the Acquire calls whose owner starts with s's token.
func (f *fakeLeaser) acquiresBy(s *Scheduler) []acquireCall {
	var calls []acquireCall
	for _, c := range f.getAcquires() {
		if strings.HasPrefix(c.owner, s.leaseToken+"/") {
			calls = append(calls, c)
		}
	}
	return calls
}

func (f *fakeLeaser) getReleases() []leaseCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]leaseCall(nil), f.releases...)
}

func (f *fakeLeaser) getRenewCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewCalls
}

// waitRenewals waits until n more Renew calls than now have been made.
func (f *fakeLeaser) waitRenewals(t *testing.T, n int) {
	t.Helper()
	target := f.getRenewCalls() + n
	require.Eventually(t, func() bool { return f.getRenewCalls() >= target }, waitFor, pollFor)
}

// slow bounds what a test waits for when the code under test waits on a
// timer of its own, so a slow runner under -race has room.
const slow = 10 * time.Second

// recvWithin fails the test when ch does not deliver within d.
func recvWithin[T any](t *testing.T, ch <-chan T, d time.Duration) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(d):
		t.Fatal("timed out waiting on channel")
	}
	var zero T
	return zero
}

// newLeasedScheduler returns a Scheduler with l and short lease intervals:
// renewed every 20ms, with a TTL of 3s, so a run that cannot renew is
// cancelled 2s after its last good renewal, far longer than the 20ms renewal
// interval.
func newLeasedScheduler(t *testing.T, l Leaser, opts ...Option) *Scheduler {
	t.Helper()
	s := newTestScheduler(t, append([]Option{WithLeaser(l)}, opts...)...)
	s.leaseTTL = 3 * time.Second
	s.renewEvery = 20 * time.Millisecond
	s.renewTimeout = 100 * time.Millisecond
	s.cancelMargin = time.Second
	s.leasePoll = 10 * time.Millisecond
	return s
}

// holdOf returns the registered hold of id, or nil.
func holdOf(s *Scheduler, id string) *hold {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	for _, h := range s.holds {
		if h.id == id {
			return h
		}
	}
	return nil
}

// deadlineOf returns h's deadline, read under holdsMu.
func deadlineOf(s *Scheduler, h *hold) time.Time {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	return h.deadline
}

func heartbeating(s *Scheduler) bool {
	s.holdsMu.Lock()
	defer s.holdsMu.Unlock()
	return s.heartbeating
}

// blocker is a job that reports its context when it starts and returns when
// released or when its context ends.
type blocker struct {
	started  chan context.Context
	release  chan struct{}
	returned chan error // context.Cause at return, nil when released
}

func newBlocker() *blocker {
	return &blocker{
		started:  make(chan context.Context, 1),
		release:  make(chan struct{}),
		returned: make(chan error, 1),
	}
}

func (b *blocker) job(ctx context.Context) {
	b.started <- ctx
	select {
	case <-b.release:
		b.returned <- nil
	case <-ctx.Done():
		b.returned <- context.Cause(ctx)
	}
}

func TestWithLeaser(t *testing.T) {
	s := newTestScheduler(t)
	assert.Nil(t, s.leaser)
	assert.Equal(t, defaultLeaseTTL, s.leaseTTL)
	assert.Equal(t, defaultRenewEvery, s.renewEvery)
	assert.Equal(t, defaultRenewTimeout, s.renewTimeout)
	assert.Equal(t, defaultCancelMargin, s.cancelMargin)
	assert.Equal(t, defaultLeasePoll, s.leasePoll)
	assert.Len(t, s.leaseToken, 32)

	s = newTestScheduler(t, WithLeaser(nil))
	assert.Nil(t, s.leaser)

	f := newFakeLeaser()
	other := newTestScheduler(t, WithLeaser(f))
	assert.Equal(t, f, other.leaser)
	assert.NotEqual(t, s.leaseToken, other.leaseToken)
}

func TestLeaseWaitForSaturates(t *testing.T) {
	assert.Equal(t, time.Hour+30*time.Second, leaseWaitFor(time.Hour, 30*time.Second))
	assert.Equal(t, time.Duration(math.MaxInt64), leaseWaitFor(math.MaxInt64, time.Second))
	assert.Equal(t, time.Duration(math.MaxInt64), leaseWaitFor(math.MaxInt64-time.Second+1, time.Second))
	assert.Equal(t, time.Duration(math.MaxInt64), leaseWaitFor(math.MaxInt64-time.Second, time.Second))
}

// TestLeaseOwnersAreUnique: each acquisition has its own owner, made of the
// Scheduler's token and a counter.
func TestLeaseOwnersAreUnique(t *testing.T) {
	f := newFakeLeaser()
	s := newLeasedScheduler(t, f)

	for i := 0; i < 3; i++ {
		ran := make(chan struct{})
		require.True(t, s.RunOnce(context.Background(), "a", func(context.Context) {
			close(ran)
		}))
		recv(t, ran)
		waitIdle(t, s)
	}

	calls := f.acquiresBy(s)
	require.Len(t, calls, 3)
	assert.Equal(t, s.leaseToken+"/1", calls[0].owner)
	assert.Equal(t, s.leaseToken+"/2", calls[1].owner)
	assert.Equal(t, s.leaseToken+"/3", calls[2].owner)
	assert.Equal(t, []leaseCall{{"a", calls[0].owner}, {"a", calls[1].owner}, {"a", calls[2].owner}}, f.getReleases())
}

func TestLeaseRunOnceSkippedWhileHeldElsewhere(t *testing.T) {
	f := newFakeLeaser()
	a := newLeasedScheduler(t, f)
	log, buf := newTestLogger()
	b := newLeasedScheduler(t, f, WithLogger(log))

	first := newBlocker()
	require.True(t, a.RunOnce(context.Background(), "x", first.job))
	recv(t, first.started)

	var skippedRan atomic.Bool
	assert.True(t, b.RunOnce(context.Background(), "x", func(context.Context) {
		skippedRan.Store(true)
	}), "accepted on this instance")
	waitIdle(t, b)
	assert.False(t, skippedRan.Load())
	assert.Len(t, f.acquiresBy(b), 1, "RunOnce asks once")
	assert.Empty(t, f.getReleases(), "a refused acquire is not released")
	assert.Contains(t, buf.String(), `level=DEBUG msg="Job lease refused, skipping" job_id=x`)

	close(first.release)
	waitIdle(t, a)
	assert.Equal(t, "", f.holder("x"))

	ran := make(chan struct{})
	require.True(t, b.RunOnce(context.Background(), "x", func(context.Context) {
		close(ran)
	}))
	recv(t, ran)
	waitIdle(t, b)
}

func TestLeaseRunNextWaitsForRemoteRun(t *testing.T) {
	f := newFakeLeaser()
	a := newLeasedScheduler(t, f)
	b := newLeasedScheduler(t, f)

	first := newBlocker()
	require.True(t, a.RunOnce(context.Background(), "x", first.job))
	recv(t, first.started)

	var firstReturned atomic.Bool
	sawFirstReturned := make(chan bool, 1)
	require.True(t, b.RunNext(context.Background(), "x", func(context.Context) {
		sawFirstReturned <- firstReturned.Load()
	}))

	// b retries on the poll interval while a holds the lease.
	require.Eventually(t, func() bool { return len(f.acquiresBy(b)) >= 3 }, waitFor, pollFor)
	assert.Equal(t, int64(1), b.Active())
	for _, c := range f.acquiresBy(b) {
		assert.False(t, c.won)
		assert.False(t, c.supersede, "RunNext never marks")
	}

	firstReturned.Store(true)
	close(first.release)
	assert.True(t, recv(t, sawFirstReturned))
	waitIdle(t, a)
	waitIdle(t, b)

	calls := f.acquiresBy(b)
	owner := calls[0].owner
	for _, c := range calls {
		assert.Equal(t, owner, c.owner, "the retries of one acquisition share its owner")
	}
}

func TestLeaseSupersedeAcrossInstances(t *testing.T) {
	f := newFakeLeaser()
	a := newLeasedScheduler(t, f)
	b := newLeasedScheduler(t, f)

	// The flag is set as the first job returns, before a can release its
	// lease, so the replacement can only read true.
	first := newBlocker()
	var firstReturned atomic.Bool
	require.True(t, a.RunOnce(context.Background(), "x", func(ctx context.Context) {
		first.job(ctx)
		firstReturned.Store(true)
	}))
	recv(t, first.started)

	sawFirstReturned := make(chan bool, 1)
	require.True(t, b.Supersede(context.Background(), "x", func(context.Context) {
		sawFirstReturned <- firstReturned.Load()
	}))

	cause := recv(t, first.returned)
	assert.True(t, errors.Is(cause, ErrSuperseded), "cause %v", cause)
	assert.True(t, errors.Is(cause, context.Canceled))
	assert.True(t, recv(t, sawFirstReturned))
	waitIdle(t, a)
	waitIdle(t, b)

	// A later run on a, a fresh take of the lease, is not cancelled by the
	// mark that ended the first run.
	second := newBlocker()
	require.True(t, a.RunOnce(context.Background(), "x", second.job))
	secondCtx := recv(t, second.started)
	f.waitRenewals(t, 5)
	assert.NoError(t, secondCtx.Err())
	close(second.release)
	waitIdle(t, a)
}

// TestLeaseFreshTakeOfMarkedLeaseRuns: a lapsed lease that still carries a
// superseded mark is taken fresh, and the run is not cancelled.
func TestLeaseFreshTakeOfMarkedLeaseRuns(t *testing.T) {
	f := newFakeLeaser()
	f.leases["x"] = &fakeLease{owner: "dead/1", superseded: true, lapsed: true}
	s := newLeasedScheduler(t, f)

	run := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "x", run.job))
	ctx := recv(t, run.started)
	f.waitRenewals(t, 5)
	assert.NoError(t, ctx.Err())
	close(run.release)
	waitIdle(t, s)
}

func TestLeaseLostCancelsRun(t *testing.T) {
	f := newFakeLeaser()
	s := newLeasedScheduler(t, f)

	run := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "x", run.job))
	recv(t, run.started)

	f.drop("x")
	cause := recv(t, run.returned)
	assert.True(t, errors.Is(cause, ErrLeaseLost), "cause %v", cause)
	assert.True(t, errors.Is(cause, context.Canceled))
	waitIdle(t, s)
	assert.Len(t, f.getReleases(), 1, "the lost lease is still released, which ends nothing")
}

// TestLeaseUnrenewedCancelsRun: when no renewal succeeds for TTL minus the
// margin, the run is cancelled with ErrLeaseLost, also while Renew hangs.
func TestLeaseUnrenewedCancelsRun(t *testing.T) {
	t.Run("renewals fail", func(t *testing.T) {
		f := newFakeLeaser()
		log, buf := newTestLogger()
		s := newLeasedScheduler(t, f, WithLogger(log))
		s.leaseTTL = 600 * time.Millisecond
		s.cancelMargin = 200 * time.Millisecond
		f.setRenewErr(ez.New(ez.EUNAVAILABLE, "store down", nil))

		run := newBlocker()
		begin := time.Now()
		require.True(t, s.RunOnce(context.Background(), "x", run.job))
		recv(t, run.started)

		cause := recvWithin(t, run.returned, slow)
		assert.True(t, errors.Is(cause, ErrLeaseLost), "cause %v", cause)
		assert.GreaterOrEqual(t, time.Since(begin), 400*time.Millisecond)
		waitIdle(t, s)
		assert.Contains(t, buf.String(), `level=WARN msg="Scheduler lease renewal failed"`)
	})

	t.Run("renew hangs", func(t *testing.T) {
		f := newFakeLeaser()
		hang := make(chan struct{})
		t.Cleanup(func() { close(hang) })
		f.afterRenew = func(context.Context, []Lease) {
			<-hang // ignores its context, as a stuck store call could
		}
		s := newLeasedScheduler(t, f)
		s.leaseTTL = 600 * time.Millisecond
		s.cancelMargin = 200 * time.Millisecond

		run := newBlocker()
		begin := time.Now()
		require.True(t, s.RunOnce(context.Background(), "x", run.job))
		recv(t, run.started)

		cause := recvWithin(t, run.returned, slow)
		assert.True(t, errors.Is(cause, ErrLeaseLost), "cause %v", cause)
		assert.GreaterOrEqual(t, time.Since(begin), 400*time.Millisecond)
		waitIdle(t, s)
	})
}

// TestLeaseExpireHoldChecksDeadline: a timer that fires while a renewal moves
// the hold's deadline does not cancel the run; one whose deadline passed does.
func TestLeaseExpireHoldChecksDeadline(t *testing.T) {
	f := newFakeLeaser()
	// Renewals stop at the first one after paused is set, so none moves the
	// deadline the test sets.
	var paused atomic.Bool
	stuck := make(chan struct{}, 1)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	f.afterRenew = func(context.Context, []Lease) {
		if paused.Load() {
			select {
			case stuck <- struct{}{}:
			default:
			}
			<-gate
		}
	}
	s := newLeasedScheduler(t, f)

	run := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "x", run.job))
	ctx := recv(t, run.started)
	paused.Store(true)
	recv(t, stuck)

	h := holdOf(s, "x")
	require.NotNil(t, h)

	// The deadline is ahead, as after a renewal applied while the timer fired.
	s.holdsMu.Lock()
	h.deadline = time.Now().Add(time.Hour)
	s.holdsMu.Unlock()
	s.expireHold(h)
	assert.NoError(t, ctx.Err())
	s.holdsMu.Lock()
	assert.False(t, h.lost)
	s.holdsMu.Unlock()

	// The deadline passed.
	s.holdsMu.Lock()
	h.deadline = time.Now().Add(-time.Millisecond)
	s.holdsMu.Unlock()
	s.expireHold(h)
	cause := recv(t, run.returned)
	assert.True(t, errors.Is(cause, ErrLeaseLost), "cause %v", cause)
	s.holdsMu.Lock()
	assert.True(t, h.lost)
	s.holdsMu.Unlock()
	waitIdle(t, s)
}

// TestLeaseRenewalsKeepRunAlive: while renewals succeed, a run outlives its
// TTL minus the margin more than twice over, and when they stop after that it
// is still cancelled with ErrLeaseLost. The window is 700ms, 35 renewal
// intervals, so a renewal succeeding within it is the only timing assumption.
func TestLeaseRenewalsKeepRunAlive(t *testing.T) {
	for _, stop := range []bool{false, true} {
		name := "released"
		if stop {
			name = "renewals stop"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeLeaser()
			s := newLeasedScheduler(t, f)
			s.leaseTTL = time.Second
			s.cancelMargin = 300 * time.Millisecond

			run := newBlocker()
			require.True(t, s.RunOnce(context.Background(), "x", run.job))
			ctx := recv(t, run.started)

			select {
			case <-ctx.Done():
				t.Fatalf("run cancelled while renewals succeed: %v", context.Cause(ctx))
			case <-time.After(1500 * time.Millisecond):
			}

			if stop {
				// Each renewal reset the timer, so it fires at the deadline of
				// the last one. Left alone it would have fired once, at the
				// first deadline, found the deadline moved, and never again.
				f.setRenewErr(ez.New(ez.EUNAVAILABLE, "store down", nil))
				cause := recvWithin(t, run.returned, slow)
				assert.True(t, errors.Is(cause, ErrLeaseLost), "cause %v", cause)
			} else {
				close(run.release)
				assert.NoError(t, recv(t, run.returned))
			}
			waitIdle(t, s)
		})
	}
}

// TestLeaseDeadlineCountsFromSend: a hold's deadline is TTL minus the margin
// from when its acquire, and then its renewal, was sent, not from when the
// answer arrived. The lease times are far longer than the test, so no timer
// fires; only the first renewal is answered, so no later one moves the
// deadline before it is read.
func TestLeaseDeadlineCountsFromSend(t *testing.T) {
	f := newFakeLeaser()
	entered := make(chan time.Time, 1)
	answer := make(chan struct{})
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	var renewals atomic.Int64
	f.beforeRenew = func(context.Context, []Lease) {
		if renewals.Add(1) > 1 {
			<-gate
			return
		}
		entered <- time.Now()
		<-answer
	}
	s := newLeasedScheduler(t, f)
	s.leaseTTL = time.Hour
	s.cancelMargin = 30 * time.Minute
	window := s.leaseTTL - s.cancelMargin

	run := newBlocker()
	before := time.Now()
	require.True(t, s.RunOnce(context.Background(), "x", run.job))
	recv(t, run.started)
	after := time.Now()

	h := holdOf(s, "x")
	require.NotNil(t, h)
	initial := deadlineOf(s, h)
	assert.WithinRange(t, initial, before.Add(window), after.Add(window), "the acquire's deadline")

	// The answer of the first renewal arrives 50ms after the call was made.
	sent := recv(t, entered)
	time.Sleep(50 * time.Millisecond)
	close(answer)

	require.Eventually(t, func() bool { return deadlineOf(s, h).After(initial) }, waitFor, pollFor, "the renewal did not move the deadline")
	renewed := deadlineOf(s, h)
	assert.False(t, renewed.After(sent.Add(window)), "the renewal's deadline %v counts from after the call %v", renewed, sent)

	close(run.release)
	waitIdle(t, s)
}

// TestLeaseKeptForLocalPendingJob: a pending job promoted after a healthy
// run runs on the same lease, without a release or an acquire in between,
// also when a local Supersede cancelled the run.
func TestLeaseKeptForLocalPendingJob(t *testing.T) {
	for _, supersede := range []bool{false, true} {
		name := "RunNext"
		if supersede {
			name = "Supersede"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeLeaser()
			s := newLeasedScheduler(t, f)

			first := newBlocker()
			require.True(t, s.RunOnce(context.Background(), "x", first.job))
			recv(t, first.started)

			type seen struct {
				acquires int
				releases int
				holder   string
			}
			atNext := make(chan seen, 1)
			next := func(context.Context) {
				atNext <- seen{len(f.getAcquires()), len(f.getReleases()), f.holder("x")}
			}
			if supersede {
				require.True(t, s.Supersede(context.Background(), "x", next))
				assert.True(t, errors.Is(recv(t, first.returned), ErrSuperseded))
			} else {
				require.True(t, s.RunNext(context.Background(), "x", next))
				close(first.release)
			}

			got := recv(t, atNext)
			waitIdle(t, s)
			owner := f.getAcquires()[0].owner
			assert.Equal(t, seen{acquires: 1, releases: 0, holder: owner}, got)
			assert.Equal(t, []leaseCall{{"x", owner}}, f.getReleases())
		})
	}
}

// TestLeaseKeptLeasePendingJobCancellable: a pending job promoted onto the
// lease of the run before it is the invocation a later Supersede or shutdown
// cancels.
func TestLeaseKeptLeasePendingJobCancellable(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		name := "Supersede"
		if shutdown {
			name = "shutdown"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeLeaser()
			s := newLeasedScheduler(t, f, WithShutdownTimeout(5*time.Second))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			startReturned := make(chan struct{})
			go func() {
				s.Start(ctx)
				close(startReturned)
			}()

			first := newBlocker()
			require.True(t, s.RunOnce(context.Background(), "x", first.job))
			recv(t, first.started)
			second := newBlocker()
			require.True(t, s.RunNext(context.Background(), "x", second.job))
			close(first.release)
			require.NoError(t, recv(t, first.returned))
			recv(t, second.started)
			require.Len(t, f.getAcquires(), 1, "the pending job runs on the first run's lease")

			if shutdown {
				cancel()
				assert.Equal(t, context.Canceled, recv(t, second.returned))
				recv(t, startReturned)
				assert.Equal(t, int64(0), s.Active())
				return
			}

			require.True(t, s.Supersede(context.Background(), "x", func(context.Context) {}))
			cause := recv(t, second.returned)
			assert.True(t, errors.Is(cause, ErrSuperseded), "cause %v", cause)
			waitIdle(t, s)
			cancel()
			recv(t, startReturned)
		})
	}
}

// TestLeaseReacquiredAfterLossOrRemoteSupersede: a pending job promoted
// after a run whose lease was lost or marked superseded runs on a lease
// acquired afresh, after the old one was released.
func TestLeaseReacquiredAfterLossOrRemoteSupersede(t *testing.T) {
	t.Run("lost", func(t *testing.T) {
		f := newFakeLeaser()
		s := newLeasedScheduler(t, f)

		first := newBlocker()
		require.True(t, s.RunOnce(context.Background(), "x", first.job))
		recv(t, first.started)

		releasesAtNext := make(chan int, 1)
		require.True(t, s.RunNext(context.Background(), "x", func(context.Context) {
			releasesAtNext <- len(f.getReleases())
		}))

		f.drop("x")
		assert.True(t, errors.Is(recv(t, first.returned), ErrLeaseLost))
		assert.Equal(t, 1, recv(t, releasesAtNext))
		waitIdle(t, s)

		calls := f.acquiresBy(s)
		require.Len(t, calls, 2)
		assert.NotEqual(t, calls[0].owner, calls[1].owner)
		assert.False(t, calls[1].supersede, "RunNext's own mode")
	})

	t.Run("superseded from another instance", func(t *testing.T) {
		f := newFakeLeaser()
		a := newLeasedScheduler(t, f)
		b := newLeasedScheduler(t, f)

		first := newBlocker()
		require.True(t, a.RunOnce(context.Background(), "x", first.job))
		recv(t, first.started)

		releasesAtNext := make(chan int, 1)
		require.True(t, a.RunNext(context.Background(), "x", func(context.Context) {
			releasesAtNext <- len(f.getReleases())
		}))
		remoteRan := make(chan struct{})
		require.True(t, b.Supersede(context.Background(), "x", func(context.Context) {
			close(remoteRan)
		}))

		assert.True(t, errors.Is(recv(t, first.returned), ErrSuperseded))
		assert.GreaterOrEqual(t, recv(t, releasesAtNext), 1)
		recv(t, remoteRan)
		waitIdle(t, a)
		waitIdle(t, b)

		calls := f.acquiresBy(a)
		require.GreaterOrEqual(t, len(calls), 2)
		assert.NotEqual(t, calls[0].owner, calls[len(calls)-1].owner)
		assert.True(t, calls[len(calls)-1].won)
	})
}

// TestLeaseReleasedBeforeIDIsIdle: the lease is released while the id is
// still busy, so a job admitted during the release queues behind it and
// acquires a lease of its own that the release does not end.
func TestLeaseReleasedBeforeIDIsIdle(t *testing.T) {
	f := newFakeLeaser()
	s := newLeasedScheduler(t, f)

	type atRelease struct {
		active  int64
		runOnce bool
		runNext bool
	}
	seen := make(chan atRelease, 1)
	next := newBlocker()
	var armed atomic.Bool
	armed.Store(true)
	f.onRelease = func(id, owner string) {
		if !armed.CompareAndSwap(true, false) {
			return
		}
		seen <- atRelease{
			active:  s.Active(),
			runOnce: s.RunOnce(context.Background(), "x", func(context.Context) {}),
			runNext: s.RunNext(context.Background(), "x", next.job),
		}
	}

	require.True(t, s.RunOnce(context.Background(), "x", func(context.Context) {}))
	assert.Equal(t, atRelease{active: 1, runOnce: false, runNext: true}, recv(t, seen))

	recv(t, next.started)
	calls := f.acquiresBy(s)
	require.Len(t, calls, 2)
	assert.Equal(t, calls[1].owner, f.holder("x"), "the old release left the new lease alone")
	f.waitRenewals(t, 3)
	assert.Equal(t, calls[1].owner, f.holder("x"))

	close(next.release)
	waitIdle(t, s)
}

// TestLeaseStaleRenewAnswerIgnored: a Renew answer computed before a lease
// was released and delivered after the id was acquired again neither cancels
// the new run nor extends its lease.
func TestLeaseStaleRenewAnswerIgnored(t *testing.T) {
	for _, lostInAnswer := range []bool{true, false} {
		name := "answer says renewed"
		if lostInAnswer {
			name = "answer says lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeLeaser()
			s := newLeasedScheduler(t, f)
			// Long enough that no timer fires during the test.
			s.leaseTTL = 10 * time.Second
			s.cancelMargin = time.Second

			// The first renewal, made while the first run holds the lease,
			// computes its answer, then waits for gate. The second, the
			// first one after the stale answer was applied, reports the
			// deadline of the second run's hold.
			var calls atomic.Int64
			computed := make(chan struct{})
			gate := make(chan struct{})
			var second atomic.Pointer[hold]
			deadlineAfter := make(chan time.Time, 1)
			f.beforeRenew = func(context.Context, []Lease) {
				if calls.Add(1) == 1 && lostInAnswer {
					f.drop("x")
				}
			}
			f.afterRenew = func(context.Context, []Lease) {
				switch calls.Load() {
				case 1:
					close(computed)
					<-gate
				case 2:
					h := second.Load()
					s.holdsMu.Lock()
					deadlineAfter <- h.deadline
					s.holdsMu.Unlock()
				}
			}

			first := newBlocker()
			require.True(t, s.RunOnce(context.Background(), "x", first.job))
			recv(t, first.started)
			firstOwner := f.getAcquires()[0].owner
			recv(t, computed)

			close(first.release)
			require.NoError(t, recv(t, first.returned))
			waitIdle(t, s)

			run := newBlocker()
			require.True(t, s.RunOnce(context.Background(), "x", run.job))
			runCtx := recv(t, run.started)
			require.NotEqual(t, firstOwner, f.holder("x"))
			h := holdOf(s, "x")
			require.NotNil(t, h)
			s.holdsMu.Lock()
			deadline := h.deadline
			s.holdsMu.Unlock()
			second.Store(h)

			close(gate)
			assert.Equal(t, deadline, recv(t, deadlineAfter), "the stale answer moved the new lease's deadline")
			f.waitRenewals(t, 2)
			assert.NoError(t, runCtx.Err(), "the stale answer cancelled the new run")
			close(run.release)
			waitIdle(t, s)
		})
	}
}

func TestLeaseAcquireErrorRunOnce(t *testing.T) {
	t.Run("error skips, logs and releases", func(t *testing.T) {
		f := newFakeLeaser()
		f.onAcquire = func(_ context.Context, id, owner string, _ bool) error {
			// The statement committed before its answer was lost.
			f.set(id, owner)
			return ez.New(ez.EUNAVAILABLE, "store down", nil)
		}
		log, buf := newTestLogger()
		s := newLeasedScheduler(t, f, WithLogger(log))

		var ran atomic.Bool
		require.True(t, s.RunOnce(context.Background(), "x", func(context.Context) {
			ran.Store(true)
		}))
		waitIdle(t, s)

		assert.False(t, ran.Load())
		calls := f.getAcquires()
		require.Len(t, calls, 1)
		assert.Equal(t, []leaseCall{{"x", calls[0].owner}}, f.getReleases())
		assert.Equal(t, "", f.holder("x"), "the committed acquire was released")
		assert.Contains(t, buf.String(), `level=ERROR msg="Scheduler lease failed, skipping" job_id=x`)
		assert.Contains(t, buf.String(), "store down")
	})

	t.Run("error after cancellation is not an error log", func(t *testing.T) {
		f := newFakeLeaser()
		log, buf := newTestLogger()
		s := newLeasedScheduler(t, f, WithLogger(log))

		replacementRan := make(chan struct{})
		var calls atomic.Int64
		f.onAcquire = func(ctx context.Context, _, _ string, _ bool) error {
			if calls.Add(1) > 1 {
				return nil
			}
			s.Supersede(context.Background(), "x", func(context.Context) {
				close(replacementRan)
			})
			<-ctx.Done()
			return ctx.Err()
		}

		var ran atomic.Bool
		require.True(t, s.RunOnce(context.Background(), "x", func(context.Context) {
			ran.Store(true)
		}))
		recv(t, replacementRan)
		waitIdle(t, s)

		assert.False(t, ran.Load())
		acquires := f.getAcquires()
		require.Len(t, acquires, 2)
		assert.True(t, acquires[1].supersede, "the replacement acquires in its own mode")
		assert.Contains(t, f.getReleases(), leaseCall{"x", acquires[0].owner})
		assert.Contains(t, buf.String(), `level=DEBUG msg="Scheduler job cancelled during lease" job_id=x`)
		assert.NotContains(t, buf.String(), "level=ERROR")
	})
}

// TestLeaseWonAfterCancel: a store that answers an acquire after the job's
// context was cancelled can still grant the lease. The job does not run; a
// job queued meanwhile runs on that lease, and the lease is released after
// the last run.
func TestLeaseWonAfterCancel(t *testing.T) {
	t.Run("a local Supersede runs on the lease", func(t *testing.T) {
		f := newFakeLeaser()
		s := newLeasedScheduler(t, f)

		replacementRan := make(chan struct{})
		var calls atomic.Int64
		f.onAcquire = func(ctx context.Context, _, _ string, _ bool) error {
			if calls.Add(1) == 1 {
				s.Supersede(context.Background(), "x", func(context.Context) {
					close(replacementRan)
				})
				<-ctx.Done()
			}
			return nil // won, as a store that does not stop on cancellation answers
		}

		var firstRan atomic.Bool
		require.True(t, s.RunOnce(context.Background(), "x", func(context.Context) {
			firstRan.Store(true)
		}))
		recv(t, replacementRan)
		waitIdle(t, s)

		assert.False(t, firstRan.Load())
		acquires := f.getAcquires()
		require.Len(t, acquires, 1, "the replacement ran on the lease the first acquire won")
		assert.True(t, acquires[0].won)
		assert.Equal(t, []leaseCall{{"x", acquires[0].owner}}, f.getReleases())
		assert.Equal(t, "", f.holder("x"))
		require.Eventually(t, func() bool { return !heartbeating(s) }, waitFor, pollFor)
	})

	t.Run("nothing pending releases the lease", func(t *testing.T) {
		f := newFakeLeaser()
		s := newLeasedScheduler(t, f)

		ctx, cancel := context.WithCancel(context.Background())
		f.onAcquire = func(context.Context, string, string, bool) error {
			cancel()
			return nil // won, as a store that does not stop on cancellation answers
		}

		var ran atomic.Bool
		require.True(t, s.RunOnce(ctx, "x", func(context.Context) {
			ran.Store(true)
		}))
		waitIdle(t, s)

		assert.False(t, ran.Load())
		acquires := f.getAcquires()
		require.Len(t, acquires, 1)
		assert.True(t, acquires[0].won)
		assert.Equal(t, []leaseCall{{"x", acquires[0].owner}}, f.getReleases())
		assert.Equal(t, "", f.holder("x"))
		require.Eventually(t, func() bool { return !heartbeating(s) }, waitFor, pollFor)
	})
}

func TestLeaseAcquireErrorRunNextRetries(t *testing.T) {
	f := newFakeLeaser()
	var calls atomic.Int64
	f.onAcquire = func(context.Context, string, string, bool) error {
		if calls.Add(1) <= 2 {
			return ez.New(ez.EUNAVAILABLE, "store down", nil)
		}
		return nil
	}
	log, buf := newTestLogger()
	s := newLeasedScheduler(t, f, WithLogger(log))

	ran := make(chan struct{})
	require.True(t, s.RunNext(context.Background(), "x", func(context.Context) {
		close(ran)
	}))
	recv(t, ran)
	waitIdle(t, s)

	acquires := f.getAcquires()
	require.Len(t, acquires, 3)
	assert.True(t, acquires[2].won)
	logged := buf.String()
	assert.Equal(t, 1, strings.Count(logged, `level=WARN msg="Scheduler lease failed, retrying" job_id=x`), "the first failure only")
	assert.Equal(t, 1, strings.Count(logged, `level=DEBUG msg="Scheduler lease failed, retrying" job_id=x`), "later failures")
	assert.Equal(t, []leaseCall{{"x", acquires[0].owner}}, f.getReleases(), "only the release after the run")
}

func TestLeaseWaitGivesUpAtBound(t *testing.T) {
	f := newFakeLeaser()
	f.set("x", "other/1")
	log, buf := newTestLogger()
	s := newLeasedScheduler(t, f, WithLogger(log), WithJobTimeout(50*time.Millisecond))
	s.leaseTTL = 300 * time.Millisecond
	s.cancelMargin = 100 * time.Millisecond

	var ran atomic.Bool
	begin := time.Now()
	require.True(t, s.RunNext(context.Background(), "x", func(context.Context) {
		ran.Store(true)
	}))
	require.Eventually(t, func() bool { return s.Active() == 0 }, slow, pollFor)

	assert.GreaterOrEqual(t, time.Since(begin), 350*time.Millisecond)
	assert.False(t, ran.Load())
	assert.Equal(t, "other/1", f.holder("x"))
	acquires := f.getAcquires()
	require.NotEmpty(t, acquires)
	assert.Equal(t, []leaseCall{{"x", acquires[0].owner}}, f.getReleases(), "best-effort release on giving up")
	assert.Contains(t, buf.String(), `level=ERROR msg="Scheduler gave up waiting for the job's lease" job_id=x`)
}

// TestLeaseWaitCancelled: a waiter cancelled after refusals only makes no
// release; one cancelled after a failed attempt releases, best effort.
func TestLeaseWaitCancelled(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "after refusals"
		if failed {
			name = "after a failed attempt"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeLeaser()
			f.set("x", "other/1")
			if failed {
				var calls atomic.Int64
				f.onAcquire = func(context.Context, string, string, bool) error {
					if calls.Add(1) == 1 {
						return ez.New(ez.EUNAVAILABLE, "store down", nil)
					}
					return nil
				}
			}
			log, buf := newTestLogger()
			s := newLeasedScheduler(t, f, WithLogger(log))

			ctx, cancel := context.WithCancel(context.Background())
			require.True(t, s.RunNext(ctx, "x", func(context.Context) {}))
			require.Eventually(t, func() bool { return len(f.getAcquires()) >= 2 }, waitFor, pollFor)
			cancel()
			waitIdle(t, s)

			if failed {
				assert.Equal(t, []leaseCall{{"x", f.getAcquires()[0].owner}}, f.getReleases())
			} else {
				assert.Empty(t, f.getReleases())
			}
			assert.Contains(t, buf.String(), `level=DEBUG msg="Scheduler job cancelled while waiting for its lease" job_id=x`)
			assert.NotContains(t, buf.String(), "level=ERROR")
		})
	}
}

// TestLeaseWaitCancelledDuringFailedAttempt: an attempt that fails as the
// waiter's context ends is logged with the cancellation, and is the last one.
// The poll interval is a nanosecond, so a wait that armed its timer after
// that attempt would find it due and could ask again.
func TestLeaseWaitCancelledDuringFailedAttempt(t *testing.T) {
	f := newFakeLeaser()
	f.set("x", "other/1")
	inSecond := make(chan struct{})
	var calls atomic.Int64
	f.onAcquire = func(ctx context.Context, _, _ string, _ bool) error {
		if calls.Add(1) != 2 {
			return nil
		}
		close(inSecond)
		<-ctx.Done()
		return ez.New(ez.EUNAVAILABLE, "store down as the wait ended", nil)
	}
	log, buf := newTestLogger()
	s := newLeasedScheduler(t, f, WithLogger(log))
	s.leasePoll = time.Nanosecond

	ctx, cancel := context.WithCancel(context.Background())
	require.True(t, s.RunNext(ctx, "x", func(context.Context) {}))
	recv(t, inSecond)
	cancel()
	waitIdle(t, s)

	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `msg="Scheduler job cancelled while waiting for its lease"`) {
			line = l
		}
	}
	assert.Contains(t, line, "store down as the wait ended")
	assert.NotContains(t, buf.String(), "level=WARN")
	assert.Equal(t, []leaseCall{{"x", f.getAcquires()[0].owner}}, f.getReleases(), "the failed attempt is released")
}

// TestLeaseSupersedeMarksEveryAttempt: Supersede passes supersede=true on
// every attempt, answered or failed, until it wins the lease.
func TestLeaseSupersedeMarksEveryAttempt(t *testing.T) {
	f := newFakeLeaser()
	f.set("x", "other/1")
	var calls atomic.Int64
	f.onAcquire = func(context.Context, string, string, bool) error {
		if calls.Add(1) <= 2 {
			return ez.New(ez.EUNAVAILABLE, "store down", nil)
		}
		return nil
	}
	s := newLeasedScheduler(t, f)

	ran := make(chan struct{})
	require.True(t, s.Supersede(context.Background(), "x", func(context.Context) {
		close(ran)
	}))
	require.Eventually(t, func() bool { return len(f.getAcquires()) >= 5 }, waitFor, pollFor)
	f.drop("x")
	recv(t, ran)
	waitIdle(t, s)

	acquires := f.getAcquires()
	for i, c := range acquires {
		assert.True(t, c.supersede, "attempt %d", i)
	}
	assert.False(t, acquires[2].won, "a refusal")
	assert.True(t, acquires[len(acquires)-1].won)
	f.mu.Lock()
	assert.Empty(t, f.leases)
	f.mu.Unlock()
}

// supersedeJob records whether a job ran to its end or was cancelled first.
type supersedeJob struct {
	mu       sync.Mutex
	started  bool
	finished bool
	cause    error // context.Cause when cancelled
	runs     time.Duration
}

func (j *supersedeJob) job(ctx context.Context) {
	j.mu.Lock()
	j.started = true
	j.mu.Unlock()

	timer := time.NewTimer(j.runs)
	defer timer.Stop()
	select {
	case <-timer.C:
		j.mu.Lock()
		j.finished = true
		j.mu.Unlock()
	case <-ctx.Done():
		j.mu.Lock()
		j.cause = context.Cause(ctx)
		j.mu.Unlock()
	}
}

func (j *supersedeJob) state() (bool, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.started, j.finished, j.cause
}

// TestLeaseSupersedeBeatsHolderPendingJob: a Supersede on B against a run on
// A that has a pending job queued on A runs without waiting for that pending
// job, which is cancelled with ErrSuperseded if it got the lease first.
func TestLeaseSupersedeBeatsHolderPendingJob(t *testing.T) {
	f := newFakeLeaser()
	a := newLeasedScheduler(t, f)
	b := newLeasedScheduler(t, f)

	first := newBlocker()
	require.True(t, a.RunOnce(context.Background(), "x", first.job))
	recv(t, first.started)

	// A's pending job runs until it is cancelled, or for longer than the
	// test waits.
	pending := &supersedeJob{runs: time.Minute}
	require.True(t, a.RunNext(context.Background(), "x", pending.job))

	type seen struct {
		started  bool
		finished bool
		cause    error
	}
	atReplacement := make(chan seen, 1)
	require.True(t, b.Supersede(context.Background(), "x", func(context.Context) {
		started, finished, cause := pending.state()
		atReplacement <- seen{started, finished, cause}
	}))

	assert.True(t, errors.Is(recv(t, first.returned), ErrSuperseded))
	got := recvWithin(t, atReplacement, slow)
	assert.False(t, got.finished)
	if got.started {
		assert.True(t, errors.Is(got.cause, ErrSuperseded), "the pending job got the lease and was not cancelled: %v", got.cause)
	}

	// A pending job that had not got the lease yet runs after B's job; it is
	// cut short here.
	waitIdle(t, b)
	require.Eventually(t, func() bool {
		started, _, _ := pending.state()
		return started
	}, slow, pollFor)
	require.True(t, a.Supersede(context.Background(), "x", func(context.Context) {}))
	require.Eventually(t, func() bool { return a.Active() == 0 }, slow, pollFor)
}

// TestLeaseTwoSupersedersAgainstHolder: two Supersede calls on two instances
// against a run on a third end: every job that started and did not finish was
// cancelled with ErrSuperseded, and at least one superseding job ran to
// completion.
func TestLeaseTwoSupersedersAgainstHolder(t *testing.T) {
	f := newFakeLeaser()
	a := newLeasedScheduler(t, f)
	b := newLeasedScheduler(t, f)
	c := newLeasedScheduler(t, f)

	holder := newBlocker()
	require.True(t, a.RunOnce(context.Background(), "x", holder.job))
	recv(t, holder.started)

	jobB := &supersedeJob{runs: 300 * time.Millisecond}
	jobC := &supersedeJob{runs: 300 * time.Millisecond}
	require.True(t, b.Supersede(context.Background(), "x", jobB.job))
	require.True(t, c.Supersede(context.Background(), "x", jobC.job))

	assert.True(t, errors.Is(recvWithin(t, holder.returned, slow), ErrSuperseded))
	for _, s := range []*Scheduler{a, b, c} {
		require.Eventually(t, func() bool { return s.Active() == 0 }, slow, pollFor)
	}

	completed := 0
	for name, j := range map[string]*supersedeJob{"b": jobB, "c": jobC} {
		started, finished, cause := j.state()
		if finished {
			completed++
			continue
		}
		if started {
			assert.True(t, errors.Is(cause, ErrSuperseded), "job %s: %v", name, cause)
		}
	}
	assert.GreaterOrEqual(t, completed, 1)
	for _, s := range []*Scheduler{a, b, c} {
		require.Eventually(t, func() bool { return !heartbeating(s) }, waitFor, pollFor)
	}
	f.mu.Lock()
	assert.Empty(t, f.leases)
	f.mu.Unlock()
}

func TestLeaseRefusedReleasesClaim(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		fc := newFakeClaimer(true, nil)
		f := newFakeLeaser()
		f.set("rec", "other/1")
		s := newLeasedScheduler(t, f, WithClaimer(fc))

		var ran atomic.Bool
		require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
			ran.Store(true)
		}))
		s.runDue(context.Background(), claimTick, claimTick)

		assert.Equal(t, claimCall{id: "rec", tick: claimTick}, recv(t, fc.released))
		waitIdle(t, s)
		assert.False(t, ran.Load())
		assert.Len(t, fc.getClaims(), 1)
		assert.Len(t, f.getAcquires(), 1)
		assert.Empty(t, f.getReleases())
	})

	t.Run("error", func(t *testing.T) {
		fc := newFakeClaimer(true, nil)
		f := newFakeLeaser()
		f.onAcquire = func(context.Context, string, string, bool) error {
			return ez.New(ez.EUNAVAILABLE, "store down", nil)
		}
		s := newLeasedScheduler(t, f, WithClaimer(fc))

		var ran atomic.Bool
		require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
			ran.Store(true)
		}))
		s.runDue(context.Background(), claimTick, claimTick)

		assert.Equal(t, claimCall{id: "rec", tick: claimTick}, recv(t, fc.released))
		waitIdle(t, s)
		assert.False(t, ran.Load())
		assert.Len(t, f.getReleases(), 1)
	})

	t.Run("won", func(t *testing.T) {
		fc := newFakeClaimer(true, nil)
		f := newFakeLeaser()
		s := newLeasedScheduler(t, f, WithClaimer(fc))

		type seen struct {
			claims int
			holder string
		}
		atRun := make(chan seen, 1)
		require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
			atRun <- seen{len(fc.getClaims()), f.holder("rec")}
		}))
		s.runDue(context.Background(), claimTick, claimTick)

		got := recv(t, atRun)
		waitIdle(t, s)
		assert.Equal(t, 1, got.claims, "the run was claimed once")
		assert.NotEmpty(t, got.holder)
		assert.Len(t, fc.getReleases(), 1)
		assert.Len(t, f.getReleases(), 1)
	})
}

// TestLeaseAcquiredBeforePermit: a job over its WithLimit cap holds its lease
// while it waits for a permit.
func TestLeaseAcquiredBeforePermit(t *testing.T) {
	f := newFakeLeaser()
	s := newLeasedScheduler(t, f, WithLimit("grp:", 1))
	permitsAtAcquire := make(chan int, 2)
	f.onAcquire = func(_ context.Context, id, _ string, _ bool) error {
		permitsAtAcquire <- len(s.limits[0].permits)
		return nil
	}

	holder := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "grp:a", holder.job))
	recv(t, holder.started)
	assert.Equal(t, 0, recv(t, permitsAtAcquire))

	waiter := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "grp:b", waiter.job))
	assert.Equal(t, 1, recv(t, permitsAtAcquire), "acquired while the only permit is taken")
	require.Eventually(t, func() bool { return f.holder("grp:b") != "" }, waitFor, pollFor)
	select {
	case <-waiter.started:
		t.Fatal("ran without a permit")
	default:
	}

	close(holder.release)
	recv(t, waiter.started)
	close(waiter.release)
	waitIdle(t, s)
}

func TestLeaseShutdownWithHolderAndWaiter(t *testing.T) {
	f := newFakeLeaser()
	f.set("w", "other/1")
	s := newLeasedScheduler(t, f, WithShutdownTimeout(5*time.Second))

	ctx, cancel := context.WithCancel(context.Background())
	startReturned := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(startReturned)
	}()

	holder := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "h", holder.job))
	recv(t, holder.started)
	var waiterRan atomic.Bool
	require.True(t, s.RunNext(context.Background(), "w", func(context.Context) {
		waiterRan.Store(true)
	}))
	require.Eventually(t, func() bool { return len(f.acquiresBy(s)) >= 3 }, waitFor, pollFor)
	assert.Equal(t, int64(2), s.Active())

	begin := time.Now()
	cancel()
	recv(t, startReturned)
	assert.Less(t, time.Since(begin), 4*time.Second)

	assert.Equal(t, context.Canceled, recv(t, holder.returned))
	assert.False(t, waiterRan.Load())
	assert.Equal(t, int64(0), s.Active())
	assert.Equal(t, "", f.holder("h"), "the holder's lease was released")
	assert.Equal(t, "other/1", f.holder("w"))
	assert.Len(t, f.getReleases(), 1)
	require.Eventually(t, func() bool { return !heartbeating(s) }, waitFor, pollFor)
}

func TestLeaseHeartbeatStopsWhenNoHoldRemains(t *testing.T) {
	f := newFakeLeaser()
	s := newLeasedScheduler(t, f)
	assert.False(t, heartbeating(s))

	run := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "x", run.job))
	recv(t, run.started)
	assert.True(t, heartbeating(s))

	close(run.release)
	waitIdle(t, s)
	require.Eventually(t, func() bool { return !heartbeating(s) }, waitFor, pollFor)
	calls := f.getRenewCalls()

	// A hold registered after the heartbeat stopped starts a new one.
	again := newBlocker()
	require.True(t, s.RunOnce(context.Background(), "x", again.job))
	recv(t, again.started)
	assert.True(t, heartbeating(s))
	f.waitRenewals(t, 2)
	assert.Greater(t, f.getRenewCalls(), calls)
	close(again.release)
	waitIdle(t, s)
	require.Eventually(t, func() bool { return !heartbeating(s) }, waitFor, pollFor)
}

// TestLeaseWaiterKeepsIDBusy: a job waiting for a lease held on another
// instance counts in Active, keeps Idled quiet, refuses RunOnce, and has its
// pending job replaced by RunNext and Supersede; a local Supersede cancels
// the waiter, and the replacement acquires in supersede mode.
func TestLeaseWaiterKeepsIDBusy(t *testing.T) {
	f := newFakeLeaser()
	f.set("w", "other/1")
	s := newLeasedScheduler(t, f)

	var waiterRan atomic.Bool
	require.True(t, s.RunNext(context.Background(), "w", func(context.Context) {
		waiterRan.Store(true)
	}))
	require.Eventually(t, func() bool { return len(f.getAcquires()) >= 2 }, waitFor, pollFor)
	assert.Equal(t, int64(1), s.Active())
	select {
	case <-s.Idled():
		t.Fatal("Idled fired while a job waits for its lease")
	default:
	}

	assert.False(t, s.RunOnce(context.Background(), "w", func(context.Context) {}))
	var nextRan atomic.Bool
	require.True(t, s.RunNext(context.Background(), "w", func(context.Context) {
		nextRan.Store(true)
	}))
	waiterOwner := f.getAcquires()[0].owner

	replacementRan := make(chan struct{})
	require.True(t, s.Supersede(context.Background(), "w", func(context.Context) {
		close(replacementRan)
	}))

	// The replacement marks the remote lease, then waits for it.
	require.Eventually(t, func() bool {
		for _, c := range f.getAcquires() {
			if c.owner != waiterOwner && c.supersede {
				return true
			}
		}
		return false
	}, waitFor, pollFor)
	assert.Equal(t, int64(1), s.Active())
	f.drop("w")

	recv(t, replacementRan)
	recv(t, s.Idled())
	assert.False(t, waiterRan.Load())
	assert.False(t, nextRan.Load())
	assert.Equal(t, int64(0), s.Active())
}
