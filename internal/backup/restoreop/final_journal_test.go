package restoreop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
)

// TestFinalStep_CarriesItsEnding checks that the journal line telling how an
// operation ended is already there when its step becomes final: the admin
// page stops reading once Status says final, so a line written after the
// step is not shown (#3600).
func TestFinalStep_CarriesItsEnding(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		final string
		want  string
		run   func(t *testing.T) (*env, string)
	}{
		{"done", StepDone, "restored generation 20261010T120000Z", func(t *testing.T) (*env, string) {
			e := newEnv(t)
			e.process("p1")
			rec, _ := e.prepare()
			require.NoError(t, e.m.Confirm(ctx, rec.OpID))
			return e, rec.OpID
		}},
		{"aborted while ready", StepAborted, "aborted; dropped " + testDB + "_restore_", func(t *testing.T) (*env, string) {
			e := newEnv(t)
			rec, _ := e.prepare()
			require.NoError(t, e.m.Abort(ctx, rec.OpID))
			return e, rec.OpID
		}},
		{"failed", StepFailed, "restoreInto failed", func(t *testing.T) (*env, string) {
			e := newEnv(t)
			e.db.fail["restoreInto"] = errors.New("restoreInto failed")
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(t, err)
			return e, rec.OpID
		}},
		{"expired", StepExpired, "nobody confirmed within", func(t *testing.T) (*env, string) {
			e := newEnv(t, func(o *Options) { o.ReadyTimeout = 30 * time.Millisecond })
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(t, err)
			return e, rec.OpID
		}},
		{"switch refused", StepAborted, "the current database app was not changed", func(t *testing.T) (*env, string) {
			e := newEnv(t)
			e.process("p1")
			e.db.switchErr = func(int) error { return errors.New("database \"app\" is being accessed by other users") }
			rec, _ := e.prepare()
			require.NoError(t, e.m.Confirm(ctx, rec.OpID))
			return e, rec.OpID
		}},
		{"reverted", StepReverted, "reverted to the previous database", func(t *testing.T) (*env, string) {
			e := newEnv(t)
			e.process("p1")
			e.db.fail["check"] = errors.New("check failed")
			rec, _ := e.prepare()
			require.NoError(t, e.m.Confirm(ctx, rec.OpID))
			return e, rec.OpID
		}},
		{"handed to the operator", "", "stopped and left the maintenance on", func(t *testing.T) (*env, string) {
			e := newEnv(t)
			e.process("p1")
			e.db.switchErr = func(int) error { return backup.ErrRestoreSwitchUnknown }
			rec, _ := e.prepare()
			require.NoError(t, e.m.Confirm(ctx, rec.OpID))
			return e, rec.OpID
		}},
		{"recovered before the switch", StepAborted, "the current database app was not changed", func(t *testing.T) (*env, string) {
			e, rec := crashed(t, StepReady, map[string]string{testDB: "current", tgt: "restored"}, "", "")
			return e, rec.OpID
		}},
		{"recovered with the finished mark", StepDone, "is marked as kept by this finished operation", func(t *testing.T) (*env, string) {
			e, rec := crashedFinished(t)
			return e, rec.OpID
		}},
		{"recovered after the judge cleared", StepDone, "the processes already serve the restored database", func(t *testing.T) (*env, string) {
			e, rec := crashed(t, StepResuming, map[string]string{testDB: "restored", before: "current"}, "", "")
			return e, rec.OpID
		}},
		{"rolled back", StepDone, "rolled back to " + keptDB, func(t *testing.T) (*env, string) {
			e := newEnv(t)
			e.withKept()
			e.process("p1")
			rec, _ := e.rollback()
			require.NoError(t, e.m.Confirm(ctx, rec.OpID))
			return e, rec.OpID
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, opID := c.run(t)
			e.waitStep(opID, StepDone, StepFailed, StepExpired, StepAborted, StepReverted, StepUnknown)
			j := e.finalJournal(opID)
			require.NotEmpty(t, j)
			last := j[len(j)-1]
			assert.Contains(t, last.Message, c.want)
			if c.final != "" {
				assert.Equal(t, c.final, last.Step)
			}
		})
	}
}

