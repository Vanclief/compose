package scheduler

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vanclief/compose/components/logger"
	"github.com/vanclief/ez"
)

// claimCall is one Claim or Release call. lease is zero for a Release.
type claimCall struct {
	id    string
	tick  time.Time
	lease time.Duration
}

// fakeClaimer answers every Claim with won and claimErr, or with ctx's error
// when ctx is done by the time it answers unless ignoreCancel is set, as the
// PostgreSQL store does, and records the calls.
type fakeClaimer struct {
	won          bool
	claimErr     error
	releaseErr   error
	ignoreCancel bool
	onClaim      func(ctx context.Context, id string) // runs inside Claim, before it answers

	mu          sync.Mutex
	claims      []claimCall
	releases    []claimCall
	releaseCtxs []error // ctx.Err() seen by each Release

	released chan claimCall
}

func newFakeClaimer(won bool, claimErr error) *fakeClaimer {
	return &fakeClaimer{won: won, claimErr: claimErr, released: make(chan claimCall, 16)}
}

func (f *fakeClaimer) Claim(ctx context.Context, id string, tick time.Time, lease time.Duration) (bool, error) {
	f.mu.Lock()
	f.claims = append(f.claims, claimCall{id: id, tick: tick, lease: lease})
	f.mu.Unlock()
	if f.onClaim != nil {
		f.onClaim(ctx, id)
	}
	if f.claimErr != nil {
		return false, f.claimErr
	}
	if ctx.Err() != nil && !f.ignoreCancel {
		return false, ctx.Err()
	}
	return f.won, nil
}

func (f *fakeClaimer) Release(ctx context.Context, id string, tick time.Time) error {
	call := claimCall{id: id, tick: tick}
	f.mu.Lock()
	f.releases = append(f.releases, call)
	f.releaseCtxs = append(f.releaseCtxs, ctx.Err())
	f.mu.Unlock()
	f.released <- call
	return f.releaseErr
}

func (f *fakeClaimer) getClaims() []claimCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]claimCall(nil), f.claims...)
}

func (f *fakeClaimer) getReleases() []claimCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]claimCall(nil), f.releases...)
}

// logBuffer collects the text a slog handler writes from any goroutine.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestLogger returns a logger that writes every level as text to the
// returned buffer.
func newTestLogger() (logger.Logger, *logBuffer) {
	buf := &logBuffer{}
	handler := slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return logger.NewSlog(slog.New(handler)), buf
}

var claimTick = time.Date(2026, 9, 24, 9, 0, 0, 0, time.Local)

func TestWithClaimer(t *testing.T) {
	s := newTestScheduler(t)
	assert.Nil(t, s.claimer)

	fc := newFakeClaimer(true, nil)
	s = newTestScheduler(t, WithClaimer(fc))
	assert.Equal(t, fc, s.claimer)
}

func TestClaimWonRunsAndReleases(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc), WithJobTimeout(time.Minute))

	releasesDuringRun := make(chan int, 1)
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		releasesDuringRun <- len(fc.getReleases())
	}))

	s.runDue(context.Background(), claimTick, claimTick)

	assert.Equal(t, 0, recv(t, releasesDuringRun), "released before the run returned")
	assert.Equal(t, claimCall{id: "rec", tick: claimTick}, recv(t, fc.released))
	waitIdle(t, s)
	assert.Equal(t, []claimCall{{id: "rec", tick: claimTick, lease: time.Minute + leaseMargin}}, fc.getClaims())
	assert.Len(t, fc.getReleases(), 1)
}

func TestClaimRefusedSkipsRun(t *testing.T) {
	fc := newFakeClaimer(false, nil)
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))

	var ran atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		ran.Store(true)
	}))

	s.runDue(context.Background(), claimTick, claimTick)
	waitIdle(t, s)

	assert.False(t, ran.Load())
	assert.Len(t, fc.getClaims(), 1)
	assert.Empty(t, fc.getReleases())
	assert.Contains(t, buf.String(), `level=DEBUG msg="Job claim refused, skipping" job_id=rec`)
}

func TestClaimErrorSkipsRunAndLogs(t *testing.T) {
	fc := newFakeClaimer(true, ez.New(ez.EUNAVAILABLE, "store down", nil))
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))

	var ran atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		ran.Store(true)
	}))

	s.runDue(context.Background(), claimTick, claimTick)
	waitIdle(t, s)

	assert.False(t, ran.Load())
	assert.Empty(t, fc.getReleases())
	assert.Contains(t, buf.String(), `level=ERROR msg="Scheduler claim failed, skipping" job_id=rec`)
	assert.Contains(t, buf.String(), "store down")
}

