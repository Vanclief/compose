package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/logger"
	"github.com/vanclief/ez"
)

const (
	waitFor = 2 * time.Second
	pollFor = time.Millisecond
)

func newTestScheduler(t *testing.T, opts ...Option) *Scheduler {
	t.Helper()
	s, err := New(opts...)
	require.NoError(t, err)
	return s
}

// recv fails the test when ch does not deliver within waitFor.
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitFor):
		t.Fatal("timed out waiting on channel")
	}
	var zero T
	return zero
}

// waitIdle waits until no id has admitted work.
func waitIdle(t *testing.T, s *Scheduler) {
	t.Helper()
	require.Eventually(t, func() bool { return s.Active() == 0 }, waitFor, pollFor)
}

// runLog records job names in the order their bodies start.
type runLog struct {
	mu    sync.Mutex
	names []string
}

func (l *runLog) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.names = append(l.names, name)
}

func (l *runLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.names...)
}

func TestNewDefaults(t *testing.T) {
	s := newTestScheduler(t)

	assert.Equal(t, logger.Noop{}, s.log)
	assert.Equal(t, DefaultShutdownTimeout, s.shutdownTimeout)
	assert.Equal(t, DefaultJobTimeout, s.jobTimeout)
	assert.Empty(t, s.limits)
	assert.NotNil(t, s.now)
	assert.NotNil(t, s.running)
	assert.Equal(t, 1, cap(s.idledCh))
	assert.Equal(t, int64(0), s.Active())
}

func TestNewAppliesOptions(t *testing.T) {
	s := newTestScheduler(t,
		WithLogger(nil),
		WithShutdownTimeout(time.Second),
		WithJobTimeout(time.Minute),
		WithShutdownTimeout(0),
		WithJobTimeout(-time.Second),
	)

	assert.Equal(t, logger.Noop{}, s.log)
	assert.Equal(t, time.Second, s.shutdownTimeout)
	assert.Equal(t, time.Minute, s.jobTimeout)
}

func TestNewRejectsInvalidLimits(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{"empty prefix", []Option{WithLimit("", 1)}},
		{"zero size", []Option{WithLimit("a:", 0)}},
		{"negative size", []Option{WithLimit("a:", -1)}},
		{"duplicate prefix", []Option{WithLimit("a:", 1), WithLimit("a:", 2)}},
		{"nested prefix", []Option{WithLimit("a:", 1), WithLimit("a:b:", 1)}},
		{"nested prefix reversed", []Option{WithLimit("a:b:", 1), WithLimit("a:", 1)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New(tt.opts...)
			require.Error(t, err)
			assert.Nil(t, s)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
		})
	}
}

func TestNewAcceptsValidLimits(t *testing.T) {
	s := newTestScheduler(t, WithLimit("a:", 2), WithLimit("b:", 1))

	require.Len(t, s.limits, 2)
	assert.Equal(t, 2, cap(s.limits[0].permits))
	assert.Equal(t, 1, cap(s.limits[1].permits))
	assert.Nil(t, s.limitFor("c:1"))
	assert.Equal(t, "a:", s.limitFor("a:1").prefix)
}

func TestAddValidation(t *testing.T) {
	noop := func(context.Context) {}

	tests := []struct {
		name string
		id   string
		when Schedule
		job  Job
	}{
		{"empty id", "", Every(time.Minute), noop},
		{"nil job", "a", Every(time.Minute), nil},
		{"zero schedule", "a", Schedule{}, noop},
		{"invalid every", "a", Every(7 * time.Minute), noop},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestScheduler(t)
			err := s.Add(tt.id, tt.when, tt.job)
			require.Error(t, err)
			assert.Equal(t, ez.EINVALID, ez.ErrorCode(err))
			assert.Empty(t, s.registrations)
		})
	}
}