// TestFinalStep_NotWrittenStillJournalsTheEnding checks that the ending line
// is appended on its own, exactly once, when the final step cannot be
// written: through recordWithLine (the expiry) and through prepare (a
// failed preparation).
func TestFinalStep_NotWrittenStillJournalsTheEnding(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, to, want string
		start          func(e *env) string
	}{
		{"recordWithLine", StepExpired, "nobody confirmed within", func(e *env) string {
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(e.t, err)
			return rec.OpID
		}},
		{"prepare", StepFailed, "restoreInto failed", func(e *env) string {
			e.db.fail["restoreInto"] = errors.New("restoreInto failed")
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(e.t, err)
			return rec.OpID
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(o *Options) { o.ReadyTimeout = 30 * time.Millisecond })
			f := newFailOnce(c.to)
			e.m.failStep = f.hook
			opID := c.start(e)
			f.wait(t)
			// 段は書けていないので、終わりを待つ代わりに操作の goroutine を止めて待つ。
			e.stop()
			got, err := e.store.Get(ctx, opID)
			require.NoError(t, err)
			assert.NotEqual(t, c.to, got.Step, "the step was not written")
			j, err := e.store.Journal(ctx, opID)
			require.NoError(t, err)
			n := 0
			for _, x := range j {
				if strings.Contains(x.Message, c.want) {
					n++
				}
			}
			assert.Equal(t, 1, n, "the ending is journaled once: %v", j)
		})
	}
}

// TestAppendLine_UnknownOutcome checks that appendLine does not add the line
// again when the write whose outcome was unknown did append it (a timeout,
// or the ErrStep a replayed script returns), and adds it when the error says
// nothing was written or the journal has no such line.
func TestAppendLine_UnknownOutcome(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.stop()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-unknown", Step: StepAborting, Database: testDB, Target: tgt, Before: before}))
	w, err := e.m.setStepWithLine(ctx, "restore-unknown", StepAborting, StepAborted, say(StepAborted, "the ending"))
	require.NoError(t, err)
	count := func() int {
		j, err := e.store.Journal(ctx, "restore-unknown")
		require.NoError(t, err)
		n := 0
		for _, x := range j {
			if x.Message == "the ending" {
				n++
			}
		}
		return n
	}
	// script は動いたが応答が届かなかった。送り直しは ErrStep を返す。
	for _, uncertain := range []error{errors.New("i/o timeout"), ErrStep} {
		e.m.appendLine(ctx, w, uncertain)
		assert.Equal(t, 1, count(), "already in the journal (%v)", uncertain)
	}
	// 送っていないと分かる誤りなら、読み直さずに足す。
	e.m.appendLine(ctx, w, ErrNotFound)
	assert.Equal(t, 2, count())
	e.m.appendLine(ctx, w, fmt.Errorf("%w: x", errStepHook))
	assert.Equal(t, 3, count())
	// 結果の分からない誤りでも、同じ行が無ければ足す (時刻が違う、段が違う)。
	other := lineWrite{opID: "restore-unknown", entry: JournalEntry{At: time.Now(), Step: StepAborted, Message: "the ending"}}
	e.m.appendLine(ctx, other, ErrStep)
	assert.Equal(t, 4, count())
	otherStep := lineWrite{opID: "restore-unknown", entry: JournalEntry{At: w.entry.At, Step: StepFailed, Message: "the ending"}}
	e.m.appendLine(ctx, otherStep, errors.New("i/o timeout"))
	assert.Equal(t, 5, count(), "the same time and message at another step is another line")
}

