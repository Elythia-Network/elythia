package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
)

// rig is a daemon wired to fakes. It must be created inside a synctest
// bubble, whose clock starts at 2000-01-01T00:00:00Z.
type rig struct {
	st     *memStorage
	taker  *fakeTaker
	ver    *fakeVerifier
	notify *recNotifier
	d      *Daemon
	cancel context.CancelFunc
	done   chan struct{}
}

func newRig(t *testing.T, sched *Schedule, prepare func(*rig)) *rig {
	return newRigOn(t, newMemStorage(), sched, prepare)
}

// newRigOn is newRig on an existing storage, for a restarted daemon.
func newRigOn(_ *testing.T, st *memStorage, sched *Schedule, prepare func(*rig)) *rig {
	r := &rig{st: st, taker: &fakeTaker{st: st, now: func() time.Time { return time.Now().UTC() }}, ver: &fakeVerifier{st: st}, notify: &recNotifier{}}
	if prepare != nil {
		prepare(r)
	}
	r.d = New(Options{Schedule: sched, Storage: st, Taker: r.taker, Verifier: r.ver, Notifier: r.notify,
		Instance: "https://example.tld", Logger: discardLogger(), Clock: utcClock{}})
	return r
}

// utcClock is the (bubble's) real clock in UTC, so that times compare equal
// with assert.Equal.
type utcClock struct{}

func (utcClock) Now() time.Time                       { return time.Now().UTC() }
func (utcClock) NewTimer(d time.Duration) *time.Timer { return time.NewTimer(d) }

func (r *rig) start(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		r.d.Run(ctx)
		close(r.done)
	}()
	synctest.Wait()
}

func (r *rig) stop() {
	r.cancel()
	<-r.done
}

var epoch = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func at(day, hour int) time.Time {
	return epoch.Add(time.Duration(day)*24*time.Hour + time.Duration(hour)*time.Hour)
}

func TestDaemonTakesVerifiesAndPrunesOnSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		require.True(t, time.Now().Equal(epoch))
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00", Keep: 2, Verify: true}, time.UTC)
		r := newRig(t, s, nil)
		r.start(t)

		// 世代が無いので起動してすぐ取り、その後は毎日 04:00。
		assert.Equal(t, []time.Time{at(0, 0)}, r.taker.callTimes())
		st := r.d.Status()
		require.NotNil(t, st.NextRunAt)
		assert.Equal(t, at(0, 4), *st.NextRunAt)

		time.Sleep(3*24*time.Hour + 5*time.Hour)
		synctest.Wait()
		assert.Equal(t, []time.Time{at(0, 0), at(0, 4), at(1, 4), at(2, 4), at(3, 4)}, r.taker.callTimes())

		want := []string{backup.NewID(at(0, 0)), backup.NewID(at(0, 4)), backup.NewID(at(1, 4)), backup.NewID(at(2, 4)), backup.NewID(at(3, 4))}
		assert.Equal(t, want, r.ver.verified(), "every new generation is verified")
		assert.Equal(t, want[3:], r.st.ids(), "keep: 2 leaves the two newest verified generations")
		assert.Empty(t, r.notify.kinds())

		st = r.d.Status()
		require.NotNil(t, st.LastTake)
		assert.True(t, st.LastTake.OK)
		assert.Equal(t, want[4], st.LastTake.GenerationID)
		assert.Equal(t, TriggerSchedule, st.LastTake.Trigger)
		assert.Equal(t, []string{want[2]}, st.LastTake.Deleted)
		require.NotNil(t, st.LastTake.Verify)
		assert.True(t, st.LastTake.Verify.OK)
		require.NotNil(t, st.LatestUsable)
		assert.Equal(t, want[4], st.LatestUsable.ID)
		assert.Equal(t, at(4, 4), *st.NextRunAt)
		assert.False(t, st.Overdue)
		assert.Nil(t, st.Running)
		assert.Equal(t, &ScheduleInfo{Interval: "24h0m0s", At: "04:00", Timezone: "UTC", Keep: 2, Verify: true, DelayAfter: "36h0m0s"}, st.Schedule)
		r.stop()
	})
}

func TestDaemonWaitsForSlotAfterRecentGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00", Keep: 3, Verify: true}, time.UTC)
		r := newRig(t, s, func(r *rig) { r.st.addGeneration(epoch.Add(-time.Hour), true, "ok") })
		r.start(t)
		assert.Empty(t, r.taker.callTimes(), "a restart does not take a backup right away")
		time.Sleep(4*time.Hour + time.Minute)
		synctest.Wait()
		assert.Equal(t, []time.Time{at(0, 4)}, r.taker.callTimes())
		r.stop()
	})
}

func TestDaemonKeepsOldGenerationsUntilNewOnesVerify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00", Keep: 2, Verify: true, DelayAfter: "240h"}, time.UTC)
		var a, b string
		r := newRig(t, s, func(r *rig) {
			a = r.st.addGeneration(at(-2, 4), true, "ok")
			b = r.st.addGeneration(at(-1, 4), true, "ok")
			r.ver.fail = true
		})
		r.start(t)
		// 検証に落ち続ける間は、古い世代を消さない。
		time.Sleep(2*24*time.Hour + 5*time.Hour)
		synctest.Wait()
		require.Len(t, r.taker.callTimes(), 3)
		failedC := backup.NewID(at(0, 4))
		assert.Equal(t, []string{a, b, failedC, backup.NewID(at(1, 4)), backup.NewID(at(2, 4))}, r.st.ids(),
			"no generation is deleted while the new ones fail verification")
		for _, k := range r.notify.kinds() {
			assert.Equal(t, EventMismatch, k)
		}
		ev := r.notify.all()
		require.Len(t, ev, 3)
		assert.Equal(t, failedC, ev[0].GenerationID)
		assert.Equal(t, "https://example.tld", ev[0].Instance)
		assert.Equal(t, []backup.RowMismatch{{Table: "public.note", Expected: 10, Actual: 9}}, ev[0].Mismatches)
		assert.Equal(t, []backup.StageResult{{Stage: backup.StageRestorable, Error: stageSummary(backup.StageRestorable)}}, ev[0].FailedStages, "skipped stages are not reported as failures")
		st := r.d.Status()
		assert.False(t, st.LastTake.OK)
		assert.Equal(t, "verify", st.LastTake.Stage)
		assert.Equal(t, b, st.LatestUsable.ID)

		// 検証に通る世代が 1 つできると、窓 (新しい 2 つ) より古い a だけが消える。
		r.ver.mu.Lock()
		r.ver.fail = false
		r.ver.mu.Unlock()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		d := backup.NewID(at(3, 4))
		assert.NotContains(t, r.st.ids(), a)
		assert.Contains(t, r.st.ids(), b)
		assert.Contains(t, r.st.ids(), failedC, "broken generations newer than the window stay")

		// もう 1 つで、b と、それより古い壊れた世代が消える。
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		assert.Equal(t, []string{d, backup.NewID(at(4, 4))}, r.st.ids())
		r.stop()
	})
}

func TestDaemonReportsFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1, Verify: true, DelayAfter: "240h"}, time.UTC)
		r := newRig(t, s, func(r *rig) {
			r.st.addGeneration(at(-1, 0), true, "ok")
			r.taker.err = errBoom
		})
		r.start(t)
		ev := r.notify.all()
		require.Len(t, ev, 1)
		assert.Equal(t, EventFailure, ev[0].Kind)
		assert.Equal(t, "take", ev[0].Stage)
		assert.Equal(t, "boom", ev[0].Message)
		assert.Equal(t, epoch, ev[0].OccurredAt)
		st := r.d.Status()
		assert.False(t, st.LastTake.OK)
		assert.Equal(t, "take", st.LastTake.Stage)
		assert.Equal(t, "boom", st.LastTake.Error)
		assert.Empty(t, r.ver.verified(), "nothing to verify after a failed take")
		assert.Len(t, r.st.ids(), 1, "nothing is pruned after a failed take")

		// 検証が最後まで走れなかったときも失敗として知らせ、消さない。
		r.taker.mu.Lock()
		r.taker.err = nil
		r.taker.mu.Unlock()
		r.ver.mu.Lock()
		r.ver.err = errBoom
		r.ver.mu.Unlock()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		ev = r.notify.all()
		require.Len(t, ev, 2)
		assert.Equal(t, EventFailure, ev[1].Kind)
		assert.Equal(t, "verify", ev[1].Stage)
		assert.Equal(t, backup.NewID(at(1, 0)), ev[1].GenerationID)
		assert.Len(t, r.st.ids(), 2)

		// 消す段の失敗。
		r.ver.mu.Lock()
		r.ver.err = nil
		r.ver.mu.Unlock()
		r.st.mu.Lock()
		r.st.deleteErr = errBoom
		r.st.mu.Unlock()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		ev = r.notify.all()
		require.Len(t, ev, 3)
		assert.Equal(t, "prune", ev[2].Stage)
		assert.Equal(t, "prune", r.d.Status().LastTake.Stage)
		r.stop()
	})
}

