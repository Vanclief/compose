package scheduler

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vanclief/compose/components/logger"
	"github.com/vanclief/ez"
)

type (
	// Job is a unit of work to be run by the Scheduler.
	Job func(ctx context.Context)
	// scheduledJob couples a Job with its unique ID.
	scheduledJob struct {
		id  string
		job Job
	}
)

// Scheduler runs Jobs on a fixed tick schedule.
// It also supports one-shot jobs and prevents concurrent runs of the same logical task (by ID).
// For each distinct id, only one instance may run at the same time.
type Scheduler struct {
	tick            time.Duration          // e.g., 15m, 10m, 5m
	granularity     int                    // minutes per tick
	mu              sync.RWMutex           // protects slots
	slots           map[int][]scheduledJob // key: minute-of-hour (0..59)
	running         map[string]struct{}    // tracks in-flight job IDs
	runMu           sync.Mutex             // protects running
	wg              sync.WaitGroup         // tracks all spawned jobs
	log             logger.Logger
	shutdownTimeout time.Duration
	jobTimeout      time.Duration
	activeJobs      int64
	idledCh         chan struct{}
}

// New creates a Scheduler that fires every tick duration.
func New(tick time.Duration, opts ...Option) (*Scheduler, error) {
	const op = "scheduler.New"

	if tick <= 0 || tick%time.Minute != 0 {
		return nil, ez.New(op, ez.EINVALID, "tick must be a positive multiple of 1 minute", nil)
	}

	gran := int(tick / time.Minute)
	if 60%gran != 0 {
		errMsg := fmt.Sprintf("tick must divide 60 minutes evenly, got %dmin", gran)
		return nil, ez.New(op, ez.EINVALID, errMsg, nil)
	}

	s := &Scheduler{
		tick:            tick,
		granularity:     gran,
		slots:           make(map[int][]scheduledJob),
		running:         make(map[string]struct{}),
		log:             logger.Noop{},
		shutdownTimeout: DefaultShutdownTimeout,
		jobTimeout:      DefaultJobTimeout,
		idledCh:         make(chan struct{}, 1),
	}

	// initialize valid slots
	for m := 0; m < 60; m += gran {
		s.slots[m] = nil
	}

	// apply options
	for _, o := range opts {
		o(s)
	}

	return s, nil
}

// Add registers a recurring job at the given slot (must be a multiple of granularity).
// id must be unique for each logical task; concurrent duplicates will be skipped.
func (s *Scheduler) Add(id string, slot int, job Job) error {
	const op = "Scheduler.Add"

	if id == "" {
		return ez.New(op, ez.EINVALID, "job id cannot be empty", nil)
	}
	if job == nil {
		return ez.New(op, ez.EINVALID, "job cannot be nil", nil)
	}
	if slot < 0 || slot > 59 || slot%s.granularity != 0 {
		errMsg := fmt.Sprintf("invalid slot %d for tick %dmin", slot, s.granularity)
		return ez.New(op, ez.EINVALID, errMsg, nil)
	}

	s.mu.Lock()
	for _, sj := range s.slots[slot] {
		if sj.id == id {
			s.mu.Unlock()
			return ez.New(op, ez.ECONFLICT, "job with this id already exists in this slot", nil)
		}
	}

	s.slots[slot] = append(s.slots[slot], scheduledJob{id: id, job: job})
	s.mu.Unlock()
	return nil
}

// AddMany registers the same job on multiple slots. IDs must be distinct per logical task.
func (s *Scheduler) AddMany(id string, slots []int, job Job) error {
	for _, sl := range slots {
		if err := s.Add(id, sl, job); err != nil {
			return err
		}
	}
	return nil
}

// RunOnce fires a one-shot job immediately.
// Returns true if the job was started, false if skipped because it's already running or invalid.
func (s *Scheduler) RunOnce(ctx context.Context, id string, job Job) bool {
	return s.spawnJob(ctx, id, job)
}

// Idled returns a channel that receives a value whenever the scheduler drains all current jobs.
// Consumers should check Active() after receiving since new jobs may already have started.
func (s *Scheduler) Idled() <-chan struct{} {
	return s.idledCh
}