// TestFinalStep_ReplayedScript checks the ending line when go-redis sends
// the script of the final step again after the first one went through but
// its reply was lost: the replay returns ErrStep, and the line must stay
// single (and a preparation must not also say aborted).
func TestFinalStep_ReplayedScript(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, to, want string
		start          func(e *env) string
	}{
		{"recordWithLine", StepExpired, "nobody confirmed within", func(e *env) string {
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(e.t, err)
			return rec.OpID
		}},
		{"prepare", StepFailed, "restoreInto failed", func(e *env) string {
			e.db.fail["restoreInto"] = errors.New("restoreInto failed")
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(e.t, err)
			return rec.OpID
		}},
		{"prepare with a message cut in a character", StepFailed, "restoreInto failed", func(e *env) string {
			// pg_restore の stderr はバイト単位で切るので、多バイト文字の途中で切れうる。
			e.db.fail["restoreInto"] = errors.New("restoreInto failed: \xe3\x81")
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(e.t, err)
			return rec.OpID
		}},
		{"prepare with the journal unreadable once", StepFailed, "restoreInto failed", func(e *env) string {
			e.db.fail["restoreInto"] = errors.New("restoreInto failed")
			var failed atomic.Bool
			e.interceptCmd(func(cmd redis.Cmder) error {
				// 送り直しの後に journal を読む 1 回目だけを落とす。
				if cmd.Name() == "lrange" && e.replayed() && failed.CompareAndSwap(false, true) {
					return errors.New("i/o timeout")
				}
				return nil
			})
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(e.t, err)
			return rec.OpID
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			logs := &logBuffer{}
			e := newEnv(t, logs.into, func(o *Options) { o.ReadyTimeout = 30 * time.Millisecond })
			e.replayStepTo(c.to)
			opID := c.start(e)
			e.waitStep(opID, c.to)
			e.stop()
			require.True(t, e.replayed(), "the script was not replayed")
			j, err := e.store.Journal(ctx, opID)
			require.NoError(t, err)
			n := 0
			for _, x := range j {
				if strings.Contains(x.Message, c.want) {
					n++
				}
				assert.NotEqual(t, StepAborted, x.Step, "%v", j)
			}
			assert.Equal(t, 1, n, "the ending is journaled once: %v", j)
			// 送り直しを書けたとみなした経路でも、結末の行のログは 1 回だけ出る。
			assert.Equal(t, 1, logs.count("step="+c.to+" message="), logs.String())
		})
	}
}

// crashedFinished is crashed at resuming after the switch, with the database
// kept aside already marked as kept by this finished operation.
func crashedFinished(t *testing.T) (*env, *Record) {
	t.Helper()
	e := newEnv(t)
	e.stop()
	e.process("p1")
	now := time.Now()
	rec := &Record{
		OpID: "restore-crashed", Step: StepResuming, GenerationID: "20261010T120000Z", Database: testDB,
		Target: tgt, Before: before, UserID: "u1", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, e.store.create(context.Background(), rec))
	e.db.mu.Lock()
	e.db.dbs = map[string]string{testDB: "restored", before: "current"}
	e.db.marks = map[string]opMark{"current": {state: backup.MarkFinished, op: rec.OpID}}
	e.db.mu.Unlock()
	e.start()
	return e, rec
}

// TestAbort_ReplayedScript checks that an abort whose script was replayed
// (the first went through, the replay returns ErrStep) reports success and
// journals its ending once.
func TestAbort_ReplayedScript(t *testing.T) {
	ctx := context.Background()
	logs := &logBuffer{}
	e := newEnv(t, logs.into)
	rec, _ := e.prepare()
	e.replayStepTo(StepAborted)
	require.NoError(t, e.m.Abort(ctx, rec.OpID))
	require.True(t, e.replayed(), "the script was not replayed")
	assert.False(t, e.slot.isHeld(), "the operation is finished and gives back the slot")
	e.stop()
	assert.Equal(t, 1, logs.count(`message="aborted; dropped`), logs.String())
	j, err := e.store.Journal(ctx, rec.OpID)
	require.NoError(t, err)
	n := 0
	for _, x := range j {
		if strings.HasPrefix(x.Message, "aborted; dropped") {
			n++
		}
	}
	assert.Equal(t, 1, n, "%v", j)
}

// TestAbort_LostScript checks that an abort whose script was never run (a
// timeout before sending) reports the error and leaves the step at
// aborting: the line is not in the journal, so it was not written.
func TestAbort_LostScript(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	rec, _ := e.prepare()
	var dropped atomic.Bool
	e.interceptCmd(func(cmd redis.Cmder) error {
		if stepScriptTo(cmd, StepAborted) && dropped.CompareAndSwap(false, true) {
			return errors.New("i/o timeout")
		}
		return nil
	})
	assert.ErrorContains(t, e.m.Abort(ctx, rec.OpID), "i/o timeout")
	require.True(t, dropped.Load(), "the script was not dropped")
	got, err := e.store.Get(ctx, rec.OpID)
	require.NoError(t, err)
	assert.Equal(t, StepAborting, got.Step)
}