// 判定の後 (使い捨てのサーバーの後始末など) で失敗しても、判定は判定として知らせ、
// verify.json を残し、整理はその verify.json で行う (#3459 のレビューの M1)。
func TestDaemonKeepsVerdictWhenCleanupFails(t *testing.T) {
	cleanupErr := errors.New("pg_ctl stop: server did not shut down")
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1, Verify: true, DelayAfter: "240h"}, time.UTC)
		var old string
		r := newRig(t, s, func(r *rig) {
			old = r.st.addGeneration(at(-1, 0), true, "ok")
			r.ver.fail = true
			r.ver.afterErr = cleanupErr
		})
		r.start(t)
		broken := backup.NewID(at(0, 0))
		ev := r.notify.all()
		require.Len(t, ev, 2)
		assert.Equal(t, EventMismatch, ev[0].Kind, "a broken generation is reported as a mismatch, not only as a cleanup failure")
		assert.Equal(t, broken, ev[0].GenerationID)
		assert.Equal(t, []backup.RowMismatch{{Table: "public.note", Expected: 10, Actual: 9}}, ev[0].Mismatches)
		assert.Equal(t, []backup.StageResult{{Stage: backup.StageRestorable, Error: stageSummary(backup.StageRestorable)}}, ev[0].FailedStages)
		assert.Equal(t, EventFailure, ev[1].Kind)
		assert.Equal(t, "verify", ev[1].Stage)
		assert.Contains(t, ev[1].Message, "server did not shut down")
		v, err := backup.ReadVerify(context.Background(), r.st, broken)
		require.NoError(t, err, "the verdict is stored as verify.json")
		assert.False(t, v.OK)
		st := r.d.Status()
		assert.False(t, st.LastTake.OK)
		assert.Equal(t, "verify", st.LastTake.Stage)
		assert.Contains(t, st.LastTake.Error, "verification failed")
		assert.Contains(t, st.LastTake.Error, "server did not shut down")
		assert.Equal(t, []string{old, broken}, r.st.ids(), "a failed verdict prunes nothing")

		// 通った判定なら、verify.json を残して整理まで進めるが、作業は OK にしない。
		r.ver.mu.Lock()
		r.ver.fail = false
		r.ver.mu.Unlock()
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		good := backup.NewID(at(1, 0))
		ev = r.notify.all()
		require.Len(t, ev, 3)
		assert.Equal(t, EventFailure, ev[2].Kind)
		assert.Equal(t, good, ev[2].GenerationID)
		v, err = backup.ReadVerify(context.Background(), r.st, good)
		require.NoError(t, err)
		assert.True(t, v.OK)
		assert.Equal(t, []string{good}, r.st.ids(), "the stored verdict lets pruning count the new generation")
		st = r.d.Status()
		assert.False(t, st.LastTake.OK)
		assert.Equal(t, "verify", st.LastTake.Stage)
		assert.Equal(t, []string{old, broken}, st.LastTake.Deleted)
		assert.Equal(t, good, st.LatestUsable.ID)
		r.stop()
	})
}

func TestDaemonReportsVerdictStoreFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1, Verify: true, DelayAfter: "240h"}, time.UTC)
		r := newRig(t, s, func(r *rig) {
			r.st.addGeneration(at(-1, 0), true, "ok")
			r.ver.afterErr = errBoom
		})
		r.st.putErr = errors.New("bucket is read-only")
		r.start(t)
		ev := r.notify.all()
		require.Len(t, ev, 1)
		assert.Equal(t, EventFailure, ev[0].Kind)
		assert.Contains(t, ev[0].Message, "boom")
		assert.Contains(t, ev[0].Message, "bucket is read-only")
		assert.Len(t, r.st.ids(), 2, "an unstored verdict does not count, so nothing is pruned")
		r.stop()
	})
}