func TestClaimSkippedWhileIDBusyLocally(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))

	var recurringRan atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		recurringRan.Store(true)
	}))

	release := make(chan struct{})
	started := make(chan struct{})
	require.True(t, s.RunOnce(context.Background(), "rec", func(context.Context) {
		close(started)
		<-release
	}))
	recv(t, started)

	s.runDue(context.Background(), claimTick, claimTick)

	close(release)
	waitIdle(t, s)
	assert.False(t, recurringRan.Load())
	assert.Empty(t, fc.getClaims())
	assert.Empty(t, fc.getReleases())
	assert.Contains(t, buf.String(), `msg="Job already running, skipping" job_id=rec`)
}

func TestOnDemandRunsNeverClaim(t *testing.T) {
	// A claimer that refuses every claim: on-demand runs must not ask it.
	fc := newFakeClaimer(false, nil)
	s := newTestScheduler(t, WithClaimer(fc))

	ran := make(chan string, 3)
	record := func(name string) Job {
		return func(context.Context) {
			ran <- name
		}
	}
	require.True(t, s.RunOnce(context.Background(), "a", record("a")))
	require.True(t, s.Supersede(context.Background(), "b", record("b")))
	require.True(t, s.RunNext(context.Background(), "c", record("c")))

	got := []string{recv(t, ran), recv(t, ran), recv(t, ran)}
	waitIdle(t, s)
	assert.ElementsMatch(t, []string{"a", "b", "c"}, got)
	assert.Empty(t, fc.getClaims())
	assert.Empty(t, fc.getReleases())
}

// TestOnDemandDuringClaimSeesBusyID: while the claim is in flight the id is
// busy, so a RunOnce is refused and a RunNext queues behind the claimed run,
// which still runs, and runs after the claim is released.
func TestOnDemandDuringClaimSeesBusyID(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc))

	ran := make(chan string, 3)
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		ran <- "rec"
	}))

	var runOnceAdmitted, runNextAdmitted atomic.Bool
	releasesAtNext := make(chan int, 1)
	fc.onClaim = func(context.Context, string) {
		runOnceAdmitted.Store(s.RunOnce(context.Background(), "rec", func(context.Context) {
			ran <- "once"
		}))
		runNextAdmitted.Store(s.RunNext(context.Background(), "rec", func(context.Context) {
			releasesAtNext <- len(fc.getReleases())
			ran <- "next"
		}))
	}

	s.runDue(context.Background(), claimTick, claimTick)

	assert.Equal(t, "rec", recv(t, ran))
	assert.Equal(t, 1, recv(t, releasesAtNext), "pending job ran before the release")
	assert.Equal(t, "next", recv(t, ran))
	waitIdle(t, s)
	assert.False(t, runOnceAdmitted.Load())
	assert.True(t, runNextAdmitted.Load())
	assert.Equal(t, []claimCall{{id: "rec", tick: claimTick}}, fc.getReleases())
}

// TestRefusedClaimRunsPendingJob: a pending job queued during a refused
// claim runs, unclaimed, as after any invocation.
func TestRefusedClaimRunsPendingJob(t *testing.T) {
	fc := newFakeClaimer(false, nil)
	s := newTestScheduler(t, WithClaimer(fc))

	var recurringRan atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		recurringRan.Store(true)
	}))

	pendingRan := make(chan struct{})
	fc.onClaim = func(context.Context, string) {
		s.RunNext(context.Background(), "rec", func(context.Context) {
			close(pendingRan)
		})
	}

	s.runDue(context.Background(), claimTick, claimTick)

	recv(t, pendingRan)
	waitIdle(t, s)
	assert.False(t, recurringRan.Load())
	assert.Len(t, fc.getClaims(), 1)
	assert.Empty(t, fc.getReleases())
}

