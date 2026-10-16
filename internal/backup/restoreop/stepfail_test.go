package restoreop

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/maintmode"
)

// failOnce fails writing the step to once, and reports when it did.
type failOnce struct {
	to    string
	once  sync.Once
	fired chan struct{}
}

func newFailOnce(to string) *failOnce { return &failOnce{to: to, fired: make(chan struct{})} }

func (f *failOnce) hook(to string) error {
	if to != f.to {
		return nil
	}
	var err error
	f.once.Do(func() {
		err = errors.New("redis: write failed")
		close(f.fired)
	})
	return err
}

func (f *failOnce) wait(t *testing.T) {
	t.Helper()
	select {
	case <-f.fired:
	case <-time.After(10 * time.Second):
		t.Fatalf("writing %s did not fail", f.to)
	}
}

// TestStepNotWritten checks that a step that cannot be written stops the
// operation before anything outside depends on it, and that the same run
// then recovers from the step that was written, without a restart (#3463
// (6c) M-2, and H-1 of the third review).
func TestStepNotWritten(t *testing.T) {
	cases := []struct {
		name      string
		to        string
		failCheck bool
		holdConn  bool
		// heldExpires is whether the case is about the wait for held running
		// out (HeldTimeout): only then is the wait short.
		heldExpires bool
		final       string
		// cleaned is how often Redis was cleaned in all: a step that was not
		// written must not be followed by the cleanup it guards.
		cleaned int
	}{
		{"switched", StepSwitched, false, false, false, StepReverted, 1},
		{"resuming", StepResuming, false, false, false, StepReverted, 2},
		{"reverting", StepReverting, true, false, false, StepReverted, 2},
		{"aborting", StepAborting, false, true, true, StepAborted, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, func(o *Options) {
				// 待ちの期限を切らすのは、held を待って中止へ進むケース (heldExpires) だけ。
				// ほかは判定役が held にするのを待つ (段が書けないのは held の後)。全部を
				// 100ms にしていたときは、負荷の下で判定役が間に合わず、書けないはずの段の
				// 前で中止へ進んで落ちた (#3369)。
				o.HeldTimeout = time.Hour
				if c.heldExpires {
					o.HeldTimeout = 100 * time.Millisecond
				}
				// 戻した後の再開の待ちも、判定役がメンテナンスを外すまで待たせる。期限で
				// 待ちを抜けると操作が終わって判定役も止まり、このテストには外す者が残らない
				// (本番では各プロセスの判定役が外す)。
				o.ResumeTimeout = time.Hour
			})
			e.process("p1")
			if c.failCheck {
				e.db.fail["check"] = errors.New("doctor failed")
			}
			rec, _ := e.prepare()
			if c.holdConn {
				e.db.mu.Lock()
				e.db.conns[testDB] = 1
				e.db.mu.Unlock()
			}
			f := newFailOnce(c.to)
			e.m.failStep = f.hook
			require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
			f.wait(t)
			e.db.mu.Lock()
			e.db.conns[testDB] = 0
			e.db.mu.Unlock()
			e.waitStep(rec.OpID, c.final)
			assert.Equal(t, "current", e.db.content(testDB))
			e.waitMaintenanceGone(5 * time.Second)
			require.Eventually(t, func() bool { return !e.slot.isHeld() }, 5*time.Second, 5*time.Millisecond)
			e.mu.Lock()
			defer e.mu.Unlock()
			assert.Equal(t, c.cleaned, e.cleaned)
			assert.Empty(t, e.notes)
		})
	}
}