func TestJudged(t *testing.T) {
	three := []backup.StageResult{{Stage: backup.StageReadable}, {Stage: backup.StageRestorable}, {Stage: backup.StageUsable}}
	assert.True(t, judged(backup.VerifyResult{Stages: three}))
	assert.False(t, judged(backup.VerifyResult{Stages: three[:2]}))
	assert.False(t, judged(backup.VerifyResult{}))
	assert.False(t, judged(backup.VerifyResult{Stages: []backup.StageResult{three[0], three[2], three[1]}}))
}

// 検証に落ちた回は整理しない。keep: 1 で使える世代が 2 つあると、整理すれば古い方が
// 消えるので、消えていないことで整理が走らなかったと分かる。
func TestDaemonDoesNotPruneAfterFailedVerification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1, Verify: true, DelayAfter: "240h"}, time.UTC)
		var a, b string
		r := newRig(t, s, func(r *rig) {
			a = r.st.addGeneration(at(-3, 0), true, "ok")
			b = r.st.addGeneration(at(-2, 0), true, "ok")
			r.ver.fail = true
		})
		r.start(t)
		require.Len(t, r.taker.callTimes(), 1)
		assert.Equal(t, []string{a, b, backup.NewID(epoch)}, r.st.ids())
		assert.Empty(t, r.d.Status().LastTake.Deleted)
		r.stop()
	})
}

// 外へ送る通知に、段の誤り (pg_restore の stderr) を載せない。
func TestDaemonMismatchNotificationOmitsStageErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1, Verify: true, DelayAfter: "240h"}, time.UTC)
		r := newRig(t, s, func(r *rig) { r.ver.fail = true })
		r.start(t)
		ev := r.notify.all()
		require.Len(t, ev, 1)
		require.Equal(t, EventMismatch, ev[0].Kind)
		body, err := json.Marshal(ev[0])
		require.NoError(t, err)
		assert.NotContains(t, string(body), "SECRET-KEY-MATERIAL")
		assert.NotContains(t, ev[0].Text(), "SECRET-KEY-MATERIAL")
		// verify.json には詳細が残る。
		v, err := backup.ReadVerify(context.Background(), r.st, backup.NewID(epoch))
		require.NoError(t, err)
		assert.Equal(t, restorableError, v.Stages[1].Error)
		r.stop()
	})
}

// 止める途中で切れた検証は判定として残さず、前の verify.json に戻す。
func TestDaemonInterruptedVerificationKeepsPreviousResult(t *testing.T) {
	for _, prev := range []string{"ok", ""} {
		t.Run("previous="+prev, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var id string
				r := newRig(t, nil, func(r *rig) {
					id = r.st.addGeneration(at(-1, 0), true, prev)
					r.ver.block = make(chan struct{})
					r.ver.cancelWrites = true
				})
				before, err := readObject(context.Background(), r.st, backup.Key(id, backup.VerifyFile))
				require.NoError(t, err)
				r.start(t)
				_, err = r.d.Start(JobVerify, id)
				require.NoError(t, err)
				synctest.Wait()
				r.stop()
				after, err := readObject(context.Background(), r.st, backup.Key(id, backup.VerifyFile))
				require.NoError(t, err)
				assert.Equal(t, before, after, "verify.json is as it was before the interrupted verification")
				assert.Empty(t, r.notify.all(), "an interruption is neither a mismatch nor a failure")
				st := r.d.Status()
				require.NotNil(t, st.LastVerify)
				assert.False(t, st.LastVerify.OK)
				assert.Contains(t, st.LastVerify.Error, "interrupted")
				assert.Nil(t, st.LastVerify.Verify)
			})
		})
	}
}