// TestSupersedeDuringClaimCancelsIt: Supersede cancels an in-flight claim,
// which is not logged as a store failure, and its job then runs.
func TestSupersedeDuringClaimCancelsIt(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))

	var recurringRan atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		recurringRan.Store(true)
	}))

	replacementRan := make(chan struct{})
	fc.onClaim = func(ctx context.Context, _ string) {
		s.Supersede(context.Background(), "rec", func(context.Context) {
			close(replacementRan)
		})
		<-ctx.Done()
	}

	s.runDue(context.Background(), claimTick, claimTick)

	recv(t, replacementRan)
	waitIdle(t, s)
	assert.False(t, recurringRan.Load())
	assert.Empty(t, fc.getReleases())
	assert.Contains(t, buf.String(), `level=DEBUG msg="Scheduler job cancelled during claim" job_id=rec`)
	assert.NotContains(t, buf.String(), "level=ERROR")
}

// TestShutdownDuringClaimLogsNoError: shutdown cancels an in-flight claim,
// as a slow store would see it, without an error log line.
func TestShutdownDuringClaimLogsNoError(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))

	var ran atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		ran.Store(true)
	}))

	claiming := make(chan struct{})
	fc.onClaim = func(ctx context.Context, _ string) {
		close(claiming)
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.runDue(ctx, claimTick, claimTick)
	recv(t, claiming)

	cancel()
	s.shutdown()

	assert.False(t, ran.Load())
	assert.Empty(t, fc.getReleases())
	assert.Equal(t, int64(0), s.Active())
	assert.NotContains(t, buf.String(), "level=ERROR")
}

func TestClaimReleasedAfterPanic(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc))
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		panic("boom")
	}))

	s.runDue(context.Background(), claimTick, claimTick)

	assert.Equal(t, claimCall{id: "rec", tick: claimTick}, recv(t, fc.released))
	waitIdle(t, s)
	assert.Len(t, fc.getReleases(), 1)
}

// TestClaimReleasedAfterSupersedeCancelsRun: a Supersede that cancels the
// claimed run releases the claim before the replacement runs.
func TestClaimReleasedAfterSupersedeCancelsRun(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc))

	started := make(chan struct{})
	require.NoError(t, s.Add("rec", Every(time.Minute), func(jobCtx context.Context) {
		close(started)
		<-jobCtx.Done()
	}))

	s.runDue(context.Background(), claimTick, claimTick)
	recv(t, started)

	releasesAtReplacement := make(chan int, 1)
	require.True(t, s.Supersede(context.Background(), "rec", func(context.Context) {
		releasesAtReplacement <- len(fc.getReleases())
	}))

	assert.Equal(t, 1, recv(t, releasesAtReplacement))
	waitIdle(t, s)
	assert.Equal(t, []claimCall{{id: "rec", tick: claimTick}}, fc.getReleases())
}

func TestClaimReleasedOnShutdownWithLiveContext(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc))

	started := make(chan struct{})
	require.NoError(t, s.Add("rec", Every(time.Minute), func(jobCtx context.Context) {
		close(started)
		<-jobCtx.Done()
	}))

	ctx, cancel := context.WithCancel(context.Background())
	s.runDue(ctx, claimTick, claimTick)
	recv(t, started)

	cancel()
	s.shutdown()

	assert.Equal(t, []claimCall{{id: "rec", tick: claimTick}}, fc.getReleases())
	fc.mu.Lock()
	releaseCtxs := append([]error(nil), fc.releaseCtxs...)
	fc.mu.Unlock()
	assert.Equal(t, []error{nil}, releaseCtxs, "Release got a cancelled context")
}

func TestClaimReleaseErrorIsLogged(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	fc.releaseErr = ez.New(ez.EUNAVAILABLE, "store down", nil)
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {}))

	s.runDue(context.Background(), claimTick, claimTick)
	waitIdle(t, s)

	assert.Len(t, fc.getReleases(), 1)
	assert.Contains(t, buf.String(), `level=ERROR msg="Scheduler claim release failed" job_id=rec`)
}

func TestClaimCatchUpClaimsLatestDueBoundary(t *testing.T) {
	at := func(h, m int) time.Time {
		return time.Date(2026, 9, 24, h, m, 0, 0, time.Local)
	}

	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc))
	noop := func(context.Context) {}
	require.NoError(t, s.Add("every-1m", Every(time.Minute), noop))
	require.NoError(t, s.Add("daily-9-17", Daily(9, 17, time.Local), noop))
	require.NoError(t, s.Add("daily-9-25", Daily(9, 25, time.Local), noop))

	s.runDue(context.Background(), at(9, 15), at(9, 20))
	waitIdle(t, s)

	lease := DefaultJobTimeout + leaseMargin
	assert.ElementsMatch(t, []claimCall{
		{id: "every-1m", tick: at(9, 20), lease: lease},
		{id: "daily-9-17", tick: at(9, 17), lease: lease},
	}, fc.getClaims())
	assert.ElementsMatch(t, []claimCall{
		{id: "every-1m", tick: at(9, 20)},
		{id: "daily-9-17", tick: at(9, 17)},
	}, fc.getReleases())
}

