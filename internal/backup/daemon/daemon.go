// Package daemon runs `elythia backup daemon` (#3460): it takes, verifies and
// prunes backups on a schedule, reports failures, mismatches and delays, and
// serves a control API that the main server's admin page uses (#3462).
//
// 取る処理 (#3458) と確かめる処理 (#3459) は Taker / Verifier の形だけに依存する。
// 1 つのプロセスの中では、取る・確かめる・消すを同時に 1 つしか走らせない。
// 確かめている世代を消す、取っている途中の世代を半端と見て消す、といった
// 取り合いを起こさないため。
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
)

// Taker takes one backup and stores it as a new generation (#3458). It
// returns the meta.json it wrote; an error means no complete generation was
// made.
type Taker interface {
	Take(ctx context.Context) (backup.Meta, error)
}

// Verifier verifies generation id (#3459). It must store the result as
// verify.json in the generation before returning a nil error, because
// pruning reads verify.json from the storage. A nil error with OK false means
// the verification ran and found the backup broken; an error means it could
// not run to the end.
type Verifier interface {
	Verify(ctx context.Context, id string) (backup.VerifyResult, error)
}

// Clock is the time source. Tests replace it.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) *time.Timer
}

type realClock struct{}

func (realClock) Now() time.Time                       { return time.Now() }
func (realClock) NewTimer(d time.Duration) *time.Timer { return time.NewTimer(d) }

// JobKind names what a job does.
type JobKind string

// Job kinds.
const (
	// JobTake takes a backup, verifies it when the schedule says so, and
	// prunes old generations.
	JobTake JobKind = "take"
	// JobVerify verifies one existing generation.
	JobVerify JobKind = "verify"
)

// Trigger is what started a job.
type Trigger string

// Job triggers.
const (
	TriggerSchedule Trigger = "schedule"
	TriggerAPI      Trigger = "api"
)

// Job is a running job.
type Job struct {
	Kind    JobKind `json:"kind"`
	Trigger Trigger `json:"trigger"`
	// GenerationID is the target of a verify job. For a take job it is set
	// once the backup has been taken.
	GenerationID string    `json:"generationId,omitempty"`
	StartedAt    time.Time `json:"startedAt"`
}

// JobResult is a finished job.
type JobResult struct {
	Job
	FinishedAt time.Time `json:"finishedAt"`
	// OK is true when every step succeeded and the verification (if run)
	// passed.
	OK bool `json:"ok"`
	// Stage is the step that failed: "take", "verify" or "prune".
	Stage string `json:"stage,omitempty"`
	Error string `json:"error,omitempty"`
	// Verify is the verification result when one was produced.
	Verify *backup.VerifyResult `json:"verify,omitempty"`
	// Deleted lists the generations pruned by a take job.
	Deleted []string `json:"deleted,omitempty"`
}

// Options configures a Daemon.
type Options struct {
	// Schedule is nil when periodic backups are off; the control API still
	// works.
	Schedule *Schedule
	Storage  backup.Storage
	Taker    Taker
	Verifier Verifier
	// Notifier is nil when no webhook is configured.
	Notifier Notifier
	// Instance is the server URL put into notifications.
	Instance string
	Logger   *slog.Logger
	Clock    Clock
	// RequireVerified makes only verified generations count as usable even
	// without a schedule. It defaults to Schedule.Verify.
	RequireVerified bool
}

// ErrBusy is returned by Start when another job is running.
var ErrBusy = errors.New("backup: another job is running")

// ErrNotRunning is returned by Start before Run or after Run began stopping.
var ErrNotRunning = errors.New("backup: daemon is not running")

// Daemon is the scheduler. Create it with New and run it with Run.
type Daemon struct {
	opts   Options
	log    *slog.Logger
	clock  Clock
	notify Notifier
	// kick wakes Run after a job finished so it recomputes its timers.
	kick chan struct{}

	mu      sync.Mutex
	ctx     context.Context // Run の ctx。Run の前に Start されたら拒む
	wg      sync.WaitGroup
	running *Job
	// stopping is set once Run's ctx is done; Start refuses new jobs.
	stopping bool
	// pending is a scheduled take that came due while another job ran.
	pending      bool
	nextRun      time.Time
	lastTake     *JobResult
	lastVerify   *JobResult
	latestUsable *Generation
	// usableBase is when the delay clock started: the newest usable
	// generation, or the daemon start when there is none.
	usableBase      time.Time
	nextDelayAlert  time.Time
	lastNotifyError string
}