// 前の verify.json が読めなければ、戻さずにそのままにする。
func TestDaemonInterruptedVerificationWithUnreadablePrevious(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var id string
		r := newRig(t, nil, func(r *rig) {
			id = r.st.addGeneration(at(-1, 0), true, "ok")
			r.ver.block = make(chan struct{})
			r.ver.cancelWrites = true
		})
		r.start(t)
		r.st.mu.Lock()
		r.st.getErr = errBoom
		r.st.mu.Unlock()
		_, err := r.d.Start(JobVerify, id)
		require.NoError(t, err)
		synctest.Wait()
		r.stop()
		r.st.mu.Lock()
		defer r.st.mu.Unlock()
		assert.Empty(t, r.st.puts, "nothing is written back")
		assert.Empty(t, r.st.deleted, "nothing is deleted")
	})
}

// 最新の使える世代が手で確かめ直されて使えなくなったら、遅れの起点を 1 つ前の
// 使える世代へ戻し、それがもう遅れならすぐに知らせる。
func TestDaemonDelayBaseFollowsInvalidatedGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", DelayAfter: "48h"}, time.UTC)
		var old, newest string
		r := newRig(t, s, func(r *rig) {
			old = r.st.addGeneration(at(-3, 0), true, "ok")
			newest = r.st.addGeneration(at(0, 0).Add(-time.Hour), true, "ok")
		})
		r.start(t)
		require.Empty(t, r.notify.all())
		require.Equal(t, newest, r.d.Status().LatestUsable.ID)
		r.ver.mu.Lock()
		r.ver.fail = true
		r.ver.mu.Unlock()
		_, err := r.d.Start(JobVerify, newest)
		require.NoError(t, err)
		synctest.Wait()
		st := r.d.Status()
		assert.Equal(t, old, st.LatestUsable.ID)
		assert.True(t, st.Overdue)
		time.Sleep(time.Second)
		synctest.Wait()
		var delays []Event
		for _, e := range r.notify.all() {
			if e.Kind == EventDelay {
				delays = append(delays, e)
			}
		}
		require.Len(t, delays, 1, "the older usable generation is already overdue")
		assert.Equal(t, old, delays[0].GenerationID)
		r.stop()
	})
}

// 遅れの通知が送り終わらなくても、次の枠は止まらない。
func TestDaemonDelayNotificationDoesNotBlockSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", DelayAfter: "24h"}, time.UTC)
		r := newRig(t, s, func(r *rig) {
			r.st.addGeneration(at(-2, 0), true, "")
			r.taker.err = errBoom
			r.notify.block = make(chan struct{})
			r.notify.blockKind = EventDelay
		})
		r.start(t)
		require.Contains(t, r.notify.kinds(), EventDelay)
		time.Sleep(24*time.Hour + time.Second)
		synctest.Wait()
		assert.Len(t, r.taker.callTimes(), 2, "the next slot runs while the delay notification hangs")
		close(r.notify.block)
		r.stop()
	})
}

// 使える世代があって取るのに失敗し続けるときも、遅れは delayAfter ごとにだけ知らせ、
// 失敗のたびに数え直さない。
func TestDaemonDoesNotRepeatDelayOnEachFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", DelayAfter: "36h"}, time.UTC)
		r := newRig(t, s, func(r *rig) {
			r.st.addGeneration(at(-2, 0), true, "")
			r.taker.err = errBoom
		})
		r.start(t)
		time.Sleep(3*24*time.Hour - time.Second)
		synctest.Wait()
		var delays []time.Time
		for _, e := range r.notify.all() {
			if e.Kind == EventDelay {
				delays = append(delays, e.OccurredAt)
			}
		}
		assert.Equal(t, []time.Time{at(0, 0), at(1, 0), at(2, 12)}, delays)
		r.stop()
	})
}

func TestDaemonReportsDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", At: "04:00", Keep: 1, Verify: true}, time.UTC)
		r := newRig(t, s, func(r *rig) { r.taker.err = errBoom })
		r.start(t)
		// 毎回の失敗とは別に、使える世代が 36h (1.5 間隔) できないと遅れを知らせ、
		// その後も 36h ごとに繰り返す。失敗のたびに数え直さない。
		time.Sleep(4 * 24 * time.Hour)
		synctest.Wait()
		var delays []Event
		for _, e := range r.notify.all() {
			if e.Kind == EventDelay {
				delays = append(delays, e)
			}
		}
		require.Len(t, delays, 2)
		assert.Equal(t, at(1, 12), delays[0].OccurredAt)
		assert.Equal(t, at(3, 0), delays[1].OccurredAt)
		assert.Nil(t, delays[0].LastUsableAt)
		assert.Contains(t, delays[0].Message, "since the daemon started at 2000-01-01T00:00:00Z")
		assert.True(t, r.d.Status().Overdue)
		r.stop()
	})
}