// Active returns the number of currently running jobs.
func (s *Scheduler) Active() int64 {
	return atomic.LoadInt64(&s.activeJobs)
}

// Start blocks until ctx is canceled. It aligns to the next tick boundary,
// then fires runJobs on each tick.
func (s *Scheduler) Start(ctx context.Context) {
	// align to next slot
	next := s.nextAligned(time.Now())
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		s.waitForJobs()
		return
	case <-timer.C:
	}

	// run first batch immediately, then on each tick
	s.runJobs(ctx, next)
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.waitForJobs()
			return
		case now := <-ticker.C:
			s.runJobs(ctx, now)
		}
	}
}

// waitForJobs blocks until all in-flight jobs finish or timeout.
func (s *Scheduler) waitForJobs() {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(s.shutdownTimeout):
		s.log.Warn().Msg("Scheduler: timed out waiting for jobs to finish")
	}
}

// runJobs dispatches all jobs scheduled for the current slot.
func (s *Scheduler) runJobs(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	slot := (now.Minute() / s.granularity) * s.granularity

	s.mu.RLock()
	scheduledJobs := append([]scheduledJob(nil), s.slots[slot]...)
	s.mu.RUnlock()

	for _, sj := range scheduledJobs {
		wasSpawned := s.spawnJob(ctx, sj.id, sj.job)
		if !wasSpawned {
			s.log.Debug().Str("job_id", sj.id).Msg("Job already running or invalid, skipping")
		}
	}
}

// spawnJob handles de-duplicating by id, tracking, panic recovery, and wg.
// Returns true if the job was started, false if skipped.
func (s *Scheduler) spawnJob(ctx context.Context, id string, job Job) bool {
	if id == "" || job == nil || ctx.Err() != nil {
		return false
	}

	// prevent duplicate concurrent runs
	s.runMu.Lock()
	if _, busy := s.running[id]; busy {
		s.runMu.Unlock()
		return false
	}
	s.running[id] = struct{}{}
	s.runMu.Unlock()

	atomic.AddInt64(&s.activeJobs, 1)
	s.wg.Add(1)
	start := time.Now()

	// ensure a default timeout if none is set
	jobCtx := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		jobCtx, cancel = context.WithTimeout(ctx, s.jobTimeout)
	} else {
		cancel = func() {}
	}

	go func() {
		s.log.Info().Str("job_id", id).Time("start", start).Msg("Scheduler job started")
		defer s.wg.Done()
		defer cancel()
		defer func() {
			// panic recovery
			if r := recover(); r != nil {
				s.log.Error().
					Time("start", start).
					Dur("duration", time.Since(start)).
					Any("panic", r).
					Bytes("stack", debug.Stack()).
					Msg("Scheduler job panic")
			}
			// cleanup running flag
			s.runMu.Lock()
			delete(s.running, id)
			s.runMu.Unlock()
			s.log.Info().Str("job_id", id).Time("end", time.Now()).Dur("duration", time.Since(start)).Msg("Scheduler job finished")
			if atomic.AddInt64(&s.activeJobs, -1) == 0 {
				if ch := s.idledCh; ch != nil {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
			}
		}()
		job(jobCtx)
	}()

	return true
}

// nextAligned returns the next time aligned to s.tick (never returns t itself).
func (s *Scheduler) nextAligned(t time.Time) time.Time {
	t = t.Truncate(time.Second)
	hourStart := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
	elapsed := t.Sub(hourStart)
	rem := elapsed % s.tick

	var wait time.Duration
	if rem == 0 {
		wait = s.tick
	} else {
		wait = s.tick - rem
	}
	return t.Add(wait)
}

func ShouldRunLocalHour(tz string, hour int) bool { return ShouldRunLocalNow(tz, hour, 0) }

// ShouldRunLocalNow returns true if the local time in tzName matches hour:minute exactly.
func ShouldRunLocalNow(tz string, hour, minute int) bool {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return false
	}
	now := time.Now().In(loc)
	return now.Hour() == hour && now.Minute() == minute
}