// New returns a Daemon.
func New(o Options) *Daemon {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = realClock{}
	}
	if o.Schedule != nil && o.Schedule.Verify {
		o.RequireVerified = true
	}
	n := o.Notifier
	if n == nil {
		n = nopNotifier{}
	}
	return &Daemon{opts: o, log: o.Logger, clock: o.Clock, notify: n, kick: make(chan struct{}, 1)}
}

// Run runs the schedule until ctx is done, then waits for the running job
// (whose context is ctx, so it is cancelled too).
func (d *Daemon) Run(ctx context.Context) {
	now := d.clock.Now()
	gens, err := Scan(ctx, d.opts.Storage)
	if err != nil {
		// 保存先に届かなくても止まらない。取る段で同じ理由で落ち、通知が出る。
		d.log.Error("backup: cannot list generations at start", "error", err)
	}
	d.mu.Lock()
	d.ctx = ctx
	d.usableBase = now
	d.setLatestLocked(gens, true)
	sched := d.opts.Schedule
	if sched != nil {
		var latest *time.Time
		if g := Latest(gens, func(g Generation) bool { return g.Complete }); g != nil {
			latest = &g.Time
		}
		d.nextRun = sched.Start(now, latest)
		d.log.Info("backup: schedule started",
			"interval", sched.Interval.String(), "at", sched.AtString(), "timezone", sched.Location.String(),
			"keep", sched.Keep, "verify", sched.Verify, "next", d.nextRun.Format(time.RFC3339))
	}
	d.mu.Unlock()

	for {
		var timerC <-chan time.Time
		var timer *time.Timer
		if sched != nil {
			d.mu.Lock()
			wake := d.nextRun
			if d.nextDelayAlert.Before(wake) {
				wake = d.nextDelayAlert
			}
			d.mu.Unlock()
			timer = d.clock.NewTimer(max(wake.Sub(d.clock.Now()), 0))
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			d.mu.Lock()
			d.stopping = true
			d.mu.Unlock()
			d.wg.Wait()
			return
		case <-timerC:
			d.onTimer(ctx)
		case <-d.kick:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

// onTimer starts a due scheduled take and sends a due delay alert.
func (d *Daemon) onTimer(ctx context.Context) {
	now := d.clock.Now()
	sched := d.opts.Schedule
	d.mu.Lock()
	if !now.Before(d.nextRun) {
		d.nextRun = sched.Next(now)
		if d.running == nil {
			d.startLocked(JobTake, TriggerSchedule, "", now)
		} else {
			// 手で頼まれた検証などが走っている。終わったらすぐ取る。
			d.pending = true
			d.log.Info("backup: scheduled backup deferred until the running job ends", "running", d.running.Kind)
		}
	}
	var delay *Event
	if !now.Before(d.nextDelayAlert) {
		// 遅れが続く間は、DelayAfter ごとに繰り返し知らせる。
		for !now.Before(d.nextDelayAlert) {
			d.nextDelayAlert = d.nextDelayAlert.Add(sched.DelayAfter)
		}
		e := Event{Kind: EventDelay, OccurredAt: now,
			Message: fmt.Sprintf("no usable backup since the daemon started at %s", d.usableBase.UTC().Format(time.RFC3339))}
		if d.latestUsable != nil {
			t := d.latestUsable.Time
			e.LastUsableAt = &t
			e.GenerationID = d.latestUsable.ID
			e.Message = fmt.Sprintf("the newest usable backup is from %s", t.UTC().Format(time.RFC3339))
		}
		delay = &e
	}
	d.mu.Unlock()
	if delay != nil {
		d.send(ctx, *delay)
	}
}

// setLatestLocked records the newest usable generation and, when it is newer
// than the delay clock (or initial), restarts the delay clock from it. d.mu
// must be held.
//
// 遅れの起点は、新しい世代ができたときだけ動かす。取るのに失敗するたびに起点から
// 数え直すと、知らせた直後の遅れをもう一度知らせてしまう。
func (d *Daemon) setLatestLocked(gens []Generation, initial bool) {
	g := Latest(gens, func(g Generation) bool { return g.Usable(d.opts.RequireVerified) })
	if g == nil {
		d.latestUsable = nil
	} else {
		cp := *g
		d.latestUsable = &cp
	}
	moved := initial
	if g != nil && (initial || g.Time.After(d.usableBase)) {
		d.usableBase = g.Time
		moved = true
	}
	if moved && d.opts.Schedule != nil {
		d.nextDelayAlert = d.usableBase.Add(d.opts.Schedule.DelayAfter)
	}
}

// Start starts a job requested through the control API. It returns ErrBusy
// when another job is running.
func (d *Daemon) Start(kind JobKind, id string) (Job, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx == nil || d.stopping {
		return Job{}, ErrNotRunning
	}
	if d.running != nil {
		return *d.running, ErrBusy
	}
	return d.startLocked(kind, TriggerAPI, id, d.clock.Now()), nil
}

func (d *Daemon) startLocked(kind JobKind, trigger Trigger, id string, now time.Time) Job {
	job := Job{Kind: kind, Trigger: trigger, GenerationID: id, StartedAt: now}
	d.running = &job
	d.wg.Add(1)
	ctx := d.ctx
	go func() {
		defer d.wg.Done()
		res := d.runJob(ctx, job)
		d.finish(ctx, res)
	}()
	return job
}

func (d *Daemon) runJob(ctx context.Context, job Job) JobResult {
	res := JobResult{Job: job}
	switch job.Kind {
	case JobTake:
		d.log.Info("backup: taking a backup", "trigger", job.Trigger)
		meta, err := d.opts.Taker.Take(ctx)
		if err != nil {
			d.fail(ctx, &res, "take", err)
			return res
		}
		res.GenerationID = meta.ID
		d.setRunningID(meta.ID)
		if d.opts.Schedule != nil && d.opts.Schedule.Verify {
			if !d.verify(ctx, &res) {
				return res
			}
		}
		deleted, err := d.prune(ctx)
		res.Deleted = deleted
		if err != nil {
			d.fail(ctx, &res, "prune", err)
			return res
		}
	case JobVerify:
		if !d.verify(ctx, &res) {
			return res
		}
	}
	res.OK = true
	res.FinishedAt = d.clock.Now()
	d.log.Info("backup: job finished", "kind", job.Kind, "generation", res.GenerationID, "deleted", res.Deleted)
	return res
}

func (d *Daemon) setRunningID(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running != nil {
		d.running.GenerationID = id
	}
}

// verify runs the verifier on res.GenerationID and reports whether it passed.
func (d *Daemon) verify(ctx context.Context, res *JobResult) bool {
	vr, err := d.opts.Verifier.Verify(ctx, res.GenerationID)
	if err != nil {
		d.fail(ctx, res, "verify", err)
		return false
	}
	res.Verify = &vr
	if vr.OK {
		return true
	}
	res.Stage = "verify"
	res.Error = "verification failed"
	res.FinishedAt = d.clock.Now()
	e := Event{Kind: EventMismatch, OccurredAt: res.FinishedAt, GenerationID: res.GenerationID,
		Message: "the backup did not pass verification", Mismatches: vr.Mismatches}
	for _, s := range vr.Stages {
		if !s.OK && !s.Skipped {
			e.FailedStages = append(e.FailedStages, s)
		}
	}
	d.log.Error("backup: verification failed", "generation", res.GenerationID, "stages", e.FailedStages, "mismatches", vr.Mismatches)
	d.send(ctx, e)
	return false
}

func (d *Daemon) fail(ctx context.Context, res *JobResult, stage string, err error) {
	res.Stage = stage
	res.Error = err.Error()
	res.FinishedAt = d.clock.Now()
	d.log.Error("backup: job failed", "kind", res.Kind, "stage", stage, "generation", res.GenerationID, "error", err)
	d.send(ctx, Event{Kind: EventFailure, OccurredAt: res.FinishedAt, GenerationID: res.GenerationID,
		Stage: stage, Message: truncateRunes(err.Error(), messageLimit)})
}

// prune deletes generations PlanPrune selects and returns their IDs.
func (d *Daemon) prune(ctx context.Context) ([]string, error) {
	keep := 0
	if d.opts.Schedule != nil {
		keep = d.opts.Schedule.Keep
	}
	gens, err := Scan(ctx, d.opts.Storage)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, g := range PlanPrune(gens, keep, d.opts.RequireVerified) {
		if err := DeleteGeneration(ctx, d.opts.Storage, g); err != nil {
			return deleted, err
		}
		deleted = append(deleted, g.ID)
		d.log.Info("backup: deleted an old generation", "generation", g.ID, "verify", g.Verify, "complete", g.Complete)
	}
	return deleted, nil
}

// finish records res, refreshes the newest usable generation and starts a
// deferred scheduled take.
func (d *Daemon) finish(ctx context.Context, res JobResult) {
	gens, err := Scan(ctx, d.opts.Storage)
	if err != nil && ctx.Err() == nil {
		d.log.Error("backup: cannot list generations", "error", err)
	}
	d.mu.Lock()
	r := res
	if r.Kind == JobTake {
		d.lastTake = &r
	} else {
		d.lastVerify = &r
	}
	d.running = nil
	if err == nil {
		d.setLatestLocked(gens, false)
	}
	if d.pending && ctx.Err() == nil {
		d.pending = false
		d.startLocked(JobTake, TriggerSchedule, "", d.clock.Now())
	}
	d.mu.Unlock()
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

func (d *Daemon) send(ctx context.Context, e Event) {
	e.Instance = d.opts.Instance
	err := d.notify.Notify(ctx, e)
	d.mu.Lock()
	if err != nil {
		d.lastNotifyError = err.Error()
	} else {
		d.lastNotifyError = ""
	}
	d.mu.Unlock()
	if err != nil {
		d.log.Error("backup: cannot send a notification", "event", e.Kind, "error", err)
	}
}

// ScheduleInfo is the schedule part of Status.
type ScheduleInfo struct {
	Interval   string `json:"interval"`
	At         string `json:"at,omitempty"`
	Timezone   string `json:"timezone"`
	Keep       int    `json:"keep"`
	Verify     bool   `json:"verify"`
	DelayAfter string `json:"delayAfter"`
}

// GenerationRef points at a generation.
type GenerationRef struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// Status is the body of GET /status.
type Status struct {
	Now time.Time `json:"now"`
	// Schedule is null when periodic backups are off.
	Schedule *ScheduleInfo `json:"schedule"`
	Running  *Job          `json:"running"`
	// Pending is true when a scheduled take waits for the running job.
	Pending    bool       `json:"pending"`
	NextRunAt  *time.Time `json:"nextRunAt"`
	LastTake   *JobResult `json:"lastTake"`
	LastVerify *JobResult `json:"lastVerify"`
	// LatestUsable is the newest generation that counts for retention and
	// delay checks.
	LatestUsable *GenerationRef `json:"latestUsable"`
	// Overdue is true when no usable generation was made within DelayAfter.
	Overdue         bool   `json:"overdue"`
	LastNotifyError string `json:"lastNotifyError,omitempty"`
}

// Status returns a snapshot of the daemon's state.
func (d *Daemon) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.Now()
	st := Status{Now: now, Pending: d.pending, LastNotifyError: d.lastNotifyError}
	if d.running != nil {
		j := *d.running
		st.Running = &j
	}
	if d.lastTake != nil {
		r := *d.lastTake
		st.LastTake = &r
	}
	if d.lastVerify != nil {
		r := *d.lastVerify
		st.LastVerify = &r
	}
	if d.latestUsable != nil {
		st.LatestUsable = &GenerationRef{ID: d.latestUsable.ID, At: d.latestUsable.Time}
	}
	if s := d.opts.Schedule; s != nil {
		st.Schedule = &ScheduleInfo{Interval: s.Interval.String(), At: s.AtString(), Timezone: s.Location.String(),
			Keep: s.Keep, Verify: s.Verify, DelayAfter: s.DelayAfter.String()}
		if !d.nextRun.IsZero() {
			t := d.nextRun
			st.NextRunAt = &t
		}
		st.Overdue = !d.usableBase.IsZero() && now.Sub(d.usableBase) >= s.DelayAfter
	}
	return st
}
