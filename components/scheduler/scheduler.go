package scheduler

import (
	"context"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/vanclief/compose/components/logger"
	"github.com/vanclief/ez"
)

// Job is a unit of work run by the Scheduler. It should return promptly once
// ctx is done.
type Job func(ctx context.Context)

// ErrSuperseded is the context cause seen by a job that was cancelled because
// Supersede queued a replacement for the same id. errors.Is(context.Cause(ctx),
// ErrSuperseded) and errors.Is(context.Cause(ctx), context.Canceled) both hold.
var ErrSuperseded error = ez.New(ez.ECANCELED, "scheduler: superseded", context.Canceled)

type registration struct {
	id   string
	when Schedule
	job  Job
}

type runningJob struct {
	cancel  context.CancelCauseFunc // cancels the CURRENT invocation
	pending *pendingJob             // at most one; shared by Supersede and RunNext
}

type pendingJob struct {
	ctx context.Context
	job Job
}

type limit struct {
	prefix  string
	permits chan struct{} // buffered, capacity n; nil when n < 1 so New can reject it
}

type admitMode int

const (
	admitSkip      admitMode = iota // RunOnce and recurring: busy id → false, pending untouched
	admitNext                       // RunNext: busy id → install pending, no cancel
	admitSupersede                  // Supersede: busy id → install pending, cancel current with ErrSuperseded
)

// Scheduler runs recurring jobs on wall-clock minute boundaries and one-shot
// jobs on demand. Work is keyed by id: at most one invocation per id runs at a
// time, and each busy id holds at most one pending job that runs after the
// current one returns. Create it with New.
type Scheduler struct {
	mu              sync.RWMutex // protects registrations
	registrations   []registration
	runMu           sync.Mutex // protects running, stopping, idled send
	running         map[string]*runningJob
	stopping        bool
	limits          []limit
	wg              sync.WaitGroup // one Add(1) per busy id for as long as it has work
	idledCh         chan struct{}  // buffer 1
	log             logger.Logger
	shutdownTimeout time.Duration
	jobTimeout      time.Duration
	now             func() time.Time // time.Now by default; tests override to steer the minute loop
}

// New creates a Scheduler with the given options. It returns an ez.EINVALID
// error when a WithLimit option has an empty prefix, a size below 1, a prefix
// given twice, or a prefix that is a prefix of another limit's prefix.
func New(opts ...Option) (*Scheduler, error) {
	s := &Scheduler{
		running:         make(map[string]*runningJob),
		idledCh:         make(chan struct{}, 1),
		log:             logger.Noop{},
		shutdownTimeout: DefaultShutdownTimeout,
		jobTimeout:      DefaultJobTimeout,
		now:             time.Now,
	}

	for _, o := range opts {
		o(s)
	}

	for i, l := range s.limits {
		if l.prefix == "" {
			return nil, ez.New(ez.EINVALID, "scheduler: limit prefix cannot be empty", nil)
		}
		if l.permits == nil {
			msg := fmt.Sprintf("scheduler: limit for prefix %q must allow at least 1 job", l.prefix)
			return nil, ez.New(ez.EINVALID, msg, nil)
		}
		for _, other := range s.limits[:i] {
			if other.prefix == l.prefix {
				msg := fmt.Sprintf("scheduler: limit prefix %q given more than once", l.prefix)
				return nil, ez.New(ez.EINVALID, msg, nil)
			}
			if strings.HasPrefix(l.prefix, other.prefix) || strings.HasPrefix(other.prefix, l.prefix) {
				msg := fmt.Sprintf("scheduler: limit prefixes %q and %q overlap", other.prefix, l.prefix)
				return nil, ez.New(ez.EINVALID, msg, nil)
			}
		}
	}

	return s, nil
}