func TestAddRejectsDuplicateID(t *testing.T) {
	s := newTestScheduler(t)
	noop := func(context.Context) {}
	when := Every(15 * time.Minute)

	require.NoError(t, s.Add("a", when, noop))
	require.NoError(t, s.Add("b", when, noop))

	err := s.Add("a", Hourly(5), noop)
	require.Error(t, err)
	assert.Equal(t, ez.ECONFLICT, ez.ErrorCode(err))
	assert.Len(t, s.registrations, 2)
}

func TestRunOnceSkipsBusyID(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	release := make(chan struct{})

	ok := s.RunOnce(context.Background(), "a", func(context.Context) {
		close(started)
		<-release
	})
	require.True(t, ok)
	recv(t, started)

	var secondRan atomic.Bool
	ok = s.RunOnce(context.Background(), "a", func(context.Context) {
		secondRan.Store(true)
	})
	assert.False(t, ok)

	close(release)
	waitIdle(t, s)
	assert.False(t, secondRan.Load())

	again := make(chan struct{})
	ok = s.RunOnce(context.Background(), "a", func(context.Context) {
		close(again)
	})
	require.True(t, ok)
	recv(t, again)
	waitIdle(t, s)
}

func TestRunOnceRejectsInvalidInput(t *testing.T) {
	s := newTestScheduler(t)
	noop := func(context.Context) {}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	assert.False(t, s.RunOnce(context.Background(), "", noop))
	assert.False(t, s.RunOnce(context.Background(), "a", nil))
	assert.False(t, s.RunOnce(cancelled, "a", noop))
	assert.False(t, s.Supersede(cancelled, "a", noop))
	assert.False(t, s.RunNext(cancelled, "a", noop))
	assert.Equal(t, int64(0), s.Active())
}

func TestSupersedeStartsIdleID(t *testing.T) {
	s := newTestScheduler(t)
	ran := make(chan struct{})

	ok := s.Supersede(context.Background(), "a", func(context.Context) {
		close(ran)
	})
	require.True(t, ok)
	recv(t, ran)
	waitIdle(t, s)
}

func TestSupersedeCancelsRunningWithErrSuperseded(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	causes := make(chan error, 1)
	var firstReturned atomic.Bool
	secondSawFirstReturned := make(chan bool, 1)

	ok := s.RunOnce(context.Background(), "a", func(ctx context.Context) {
		defer firstReturned.Store(true)
		close(started)
		<-ctx.Done()
		causes <- context.Cause(ctx)
	})
	require.True(t, ok)
	recv(t, started)

	ok = s.Supersede(context.Background(), "a", func(context.Context) {
		secondSawFirstReturned <- firstReturned.Load()
	})
	require.True(t, ok)

	cause := recv(t, causes)
	assert.True(t, errors.Is(cause, ErrSuperseded))
	assert.True(t, errors.Is(cause, context.Canceled))
	assert.True(t, recv(t, secondSawFirstReturned))
	waitIdle(t, s)
}

func TestSupersedeThreeRapidCallsRunFirstAndLast(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	release := make(chan struct{})
	runs := &runLog{}

	require.True(t, s.Supersede(context.Background(), "a", func(context.Context) {
		runs.add("first")
		close(started)
		<-release
	}))
	recv(t, started)

	require.True(t, s.Supersede(context.Background(), "a", func(context.Context) {
		runs.add("middle")
	}))
	require.True(t, s.Supersede(context.Background(), "a", func(context.Context) {
		runs.add("last")
	}))

	close(release)
	recv(t, s.Idled())
	assert.Equal(t, []string{"first", "last"}, runs.get())
}

func TestRunNextDoesNotCancelRunning(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	release := make(chan struct{})
	firstCancelled := make(chan bool, 1)
	runs := &runLog{}

	require.True(t, s.RunOnce(context.Background(), "a", func(ctx context.Context) {
		runs.add("first")
		close(started)
		select {
		case <-release:
			firstCancelled <- false
		case <-ctx.Done():
			firstCancelled <- true
		}
	}))
	recv(t, started)

	require.True(t, s.RunNext(context.Background(), "a", func(context.Context) {
		runs.add("next")
	}))

	close(release)
	assert.False(t, recv(t, firstCancelled))
	recv(t, s.Idled())
	assert.Equal(t, []string{"first", "next"}, runs.get())
}