// TestStepNotWritten_Notifies checks that an operation that cannot go on
// keeps the judge and the slot and notifies once, whether Redis cannot be
// read or can be read but not written, and finishes when Redis is back.
func TestStepNotWritten_Notifies(t *testing.T) {
	for _, c := range []struct {
		name     string
		readable bool
	}{{"unreadable", false}, {"readable but not writable", true}} {
		t.Run(c.name, func(t *testing.T) {
			var clock atomic.Int64
			start := time.Now()
			e := newEnv(t, func(o *Options) {
				o.Now = func() time.Time { return start.Add(time.Duration(clock.Load())) }
			})
			e.process("p1")
			rec, _ := e.prepare()
			var failed, broken atomic.Bool
			var writes atomic.Int32
			e.m.failStep = func(to string) error {
				if to == StepSwitched && failed.CompareAndSwap(false, true) {
					broken.Store(true)
					if !c.readable {
						// go-redis が再試行しない種類の誤り。送る前にクライアント側で返す。
						// miniredis の SetError は、プロセスの報告の MULTI と EXEC の間で
						// 切り替わると、サーバー側の接続を MULTI のまま残す。go-redis はその
						// 接続をプールへ戻すので、戻した後も +QUEUED しか返らず、報告が
						// 止まってメンテナンスが外れなかった (#3369)。
						e.interceptCmd(func(redis.Cmder) error { return errors.New("ERR the record is not available") })
					}
				}
				if broken.Load() {
					writes.Add(1)
					return errors.New("OOM command not allowed when used memory > 'maxmemory'")
				}
				return nil
			}
			require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
			require.Eventually(t, func() bool {
				// 書けなかった後だけ時計を進める (それまでは held の待ちの期限に効く)。
				if failed.Load() {
					clock.Add(int64(stalledNotifyAfter))
				}
				e.mu.Lock()
				defer e.mu.Unlock()
				return len(e.notes) >= 1
			}, 5*time.Second, 5*time.Millisecond)
			if c.readable {
				// 回復を何周も回しても、知らせるのは 1 回。
				w := writes.Load()
				require.Eventually(t, func() bool { return writes.Load() >= w+3 }, 5*time.Second, 5*time.Millisecond)
			}
			assert.True(t, e.slot.isHeld(), "the slot is kept while it waits")
			broken.Store(false)
			e.interceptCmd(nil)
			e.waitStep(rec.OpID, StepReverted)
			e.waitMaintenanceGone(5 * time.Second)
			e.mu.Lock()
			defer e.mu.Unlock()
			assert.Len(t, e.notes, 1, "notified once")
			assert.Contains(t, e.notes[0], "cannot write or read its record")
		})
	}
}

// TestStepNotWritten_RecordGone checks that an operation whose record is gone
// stops, notifies and gives the slot back instead of reading forever.
func TestStepNotWritten_RecordGone(t *testing.T) {
	// 書けなくするのは held の後の段。held の待ちが期限で切れると中止へ進み、記録を
	// 消す前に終わる (#3369)。
	e := newEnv(t, func(o *Options) { o.HeldTimeout = time.Hour })
	e.process("p1")
	rec, _ := e.prepare()
	var once sync.Once
	// 消せたかは、manager の goroutine ではなくテストの goroutine で確かめる
	// (FailNow はテストの goroutine から呼ぶ)。
	deleted := make(chan error, 1)
	e.m.failStep = func(to string) error {
		var err error
		if to == StepSwitched {
			once.Do(func() {
				deleted <- e.rdb.Del(context.Background(), "p:elythia:maintenance:restore:"+rec.OpID).Err()
				err = errors.New("redis: write failed")
			})
		}
		return err
	}
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	require.Eventually(t, func() bool { return !e.slot.isHeld() }, 5*time.Second, 5*time.Millisecond)
	select {
	case err := <-deleted:
		require.NoError(t, err)
	default:
		t.Fatal("writing switched did not fail")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Len(t, e.notes, 1)
	assert.Contains(t, e.notes[0], "its record is gone")
}

// TestStepNotWritten_AtStart checks that the recovery at start also goes on
// from the written step when it cannot write one (settle after recoverRecord).
func TestStepNotWritten_AtStart(t *testing.T) {
	// 戻した後の再開の待ちが期限で抜けると、回復が終わって判定役も止まり、メンテナンスを
	// 外す者が残らない (#3369)。
	e := newEnv(t, func(o *Options) { o.ResumeTimeout = time.Hour })
	e.stop()
	e.process("p1")
	ctx := context.Background()
	rec := &Record{OpID: "restore-0123456789abcdef", Step: StepSwitched, Database: testDB, Target: tgt, Before: before}
	require.NoError(t, e.store.create(ctx, rec))
	e.db.mu.Lock()
	e.db.dbs = map[string]string{testDB: "restored", before: "current"}
	markSwitchedOut(e, rec)
	e.db.mu.Unlock()
	require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "restore-0123456789abcdef", Kind: maintmode.KindRestore, Phase: maintmode.PhaseHeld}))
	f := newFailOnce(StepReverting)
	e.stepHook = f.hook
	e.start()
	f.wait(t)
	e.waitStep("restore-0123456789abcdef", StepReverted)
	assert.Equal(t, "current", e.db.content(testDB))
	e.waitMaintenanceGone(5 * time.Second)
}

// TestRecover_ResumingWithNamesNotSwapped checks that a resuming record whose
// database names are not as after the switch is left to an operator: the
// record may lag behind a revert that already put the previous one back.
func TestRecover_ResumingWithNamesNotSwapped(t *testing.T) {
	names := map[string]string{testDB: "current", testDB + "_rolled_back_20261010120500": "restored"}
	e, rec := crashed(t, StepResuming, names, maintmode.PhaseHeld, "")
	e.waitStep(rec.OpID, StepUnknown)
	assert.Equal(t, names, e.db.names())
	st := e.maintenance()
	require.NotNil(t, st)
	assert.Equal(t, maintmode.PhaseHeld, st.Phase, "not resumed")
}