// TestStepLine_NothingAfterTheEnding checks that the line of a step another
// goroutine can move on from (ready, and the abort requested while
// preparing) is in the journal before that goroutine writes the ending:
// the sender of the step is held right after its script, and the ending is
// written meanwhile (#3626).
func TestStepLine_NothingAfterTheEnding(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name, pause, final, want string
		// act moves the operation on while the sender of pause is held, and
		// returns once the ending is written. release lets the sender go on.
		act func(t *testing.T, e *env, opID string, release func())
	}{
		{"aborted while ready", StepReady, StepAborted, "aborted; dropped " + testDB + "_restore_", func(t *testing.T, e *env, opID string, release func()) {
			require.NoError(t, e.m.Abort(ctx, opID))
			release()
		}},
		{"confirmed while ready", StepReady, StepDone, "restored generation 20261010T120000Z", func(t *testing.T, e *env, opID string, release func()) {
			e.process("p1")
			require.NoError(t, e.m.Confirm(ctx, opID))
			e.waitStep(opID, StepDone)
			release()
		}},
		{"aborted while preparing", StepAborting, StepAborted, "", func(t *testing.T, e *env, opID string, release func()) {
			// 中止の側を止めている間に、準備を終わらせて aborted まで進める。
			close(e.db.block)
			e.waitStep(opID, StepAborted)
			release()
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			if c.pause == StepAborting {
				e.db.block = make(chan struct{})
			}
			reached, release := e.pauseAfterStepTo(c.pause)
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(t, err)
			abortDone := make(chan error, 1)
			if c.pause == StepAborting {
				e.waitStep(rec.OpID, StepPreparing)
				go func() { abortDone <- e.m.Abort(ctx, rec.OpID) }()
			}
			select {
			case <-reached:
			case <-time.After(10 * time.Second):
				t.Fatalf("the script to %s did not run", c.pause)
			}
			c.act(t, e, rec.OpID, release)
			if c.pause == StepAborting {
				require.NoError(t, <-abortDone)
			}
			// 止めていた側が書き終えるまで待つ。
			e.stop()
			got, err := e.store.Get(ctx, rec.OpID)
			require.NoError(t, err)
			require.Equal(t, c.final, got.Step)
			j, err := e.store.Journal(ctx, rec.OpID)
			require.NoError(t, err)
			require.NotEmpty(t, j)
			last := j[len(j)-1]
			if c.want != "" {
				assert.Contains(t, last.Message, c.want, "%v", j)
			}
			assert.Equal(t, c.final, last.Step, "%v", j)
			// 段と同じ script で書く値も、前の 2 コマンドの形と同じであること。
			switch c.pause {
			case StepReady:
				assert.False(t, got.ReadyAt.IsZero(), "readyAt is written with ready")
				assert.Equal(t, StepReady, lineStep(j, "ready to switch; waiting for the confirmation"), "%v", j)
			case StepAborting:
				assert.Equal(t, "aborted by the administrator", got.Error)
				assert.Equal(t, StepPreparing, lineStep(j, "abort requested"), "%v", j)
			}
		})
	}
}