func TestRunNextAfterSupersedeKeepsCancellation(t *testing.T) {
	s := newTestScheduler(t)
	ctxs := make(chan context.Context, 1)
	release := make(chan struct{})
	runs := &runLog{}

	require.True(t, s.RunOnce(context.Background(), "a", func(ctx context.Context) {
		runs.add("first")
		ctxs <- ctx
		<-release
	}))
	firstCtx := recv(t, ctxs)

	require.True(t, s.Supersede(context.Background(), "a", func(context.Context) {
		runs.add("superseding")
	}))
	require.True(t, s.RunNext(context.Background(), "a", func(context.Context) {
		runs.add("next")
	}))

	require.Error(t, firstCtx.Err())
	assert.True(t, errors.Is(context.Cause(firstCtx), ErrSuperseded))

	close(release)
	recv(t, s.Idled())
	assert.Equal(t, []string{"first", "next"}, runs.get())
}

func TestSupersedeAfterRunNextReplacesPendingAndCancels(t *testing.T) {
	s := newTestScheduler(t)
	ctxs := make(chan context.Context, 1)
	runs := &runLog{}

	require.True(t, s.RunOnce(context.Background(), "a", func(ctx context.Context) {
		runs.add("first")
		ctxs <- ctx
		<-ctx.Done()
	}))
	firstCtx := recv(t, ctxs)

	require.True(t, s.RunNext(context.Background(), "a", func(context.Context) {
		runs.add("next")
	}))
	assert.NoError(t, firstCtx.Err())

	require.True(t, s.Supersede(context.Background(), "a", func(context.Context) {
		runs.add("superseding")
	}))

	recv(t, s.Idled())
	assert.True(t, errors.Is(context.Cause(firstCtx), ErrSuperseded))
	assert.Equal(t, []string{"first", "superseding"}, runs.get())
}

func TestPendingWithCancelledContextIsDropped(t *testing.T) {
	s := newTestScheduler(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var pendingRan atomic.Bool

	require.True(t, s.RunOnce(context.Background(), "a", func(context.Context) {
		close(started)
		<-release
	}))
	recv(t, started)

	pendingCtx, cancel := context.WithCancel(context.Background())
	require.True(t, s.RunNext(pendingCtx, "a", func(context.Context) {
		pendingRan.Store(true)
	}))
	cancel()

	close(release)
	recv(t, s.Idled())
	assert.False(t, pendingRan.Load())
	assert.Equal(t, int64(0), s.Active())

	s.runMu.Lock()
	_, busy := s.running["a"]
	s.runMu.Unlock()
	assert.False(t, busy)
}

func TestActiveCountsIDOnceAndIdledFiresAfterSuccessor(t *testing.T) {
	s := newTestScheduler(t)
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	nextStarted := make(chan struct{})
	nextRelease := make(chan struct{})

	require.True(t, s.RunOnce(context.Background(), "a", func(context.Context) {
		close(firstStarted)
		<-firstRelease
	}))
	recv(t, firstStarted)
	require.True(t, s.RunNext(context.Background(), "a", func(context.Context) {
		close(nextStarted)
		<-nextRelease
	}))
	assert.Equal(t, int64(1), s.Active())

	close(firstRelease)
	recv(t, nextStarted)
	assert.Equal(t, int64(1), s.Active())
	select {
	case <-s.Idled():
		t.Fatal("Idled fired between a job and its successor")
	default:
	}

	close(nextRelease)
	recv(t, s.Idled())
	assert.Equal(t, int64(0), s.Active())
}

func TestWithLimitCapsConcurrency(t *testing.T) {
	s := newTestScheduler(t, WithLimit("grp:", 2))
	release := make(chan struct{})
	var inFlight atomic.Int64
	var maxInFlight atomic.Int64
	var started atomic.Int64
	var finished atomic.Int64

	body := func(context.Context) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		started.Add(1)
		<-release
		inFlight.Add(-1)
		finished.Add(1)
	}

	for _, id := range []string{"grp:1", "grp:2", "grp:3", "grp:4"} {
		require.True(t, s.RunOnce(context.Background(), id, body))
	}
	require.Eventually(t, func() bool { return started.Load() == 2 }, waitFor, pollFor)
	assert.Equal(t, int64(4), s.Active())

	otherRan := make(chan struct{})
	require.True(t, s.RunOnce(context.Background(), "other:1", func(context.Context) {
		close(otherRan)
	}))
	recv(t, otherRan)
	assert.Equal(t, int64(2), started.Load())

	close(release)
	recv(t, s.Idled())
	assert.Equal(t, int64(4), finished.Load())
	assert.Equal(t, int64(2), maxInFlight.Load())
	assert.Empty(t, s.limits[0].permits)
}