func TestDaemonReportsDelayWhileTakeHangs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", DelayAfter: "30h", Keep: 1}, time.UTC)
		var last string
		r := newRig(t, s, func(r *rig) {
			last = r.st.addGeneration(at(-1, 0), true, "")
			r.taker.block = make(chan struct{})
		})
		r.start(t)
		require.Len(t, r.taker.callTimes(), 1, "the newest generation is one interval old")
		st := r.d.Status()
		require.NotNil(t, st.Running)
		assert.Equal(t, JobTake, st.Running.Kind)
		assert.False(t, st.Overdue)

		time.Sleep(6*time.Hour + time.Second)
		synctest.Wait()
		ev := r.notify.all()
		require.Len(t, ev, 1, "the delay is reported although the take never returns")
		assert.Equal(t, EventDelay, ev[0].Kind)
		assert.Equal(t, last, ev[0].GenerationID)
		require.NotNil(t, ev[0].LastUsableAt)
		assert.Equal(t, at(-1, 0), *ev[0].LastUsableAt)
		assert.Contains(t, ev[0].Message, "the newest usable backup is from 1999-12-31T00:00:00Z")

		// 次の枠が来ても、走っている間は重ねて取らない (終わったらすぐ取る)。
		time.Sleep(18 * time.Hour)
		synctest.Wait()
		assert.Len(t, r.taker.callTimes(), 1)
		assert.True(t, r.d.Status().Pending)
		close(r.taker.block)
		synctest.Wait()
		assert.Len(t, r.taker.callTimes(), 2)
		assert.False(t, r.d.Status().Pending)
		r.stop()
	})
}

func TestDaemonDefersScheduledTakeBehindManualVerify(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "1h", Keep: 5}, time.UTC)
		var id string
		r := newRig(t, s, func(r *rig) {
			id = r.st.addGeneration(epoch.Add(-30*time.Minute), true, "")
			r.ver.block = make(chan struct{})
		})
		r.start(t)
		job, err := r.d.Start(JobVerify, id)
		require.NoError(t, err)
		assert.Equal(t, Job{Kind: JobVerify, Trigger: TriggerAPI, GenerationID: id, StartedAt: epoch}, job)
		_, err = r.d.Start(JobTake, "")
		require.ErrorIs(t, err, ErrBusy)

		time.Sleep(31 * time.Minute)
		synctest.Wait()
		assert.Empty(t, r.taker.callTimes(), "the scheduled take waits for the manual verify")
		close(r.ver.block)
		synctest.Wait()
		assert.Equal(t, []time.Time{epoch.Add(31 * time.Minute)}, r.taker.callTimes())
		st := r.d.Status()
		require.NotNil(t, st.LastVerify)
		assert.True(t, st.LastVerify.OK)
		assert.Equal(t, id, st.LastVerify.GenerationID)
		assert.Equal(t, []string{id}, r.ver.verified(), "schedule.verify is off, so the new take is not verified")
		r.stop()
	})
}

// 止める途中で切れた検証は verify.json が戻されるので、再起動したら最初にその世代を
// 確かめる (取り直さない)。
func TestDaemonVerifiesInterruptedGenerationAfterRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1, Verify: true}, time.UTC)
		r := newRig(t, s, func(r *rig) {
			r.ver.block = make(chan struct{})
			r.ver.cancelWrites = true
		})
		r.start(t)
		require.Len(t, r.taker.callTimes(), 1)
		id := backup.NewID(epoch)
		r.stop()
		_, err := readObject(context.Background(), r.st, backup.Key(id, backup.VerifyFile))
		require.NoError(t, err)
		require.Equal(t, []string{id}, r.st.ids())

		time.Sleep(time.Hour)
		r2 := newRigOn(t, r.st, s, nil)
		r2.start(t)
		assert.Equal(t, []string{id}, r2.ver.verified(), "the interrupted generation is verified first")
		assert.Empty(t, r2.taker.callTimes(), "it is not taken again")
		st := r2.d.Status()
		require.NotNil(t, st.LatestUsable)
		assert.Equal(t, id, st.LatestUsable.ID)
		require.NotNil(t, st.LastVerify)
		assert.True(t, st.LastVerify.OK)
		assert.Equal(t, TriggerSchedule, st.LastVerify.Trigger)
		assert.Equal(t, at(1, 0), *st.NextRunAt)

		// 枠まで遅れを知らせない。
		time.Sleep(23*time.Hour - time.Second)
		synctest.Wait()
		assert.Empty(t, r2.notify.all())
		r2.stop()
	})
}