// Add registers job to run at the minute boundaries selected by when. It
// returns an ez.EINVALID error for an empty id, a nil job or an invalid
// Schedule, and an ez.ECONFLICT error when a recurring job with the same id is
// already registered; on error nothing is registered. Add may be called before
// or after Start. The id is shared with RunOnce, Supersede and RunNext: a due
// boundary is skipped while the id is busy.
//
// Daily schedules follow wall time in their location: on a DST spring-forward
// day a nonexistent local time does not fire, and on a fall-back day a
// repeated local time fires at both instants.
func (s *Scheduler) Add(id string, when Schedule, job Job) error {
	if id == "" {
		return ez.New(ez.EINVALID, "scheduler: job id cannot be empty", nil)
	}
	if job == nil {
		return ez.New(ez.EINVALID, "scheduler: job cannot be nil", nil)
	}

	err := when.validate()
	if err != nil {
		return ez.Wrap(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, r := range s.registrations {
		if r.id == id {
			return ez.New(ez.ECONFLICT, "scheduler: recurring job with this id already exists", nil)
		}
	}

	s.registrations = append(s.registrations, registration{id: id, when: when, job: job})
	return nil
}

// Start runs the minute engine and blocks until ctx is done. It sleeps until
// the next wall-clock minute boundary, runs the recurring jobs due at it, and
// repeats. It never fires immediately: the first boundary evaluated is the
// first one after Start is called.
//
// A late wake, because the process was frozen or the clock jumped forward,
// catches up: every minute boundary between the last one handled and the
// clock is evaluated, and a registration due at any of them is admitted once.
// A due job whose id is still busy is skipped for that wake. Nothing is
// remembered across restarts, so minutes that pass while the process is not
// running are never replayed. If the clock is stepped back by a minute or
// more the engine follows it and evaluates those minutes again as they pass.
//
// When ctx is done Start stops the Scheduler: nothing new is admitted, pending
// jobs are discarded, running jobs are cancelled with context.Canceled, and
// Start waits for them up to the shutdown timeout before returning. Jobs that
// ignore their context may outlive that wait. Call Start once; once it has
// returned, a later call returns immediately.
func (s *Scheduler) Start(ctx context.Context) {
	s.runMu.Lock()
	stopping := s.stopping
	s.runMu.Unlock()
	if stopping {
		return
	}

	// The current minute counts as handled, so nothing fires immediately.
	now := s.now()
	last := now.Truncate(time.Minute)
	for {
		timer := time.NewTimer(last.Add(time.Minute).Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			s.shutdown()
			return
		case <-timer.C:
		}

		last = s.catchUp(ctx, last, s.now())

		// Read the clock again so the time dispatch took does not stretch
		// the next wait.
		now = s.now()
	}
}

// catchUp evaluates every minute boundary after last up to the clock reading
// now and returns the last boundary handled. When the clock still reads inside
// the minute of last, because it was slewed back a few milliseconds during the
// wait, nothing is evaluated and last is returned, so the loop re-arms for the
// same boundary. When the clock reads a minute or more before last it was
// stepped back: the clock minute is returned and the minutes in between are
// evaluated again as the clock passes them.
func (s *Scheduler) catchUp(ctx context.Context, last, now time.Time) time.Time {
	clockMinute := now.Truncate(time.Minute)
	if clockMinute.Before(last) {
		s.log.Warn().Time("last", last).Time("now", now).Msg("Scheduler clock stepped back, following it")
		return clockMinute
	}
	if clockMinute.Equal(last) {
		return last
	}

	first := last.Add(time.Minute)
	if clockMinute.After(first) {
		missed := int(clockMinute.Sub(first) / time.Minute)
		s.log.Warn().Time("from", first).Time("through", clockMinute).Int("missed_minutes", missed).Msg("Scheduler woke late, catching up")
	}

	s.runDue(ctx, first, clockMinute)
	return clockMinute
}

// runDue admits, once each, the registrations due at any minute boundary from
// first through last inclusive.
func (s *Scheduler) runDue(ctx context.Context, first, last time.Time) {
	if ctx.Err() != nil {
		return
	}

	s.mu.RLock()
	registrations := append([]registration(nil), s.registrations...)
	s.mu.RUnlock()

	for _, r := range registrations {
		if !dueBetween(r.when, first, last) {
			continue
		}
		admitted := s.admit(ctx, r.id, r.job, admitSkip)
		if !admitted {
			s.log.Debug().Str("job_id", r.id).Msg("Job already running, skipping")
		}
	}
}

// dueBetween reports whether when matches any minute boundary from first
// through last inclusive.
func dueBetween(when Schedule, first, last time.Time) bool {
	for b := first; !b.After(last); b = b.Add(time.Minute) {
		if when.matches(b) {
			return true
		}
	}
	return false
}

// RunOnce starts job under id now. It returns false without running anything
// when id is empty, job is nil, ctx is already done, the Scheduler is
// stopping, or id is busy; a busy id's pending job is left untouched.
func (s *Scheduler) RunOnce(ctx context.Context, id string, job Job) bool {
	return s.admit(ctx, id, job, admitSkip)
}

// Supersede starts job under id now when id is idle. When id is busy it
// replaces the id's pending job with job, cancels the current invocation with
// cause ErrSuperseded and returns true without waiting; job starts after the
// current invocation returns. It returns false when id is empty, job is nil,
// ctx is already done, or the Scheduler is stopping.
func (s *Scheduler) Supersede(ctx context.Context, id string, job Job) bool {
	return s.admit(ctx, id, job, admitSupersede)
}

// RunNext starts job under id now when id is idle. When id is busy it replaces
// the id's pending job with job and returns true without cancelling the
// current invocation; job starts after the current invocation returns. It
// returns false when id is empty, job is nil, ctx is already done, or the
// Scheduler is stopping. A pending job whose ctx is done by then is dropped.
func (s *Scheduler) RunNext(ctx context.Context, id string, job Job) bool {
	return s.admit(ctx, id, job, admitNext)
}

// admit starts job on an idle id or, depending on mode, queues it as the busy
// id's pending job. It reports whether the job was started or queued.
func (s *Scheduler) admit(ctx context.Context, id string, job Job, mode admitMode) bool {
	if id == "" || job == nil || ctx.Err() != nil {
		return false
	}

	s.runMu.Lock()
	if s.stopping || ctx.Err() != nil {
		s.runMu.Unlock()
		return false
	}

	rj, busy := s.running[id]
	if !busy {
		jobCtx, cancel := context.WithCancelCause(ctx)
		s.running[id] = &runningJob{cancel: cancel}
		s.wg.Add(1)
		s.runMu.Unlock()
		go s.own(jobCtx, id, cancel, job)
		return true
	}

	if mode == admitSkip {
		s.runMu.Unlock()
		return false
	}

	rj.pending = &pendingJob{ctx: ctx, job: job}
	if mode == admitSupersede {
		rj.cancel(ErrSuperseded)
	}
	s.runMu.Unlock()
	return true
}

// own is the owner goroutine of a busy id. It runs the current invocation,
// then promotes the pending job, if any, until there is nothing left to run.
func (s *Scheduler) own(ctx context.Context, id string, cancel context.CancelCauseFunc, job Job) {
	for {
		s.invoke(ctx, id, job)
		cancel(nil)

		s.runMu.Lock()
		rj := s.running[id]
		p := rj.pending
		rj.pending = nil

		if s.stopping || p == nil || p.ctx.Err() != nil {
			if p != nil {
				s.log.Debug().Str("job_id", id).Msg("Scheduler pending job dropped")
			}
			delete(s.running, id)
			if len(s.running) == 0 {
				select {
				case s.idledCh <- struct{}{}:
				default:
				}
			}
			s.runMu.Unlock()
			s.wg.Done()
			return
		}

		ctx, cancel = context.WithCancelCause(p.ctx)
		rj.cancel = cancel
		job = p.job
		s.runMu.Unlock()
	}
}

// invoke acquires the id's permit, if a limit applies, and runs job with the
// job timeout applied. The permit is released when job returns.
func (s *Scheduler) invoke(ctx context.Context, id string, job Job) {
	lim := s.limitFor(id)
	if lim != nil {
		if ctx.Err() != nil {
			s.log.Debug().Str("job_id", id).Msg("Scheduler job cancelled before permit")
			return
		}

		select {
		case lim.permits <- struct{}{}:
		case <-ctx.Done():
			s.log.Debug().Str("job_id", id).Msg("Scheduler job cancelled before permit")
			return
		}

		if ctx.Err() != nil {
			<-lim.permits
			s.log.Debug().Str("job_id", id).Msg("Scheduler job cancelled before permit")
			return
		}
		defer func() {
			<-lim.permits
		}()
	}

	// The timeout starts here, after the permit, so waiting for a permit does
	// not eat into the job's time.
	jobCtx := ctx
	_, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		var timeoutCancel context.CancelFunc
		jobCtx, timeoutCancel = context.WithTimeout(ctx, s.jobTimeout)
		defer timeoutCancel()
	}

	s.runJob(jobCtx, id, job)
}

// limitFor returns the limit whose prefix is a prefix of id, or nil. New
// rejects overlapping prefixes, so at most one limit matches.
func (s *Scheduler) limitFor(id string) *limit {
	for i := range s.limits {
		if strings.HasPrefix(id, s.limits[i].prefix) {
			return &s.limits[i]
		}
	}
	return nil
}

// runJob runs job, recovering and logging a panic, and logs its start and end.
func (s *Scheduler) runJob(ctx context.Context, id string, job Job) {
	start := time.Now()
	s.log.Info().Str("job_id", id).Time("start", start).Msg("Scheduler job started")

	defer func() {
		r := recover()
		if r != nil {
			s.log.Error().
				Str("job_id", id).
				Time("start", start).
				Dur("duration", time.Since(start)).
				Any("panic", r).
				Bytes("stack", debug.Stack()).
				Msg("Scheduler job panic")
		}
		s.log.Info().Str("job_id", id).Time("end", time.Now()).Dur("duration", time.Since(start)).Msg("Scheduler job finished")
	}()

	job(ctx)
}

// shutdown stops admission, discards pending jobs, cancels running jobs and
// waits for them up to the shutdown timeout.
func (s *Scheduler) shutdown() {
	s.runMu.Lock()
	s.stopping = true
	cancels := make([]context.CancelCauseFunc, 0, len(s.running))
	for _, rj := range s.running {
		rj.pending = nil
		cancels = append(cancels, rj.cancel)
	}
	s.runMu.Unlock()

	for _, cancel := range cancels {
		cancel(context.Canceled)
	}

	s.waitForJobs()
}

// waitForJobs blocks until all in-flight jobs finish or timeout.
func (s *Scheduler) waitForJobs() {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	timer := time.NewTimer(s.shutdownTimeout)
	defer timer.Stop()

	select {
	case <-done:
	case <-timer.C:
		s.log.Warn().Msg("Scheduler: timed out waiting for jobs to finish")
	}
}

// Idled returns a channel that receives a value when the last busy id finishes
// and Active drops to zero. It has a buffer of one and sends never block, so a
// receiver may see one value for several idle transitions. It does not fire
// between a job and the pending job that follows it. Check Active after
// receiving, since new work may already have been admitted.
func (s *Scheduler) Idled() <-chan struct{} {
	return s.idledCh
}

// Active returns the number of ids with admitted work: running, or waiting for
// a permit from a WithLimit option. A pending job adds nothing to the count.
func (s *Scheduler) Active() int64 {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	return int64(len(s.running))
}

// ShouldRunLocalHour returns true if the local time in tz is exactly hour:00.
func ShouldRunLocalHour(tz string, hour int) bool {
	return ShouldRunLocalTime(tz, hour, 0)
}

// ShouldRunLocalTime returns true if the local time in tzName matches hour:minute exactly.
func ShouldRunLocalTime(tz string, hour, minute int) bool {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return false
	}
	now := time.Now().In(loc)
	return now.Hour() == hour && now.Minute() == minute
}