func TestSupersedeCancelsPermitWaiter(t *testing.T) {
	s := newTestScheduler(t, WithLimit("grp:", 1))
	holderStarted := make(chan struct{})
	release := make(chan struct{})
	var waiterRan atomic.Bool
	replacementRan := make(chan struct{})

	require.True(t, s.RunOnce(context.Background(), "grp:a", func(context.Context) {
		close(holderStarted)
		<-release
	}))
	recv(t, holderStarted)

	require.True(t, s.RunOnce(context.Background(), "grp:b", func(context.Context) {
		waiterRan.Store(true)
	}))
	assert.Equal(t, int64(2), s.Active())

	require.True(t, s.Supersede(context.Background(), "grp:b", func(context.Context) {
		close(replacementRan)
	}))
	assert.Equal(t, int64(2), s.Active())

	// With a single permit, the replacement can only run if the cancelled
	// waiter did not keep one.
	close(release)
	recv(t, replacementRan)
	recv(t, s.Idled())
	assert.False(t, waiterRan.Load())
	assert.Empty(t, s.limits[0].permits)
}

func TestJobTimeoutCancelsJob(t *testing.T) {
	s := newTestScheduler(t, WithJobTimeout(50*time.Millisecond))
	errs := make(chan error, 1)

	require.True(t, s.RunOnce(context.Background(), "a", func(ctx context.Context) {
		<-ctx.Done()
		errs <- ctx.Err()
	}))

	assert.ErrorIs(t, recv(t, errs), context.DeadlineExceeded)
	waitIdle(t, s)
}

func TestJobTimeoutStartsAfterPermit(t *testing.T) {
	timeout := 200 * time.Millisecond
	s := newTestScheduler(t, WithLimit("grp:", 1), WithJobTimeout(timeout))
	holderStarted := make(chan struct{})
	release := make(chan struct{})

	type observation struct {
		errAtStart error
		remaining  time.Duration
		waited     time.Duration
	}
	observed := make(chan observation, 1)

	require.True(t, s.RunOnce(context.Background(), "grp:a", func(context.Context) {
		close(holderStarted)
		<-release
	}))
	recv(t, holderStarted)

	admitted := time.Now()
	require.True(t, s.RunOnce(context.Background(), "grp:b", func(ctx context.Context) {
		deadline, _ := ctx.Deadline()
		observed <- observation{
			errAtStart: ctx.Err(),
			remaining:  time.Until(deadline),
			waited:     time.Since(admitted),
		}
	}))

	time.Sleep(2 * timeout)
	close(release)

	o := recv(t, observed)
	assert.GreaterOrEqual(t, o.waited, 2*timeout)
	assert.NoError(t, o.errAtStart)
	assert.Greater(t, o.remaining, timeout/2)
	waitIdle(t, s)
}