// 検証が判定を書き終えた直後に止めても、verify.json は戻されるが、再起動で確かめ直す。
func TestDaemonVerifiesAfterStopRightAfterVerdict(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1, Verify: true}, time.UTC)
		var r *rig
		r = newRig(t, s, func(r0 *rig) { r0.ver.afterStore = func() { r.cancel() } })
		r.start(t)
		<-r.done
		id := backup.NewID(epoch)
		b, err := readObject(context.Background(), r.st, backup.Key(id, backup.VerifyFile))
		require.NoError(t, err)
		require.Nil(t, b, "the verdict written while stopping is rolled back")

		r2 := newRigOn(t, r.st, s, nil)
		r2.start(t)
		assert.Equal(t, []string{id}, r2.ver.verified())
		assert.Empty(t, r2.taker.callTimes())
		require.NotNil(t, r2.d.Status().LatestUsable)
		assert.Equal(t, id, r2.d.Status().LatestUsable.ID)
		r2.stop()
	})
}

// 揃った最新の世代が使える、または確かめた結果が残っているなら、起動で確かめない。
// 取り戻しですぐ取るときも、古い世代は確かめない。
func TestDaemonDoesNotVerifyAtStartWhenNotNeeded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		verify bool
		age    time.Duration
		state  string
		takes  int
	}{
		{"verified", true, time.Hour, "ok", 0},
		{"failed", true, time.Hour, "failed", 0},
		{"verify off", false, time.Hour, "", 0},
		{"catching up", true, 25 * time.Hour, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Verify: tc.verify}, time.UTC)
				r := newRig(t, s, func(r *rig) { r.st.addGeneration(epoch.Add(-tc.age), true, tc.state) })
				r.start(t)
				assert.Len(t, r.taker.callTimes(), tc.takes)
				var want []string
				if tc.takes > 0 {
					want = []string{backup.NewID(epoch)}
				}
				assert.Equal(t, want, r.ver.verified())
				r.stop()
			})
		})
	}
}

// 使える世代が手で確かめ直されて 1 つも無くなったとき、遅れの起点を起動時刻と書かない。
func TestDaemonDelayMessageAfterLastUsableInvalidated(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", DelayAfter: "48h", Verify: true}, time.UTC)
		var id string
		r := newRig(t, s, func(r *rig) {
			id = r.st.addGeneration(epoch.Add(-time.Hour), true, "ok")
			r.taker.err = errBoom
		})
		r.start(t)
		r.ver.mu.Lock()
		r.ver.fail = true
		r.ver.mu.Unlock()
		_, err := r.d.Start(JobVerify, id)
		require.NoError(t, err)
		synctest.Wait()
		require.Nil(t, r.d.Status().LatestUsable)
		time.Sleep(47 * time.Hour)
		synctest.Wait()
		var delays []Event
		for _, e := range r.notify.all() {
			if e.Kind == EventDelay {
				delays = append(delays, e)
			}
		}
		require.Len(t, delays, 1)
		assert.Equal(t, epoch.Add(47*time.Hour), delays[0].OccurredAt)
		assert.NotContains(t, delays[0].Message, "daemon started")
		assert.Contains(t, delays[0].Message, "1999-12-31T23:00:00Z")
		assert.Nil(t, delays[0].LastUsableAt)
		r.stop()
	})
}