// TestStepLine_ReplayedScript checks that a replayed script for ready or for
// the abort requested while preparing, whose second run returns ErrStep, is
// taken as written: the line is in the journal (#3626).
func TestStepLine_ReplayedScript(t *testing.T) {
	ctx := context.Background()
	t.Run("ready", func(t *testing.T) {
		logs := &logBuffer{}
		e := newEnv(t, logs.into)
		e.process("p1")
		e.replayStepTo(StepReady)
		rec, _ := e.prepare()
		require.NoError(t, e.m.Confirm(ctx, rec.OpID))
		e.waitStep(rec.OpID, StepDone, StepFailed, StepAborted)
		e.stop()
		// 送り直しは準備の goroutine の中で終わるので、止めた後に確かめる。
		require.True(t, e.replayed(), "the script was not replayed")
		assert.Equal(t, 1, logs.count(`message="ready to switch; waiting for the confirmation"`), logs.String())
		got, err := e.store.Get(ctx, rec.OpID)
		require.NoError(t, err)
		assert.Equal(t, StepDone, got.Step)
		j, err := e.store.Journal(ctx, rec.OpID)
		require.NoError(t, err)
		n := 0
		for _, x := range j {
			assert.NotEqual(t, StepFailed, x.Step, "%v", j)
			if x.Message == "ready to switch; waiting for the confirmation" {
				n++
			}
		}
		assert.Equal(t, 1, n, "%v", j)
	})
	t.Run("abort while preparing", func(t *testing.T) {
		logs := &logBuffer{}
		e := newEnv(t, logs.into)
		e.db.block = make(chan struct{})
		rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
		require.NoError(t, err)
		e.waitStep(rec.OpID, StepPreparing)
		e.replayStepTo(StepAborting)
		require.NoError(t, e.m.Abort(ctx, rec.OpID))
		require.True(t, e.replayed(), "the script was not replayed")
		e.waitStep(rec.OpID, StepAborted)
		e.stop()
		assert.Equal(t, 1, logs.count(`message="abort requested"`), logs.String())
		j, err := e.store.Journal(ctx, rec.OpID)
		require.NoError(t, err)
		n := 0
		for _, x := range j {
			if x.Message == "abort requested" {
				n++
			}
		}
		assert.Equal(t, 1, n, "%v", j)
	})
}

// TestStepLine_AbortLosesToReady checks that an abort requested while
// preparing, whose script finds the step already moved to ready by the
// preparation, returns ErrStep and leaves the preparation alone: the line is
// not in the journal, so the ErrStep is not a replay (#3626). The ready
// operation can still be confirmed, and still expires when nobody confirms.
func TestStepLine_AbortLosesToReady(t *testing.T) {
	ctx := context.Background()
	for _, confirm := range []bool{true, false} {
		name := "then expires"
		if confirm {
			name = "then confirmed"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, func(o *Options) {
				if !confirm {
					o.ReadyTimeout = 300 * time.Millisecond
				}
			})
			e.db.block = make(chan struct{})
			rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(t, err)
			e.waitStep(rec.OpID, StepPreparing)
			var once sync.Once
			e.interceptCmd(func(cmd redis.Cmder) error {
				if stepScriptTo(cmd, StepAborting) {
					// 中止の script を送る前に準備を終わらせ、段が ready になるまで待つ。
					once.Do(func() {
						close(e.db.block)
						deadline := time.Now().Add(10 * time.Second)
						for time.Now().Before(deadline) {
							if e.mr.HGet("p:elythia:maintenance:restore:"+rec.OpID, "step") == StepReady {
								return
							}
							time.Sleep(time.Millisecond)
						}
					})
				}
				return nil
			})
			assert.ErrorIs(t, e.m.Abort(ctx, rec.OpID), ErrStep)
			e.interceptCmd(nil)
			got, err := e.store.Get(ctx, rec.OpID)
			require.NoError(t, err)
			require.Equal(t, StepReady, got.Step)
			if confirm {
				e.process("p1")
				require.NoError(t, e.m.Confirm(ctx, rec.OpID))
				e.waitStep(rec.OpID, StepDone)
				return
			}
			// 中止が準備を止めていなければ、期限を見る待ちが残っていて expired になる。
			e.waitStep(rec.OpID, StepExpired)
		})
	}
}

// lineStep returns the step of the journal line whose message is msg, or ""
// when there is none.
func lineStep(j []JournalEntry, msg string) string {
	for _, x := range j {
		if x.Message == msg {
			return x.Step
		}
	}
	return ""
}

// logBuffer collects the log of a manager (Options.Logger) for a test.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// count returns how many times s appears in the log.
func (l *logBuffer) count(s string) int { return strings.Count(l.String(), s) }

// into is a newEnv option that sends the manager's log to l.
func (l *logBuffer) into(o *Options) { o.Logger = slog.New(slog.NewTextHandler(l, nil)) }