func TestPanicRecovery(t *testing.T) {
	s := newTestScheduler(t)

	require.True(t, s.RunOnce(context.Background(), "a", func(context.Context) {
		panic("boom")
	}))
	recv(t, s.Idled())

	ran := make(chan struct{})
	require.True(t, s.RunOnce(context.Background(), "a", func(context.Context) {
		close(ran)
	}))
	recv(t, ran)
	waitIdle(t, s)
}

func TestShutdownCancelsRunningAndDropsPending(t *testing.T) {
	s := newTestScheduler(t)
	ctx, cancel := context.WithCancel(context.Background())
	startReturned := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(startReturned)
	}()

	jobCtxs := make(chan context.Context, 1)
	require.True(t, s.RunOnce(context.Background(), "a", func(jobCtx context.Context) {
		jobCtxs <- jobCtx
		<-jobCtx.Done()
	}))
	jobCtx := recv(t, jobCtxs)

	var pendingRan atomic.Bool
	require.True(t, s.RunNext(context.Background(), "a", func(context.Context) {
		pendingRan.Store(true)
	}))

	cancel()
	recv(t, startReturned)

	assert.Equal(t, context.Canceled, context.Cause(jobCtx))
	assert.False(t, errors.Is(context.Cause(jobCtx), ErrSuperseded))
	assert.False(t, pendingRan.Load())
	assert.Equal(t, int64(0), s.Active())

	var lateRan atomic.Bool
	late := func(context.Context) {
		lateRan.Store(true)
	}
	assert.False(t, s.RunOnce(context.Background(), "b", late))
	assert.False(t, s.Supersede(context.Background(), "b", late))
	assert.False(t, s.RunNext(context.Background(), "b", late))

	require.NoError(t, s.Add("c", Every(time.Minute), late))
	minute := time.Now().Truncate(time.Minute)
	s.runDue(context.Background(), minute, minute)

	// A second Start on a stopped Scheduler returns immediately.
	s.Start(context.Background())

	assert.False(t, lateRan.Load())
	assert.Equal(t, int64(0), s.Active())
}

func TestShutdownTimeoutBoundsWait(t *testing.T) {
	s := newTestScheduler(t, WithShutdownTimeout(50*time.Millisecond))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	started := make(chan struct{})
	require.True(t, s.RunOnce(context.Background(), "stubborn", func(context.Context) {
		close(started)
		<-release
	}))
	recv(t, started)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	begin := time.Now()
	s.Start(ctx)
	elapsed := time.Since(begin)

	assert.GreaterOrEqual(t, elapsed, 50*time.Millisecond)
	assert.Less(t, elapsed, time.Second)
}

func TestCatchUp(t *testing.T) {
	at := func(h, m, sec, ms int) time.Time {
		return time.Date(2026, 9, 24, h, m, sec, ms*int(time.Millisecond), time.Local)
	}

	newRecorder := func(t *testing.T) (*Scheduler, *runLog) {
		t.Helper()
		s := newTestScheduler(t)
		log := &runLog{}
		record := func(name string) Job {
			return func(context.Context) {
				log.add(name)
			}
		}
		require.NoError(t, s.Add("every-1m", Every(time.Minute), record("every-1m")))
		require.NoError(t, s.Add("daily-9-17", Daily(9, 17, time.Local), record("daily-9-17")))
		require.NoError(t, s.Add("daily-9-25", Daily(9, 25, time.Local), record("daily-9-25")))
		return s, log
	}

	t.Run("normal wake evaluates the aimed minute only", func(t *testing.T) {
		s, log := newRecorder(t)
		last := s.catchUp(context.Background(), at(9, 14, 0, 0), at(9, 15, 0, 200))
		waitIdle(t, s)
		assert.Equal(t, at(9, 15, 0, 0), last)
		assert.ElementsMatch(t, []string{"every-1m"}, log.get())
	})

	t.Run("clock still inside the last minute evaluates nothing", func(t *testing.T) {
		s, log := newRecorder(t)
		last := s.catchUp(context.Background(), at(9, 15, 0, 0), at(9, 15, 59, 990))
		assert.Equal(t, at(9, 15, 0, 0), last)
		assert.Empty(t, log.get())
		assert.Equal(t, int64(0), s.Active())
	})

	t.Run("late wake catches up each registration once", func(t *testing.T) {
		s, log := newRecorder(t)
		last := s.catchUp(context.Background(), at(9, 14, 0, 0), at(9, 20, 5, 0))
		waitIdle(t, s)
		assert.Equal(t, at(9, 20, 0, 0), last)
		// 09:15 through 09:20: the daily at 09:17 is due, the one at 09:25 is
		// not, and the per-minute job runs once, not six times.
		assert.ElementsMatch(t, []string{"every-1m", "daily-9-17"}, log.get())
	})

	t.Run("clock stepped back follows the clock without evaluating", func(t *testing.T) {
		s, log := newRecorder(t)
		last := s.catchUp(context.Background(), at(9, 15, 0, 0), at(8, 14, 30, 0))
		assert.Equal(t, at(8, 14, 0, 0), last)
		assert.Empty(t, log.get())
		assert.Equal(t, int64(0), s.Active())
	})
}