// 24 時間を割り切らない間隔に at を付けると、起動時に警告する (止めない)。
func TestDaemonWarnsAboutDriftingAt(t *testing.T) {
	for _, tc := range []struct {
		interval, at string
		warn         bool
	}{
		{"7h", "04:00", true},
		{"36h", "04:00", true},
		{"6h", "04:00", false},
		{"48h", "04:00", false},
		{"7h", "", false},
	} {
		t.Run(tc.interval+"@"+tc.at, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := mustSchedule(t, config.BackupScheduleOptions{Interval: tc.interval, At: tc.at}, time.UTC)
				r := newRig(t, s, nil)
				var buf bytes.Buffer
				r.d.log = slog.New(slog.NewTextHandler(&buf, nil))
				r.start(t)
				r.stop()
				assert.Equal(t, tc.warn, strings.Contains(buf.String(), "level=WARN"), buf.String())
				assert.Len(t, r.taker.callTimes(), 1, "the daemon still runs")
			})
		})
	}
}

func TestDaemonWithoutSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil, func(r *rig) { r.st.addGeneration(at(-1, 0), true, "ok") })
		_, err := r.d.Start(JobTake, "")
		require.ErrorIs(t, err, ErrNotRunning)
		r.start(t)
		assert.Empty(t, r.taker.callTimes())
		time.Sleep(72 * time.Hour)
		synctest.Wait()
		assert.Empty(t, r.taker.callTimes(), "no schedule, no periodic take")

		_, err = r.d.Start(JobTake, "")
		require.NoError(t, err)
		synctest.Wait()
		st := r.d.Status()
		assert.Nil(t, st.Schedule)
		assert.Nil(t, st.NextRunAt)
		assert.False(t, st.Overdue)
		assert.True(t, st.LastTake.OK)
		assert.Equal(t, TriggerAPI, st.LastTake.Trigger)
		assert.Len(t, r.st.ids(), 2, "without keep nothing is pruned")
		assert.Empty(t, r.ver.verified())
		r.stop()

		_, err = r.d.Start(JobTake, "")
		require.ErrorIs(t, err, ErrNotRunning, "a stopped daemon refuses jobs")
	})
}

func TestDaemonStopCancelsRunningJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil, func(r *rig) { r.taker.block = make(chan struct{}) })
		r.start(t)
		_, err := r.d.Start(JobTake, "")
		require.NoError(t, err)
		synctest.Wait()
		r.stop()
		assert.Equal(t, "take", r.d.Status().LastTake.Stage, "the job saw its context canceled")
	})
}

func TestDaemonSurvivesStorageErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := mustSchedule(t, config.BackupScheduleOptions{Interval: "24h", Keep: 1}, time.UTC)
		r := newRig(t, s, func(r *rig) { r.st.listErr = errBoom })
		r.start(t)
		// 一覧が取れなくても起動し、取る段まで進む。消す段で一覧の失敗を知らせる。
		require.Len(t, r.taker.callTimes(), 1)
		ev := r.notify.all()
		require.Len(t, ev, 1)
		assert.Equal(t, "prune", ev[0].Stage)
		assert.Nil(t, r.d.Status().LatestUsable)
		r.stop()
	})
}

func TestDaemonRecordsNotifyErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil, func(r *rig) {
			r.taker.err = errBoom
			r.notify.err = errBoom
		})
		r.start(t)
		_, err := r.d.Start(JobTake, "")
		require.NoError(t, err)
		synctest.Wait()
		assert.Equal(t, "boom", r.d.Status().LastNotifyError)

		r.notify.mu.Lock()
		r.notify.err = nil
		r.notify.mu.Unlock()
		_, err = r.d.Start(JobTake, "")
		require.NoError(t, err)
		synctest.Wait()
		assert.Empty(t, r.d.Status().LastNotifyError)
		r.stop()
	})
}

func TestNewDefaults(t *testing.T) {
	d := New(Options{})
	assert.NotNil(t, d.log)
	assert.IsType(t, realClock{}, d.clock)
	assert.IsType(t, nopNotifier{}, d.notify)
	assert.NoError(t, d.notify.Notify(context.Background(), Event{}))
	assert.WithinDuration(t, time.Now(), d.clock.Now(), time.Minute)
	tm := d.clock.NewTimer(time.Hour)
	assert.True(t, tm.Stop())
}