// TestSupersedeDuringClaimWonAfterCancel: a store that does not stop on
// cancellation wins the claim after Supersede cancelled it. The recurring job
// does not run, the claim is released, and the replacement runs.
func TestSupersedeDuringClaimWonAfterCancel(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	fc.ignoreCancel = true
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))

	var recurringRan atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		recurringRan.Store(true)
	}))

	releasesAtReplacement := make(chan int, 1)
	fc.onClaim = func(ctx context.Context, _ string) {
		s.Supersede(context.Background(), "rec", func(context.Context) {
			releasesAtReplacement <- len(fc.getReleases())
		})
		<-ctx.Done()
	}

	s.runDue(context.Background(), claimTick, claimTick)

	assert.Equal(t, 1, recv(t, releasesAtReplacement), "replacement ran before the release")
	waitIdle(t, s)
	assert.False(t, recurringRan.Load())
	assert.Equal(t, []claimCall{{id: "rec", tick: claimTick}}, fc.getReleases())
	assert.NotContains(t, buf.String(), "level=ERROR")
}

// TestShutdownDuringClaimWonAfterCancel: a store that does not stop on
// cancellation wins the claim after shutdown cancelled it. The job does not
// run, the claim is released, and nothing is logged at error level.
func TestShutdownDuringClaimWonAfterCancel(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	fc.ignoreCancel = true
	log, buf := newTestLogger()
	s := newTestScheduler(t, WithClaimer(fc), WithLogger(log))

	var ran atomic.Bool
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {
		ran.Store(true)
	}))

	claiming := make(chan struct{})
	fc.onClaim = func(ctx context.Context, _ string) {
		close(claiming)
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.runDue(ctx, claimTick, claimTick)
	recv(t, claiming)

	cancel()
	s.shutdown()

	assert.False(t, ran.Load())
	assert.Equal(t, []claimCall{{id: "rec", tick: claimTick}}, fc.getReleases())
	assert.Equal(t, int64(0), s.Active())
	assert.NotContains(t, buf.String(), "level=ERROR")
}

// TestClaimStaysOffTheMinuteLoop: a claim that blocks holds up neither
// runDue nor another job due at the same boundary.
func TestClaimStaysOffTheMinuteLoop(t *testing.T) {
	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc))

	unblock := make(chan struct{})
	fc.onClaim = func(_ context.Context, id string) {
		if id == "slow" {
			<-unblock
		}
	}

	fastRan := make(chan struct{})
	require.NoError(t, s.Add("slow", Every(time.Minute), func(context.Context) {}))
	require.NoError(t, s.Add("fast", Every(time.Minute), func(context.Context) {
		close(fastRan)
	}))

	runDueReturned := make(chan struct{})
	go func() {
		s.runDue(context.Background(), claimTick, claimTick)
		close(runDueReturned)
	}()

	recv(t, runDueReturned)
	recv(t, fastRan)
	close(unblock)
	waitIdle(t, s)
}

func TestClaimLeaseSaturates(t *testing.T) {
	assert.Equal(t, time.Hour+leaseMargin, leaseFor(time.Hour))
	assert.Equal(t, time.Duration(math.MaxInt64), leaseFor(math.MaxInt64))
	assert.Equal(t, time.Duration(math.MaxInt64), leaseFor(math.MaxInt64-leaseMargin+1))
	assert.Equal(t, time.Duration(math.MaxInt64), leaseFor(math.MaxInt64-leaseMargin))

	fc := newFakeClaimer(true, nil)
	s := newTestScheduler(t, WithClaimer(fc), WithJobTimeout(math.MaxInt64))
	require.NoError(t, s.Add("rec", Every(time.Minute), func(context.Context) {}))

	s.runDue(context.Background(), claimTick, claimTick)
	waitIdle(t, s)

	assert.Equal(t, []claimCall{{id: "rec", tick: claimTick, lease: math.MaxInt64}}, fc.getClaims())
}