func TestStartCatchesUpAfterLateWake(t *testing.T) {
	mx, err := time.LoadLocation("America/Mexico_City")
	require.NoError(t, err)

	target := time.Date(2026, 9, 24, 9, 0, 0, 0, mx)
	s := newTestScheduler(t)
	log := &runLog{}
	record := func(name string) Job {
		return func(context.Context) {
			log.add(name)
		}
	}
	require.NoError(t, s.Add("every-1m", Every(time.Minute), record("every-1m")))
	require.NoError(t, s.Add("daily-9-02", Daily(9, 2, mx), record("daily-9-02")))
	require.NoError(t, s.Add("daily-9-07", Daily(9, 7, mx), record("daily-9-07")))

	// Start reads the clock once before its first sleep. That read sees a
	// clock 500ms before the target minute. Every later read, starting with
	// the one after the wake, sees a clock five minutes past it, as if the
	// process had been frozen during the wait.
	before := target.Add(-500 * time.Millisecond).Sub(time.Now())
	after := target.Add(5*time.Minute + 100*time.Millisecond).Sub(time.Now())
	var reads atomic.Int64
	s.now = func() time.Time {
		n := reads.Add(1)
		if n == 1 {
			return time.Now().Add(before)
		}
		return time.Now().Add(after)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startReturned := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(startReturned)
	}()

	require.Eventually(t, func() bool { return len(log.get()) == 2 }, waitFor, pollFor)
	cancel()
	recv(t, startReturned)

	// 09:00 through 09:05 were caught up: the 09:02 daily ran, the 09:07 one
	// did not, and the per-minute job ran once for the whole wake.
	assert.ElementsMatch(t, []string{"every-1m", "daily-9-02"}, log.get())
}

func TestStartRereadsClockAfterDispatch(t *testing.T) {
	mx, err := time.LoadLocation("America/Mexico_City")
	require.NoError(t, err)

	target := time.Date(2026, 9, 24, 9, 0, 0, 0, mx)
	s := newTestScheduler(t)
	fired := make(chan struct{}, 4)
	require.NoError(t, s.Add("every-1m", Every(time.Minute), func(context.Context) {
		fired <- struct{}{}
	}))

	// Start reads the clock before its first sleep, on each wake, and once
	// more after dispatch to arm the next timer. The read after the first
	// dispatch lands half a second before the next boundary, so the second
	// wait is short only if Start uses it. Measured from the first read, the
	// second wait would be a minute long and the second firing would miss
	// the deadline below.
	readings := []time.Time{
		target.Add(-500 * time.Millisecond),
		target.Add(100 * time.Millisecond),
		target.Add(time.Minute - 500*time.Millisecond),
		target.Add(time.Minute + 100*time.Millisecond),
	}
	var reads atomic.Int64
	s.now = func() time.Time {
		n := int(reads.Add(1)) - 1
		if n >= len(readings) {
			n = len(readings) - 1
		}
		return readings[n]
	}

	ctx, cancel := context.WithCancel(context.Background())
	startReturned := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(startReturned)
	}()

	recv(t, fired)
	recv(t, fired)
	cancel()
	recv(t, startReturned)
}

func TestStartRunsDueJobsAtNextBoundary(t *testing.T) {
	mx, err := time.LoadLocation("America/Mexico_City")
	require.NoError(t, err)
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)

	target := time.Date(2026, 9, 24, 9, 0, 0, 0, mx)
	localMinute := target.Local().Minute()

	s := newTestScheduler(t)
	type firing struct {
		name string
		at   time.Time
	}
	fired := make(chan firing, 16)
	record := func(name string) Job {
		return func(context.Context) {
			fired <- firing{name: name, at: s.now()}
		}
	}

	// On a host whose UTC offset is a whole number of hours, localMinute is 0
	// and these are Hourly(0), Every(30m) firing and Hourly(15) not firing.
	schedules := map[string]Schedule{
		"daily-mx":    Daily(9, 0, mx),
		"hourly":      Hourly(localMinute),
		"every-1m":    Every(time.Minute),
		"hourly-late": Hourly((localMinute + 15) % 60),
		"every-30m":   Every(30 * time.Minute),
		"daily-tokyo": Daily(9, 0, tokyo),
	}
	want := map[string]bool{
		"daily-mx":    true,
		"hourly":      true,
		"every-1m":    true,
		"hourly-late": false,
		"every-30m":   localMinute%30 == 0,
		"daily-tokyo": false,
	}
	for name, when := range schedules {
		require.NoError(t, s.Add(name, when, record(name)))
	}

	shift := target.Add(-500 * time.Millisecond).Sub(time.Now())
	s.now = func() time.Time { return time.Now().Add(shift) }

	ctx, cancel := context.WithCancel(context.Background())
	startReturned := make(chan struct{})
	go func() {
		s.Start(ctx)
		close(startReturned)
	}()

	expected := 0
	for _, w := range want {
		if w {
			expected++
		}
	}

	got := map[string]time.Time{}
	for len(got) < expected {
		f := recv(t, fired)
		got[f.name] = f.at
	}

	// Shutdown waits for every admitted job, so anything else that was due at
	// this boundary has recorded by the time Start returns.
	cancel()
	recv(t, startReturned)
	close(fired)
	for f := range fired {
		got[f.name] = f.at
	}

	for name, w := range want {
		at, ran := got[name]
		assert.Equal(t, w, ran, name)
		if ran {
			assert.False(t, at.Before(target), "%s fired before the first boundary", name)
		}
	}
}

func TestRunDueSkipsBusyRegistrationAndKeepsPending(t *testing.T) {
	s := newTestScheduler(t)
	release := make(chan struct{})
	started := make(chan struct{}, 2)
	var recurringRuns atomic.Int64

	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		recurringRuns.Add(1)
		started <- struct{}{}
		<-release
	}))

	first := time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local)
	s.runDue(context.Background(), first, first)
	recv(t, started)

	pendingRan := make(chan struct{})
	require.True(t, s.RunNext(context.Background(), "rec", func(context.Context) {
		close(pendingRan)
	}))

	second := first.Add(time.Minute)
	s.runDue(context.Background(), second, second)

	s.runMu.Lock()
	pending := s.running["rec"].pending
	s.runMu.Unlock()
	require.NotNil(t, pending)
	assert.Equal(t, int64(1), recurringRuns.Load())

	close(release)
	recv(t, pendingRan)
	recv(t, s.Idled())
	assert.Equal(t, int64(1), recurringRuns.Load())
}
