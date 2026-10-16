package restoreop

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/maintmode"
)

func TestRestore_HappyPath(t *testing.T) {
	e := newEnv(t)
	p := e.process("p1")
	rec, ticket := e.prepare()
	assert.True(t, e.slot.isHeld(), "the job slot is held while the operation runs")
	assert.Equal(t, "restored:20261010T120000Z", e.db.content(rec.Target))
	assert.Equal(t, "current", e.db.content(testDB), "preparing does not touch the current database")

	got, err := e.m.CheckTicket(context.Background(), rec.OpID, ticket, "session-token-01")
	require.NoError(t, err)
	require.NoError(t, e.m.Confirm(context.Background(), got.OpID))
	e.waitStep(rec.OpID, StepDone)

	names := e.db.names()
	assert.Equal(t, "restored:20261010T120000Z", names[testDB])
	assert.Equal(t, "current", names[rec.Before], "the previous database is kept")
	assert.NotContains(t, names, rec.Target)
	assert.Nil(t, e.maintenance(), "the maintenance is over")
	assert.Equal(t, 1, e.cleaned)
	assert.Equal(t, 1, e.checked)
	assert.Equal(t, []string{"app:current:prepare", "app:restored:20261010T120000Z:switch"}, e.modlogs,
		"the prepare is logged in the current database, the switch in the restored one")
	require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
	p.mu.Lock()
	assert.Equal(t, "restored:20261010T120000Z", p.serving)
	p.mu.Unlock()
	cur, err := e.store.Current(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cur)
	ttl := e.mr.TTL("p:elythia:maintenance:restore:" + rec.OpID)
	assert.Greater(t, ttl, 6*24*time.Hour, "a finished record expires")

	st, err := e.m.Status(context.Background(), got)
	require.NoError(t, err)
	assert.NotEmpty(t, st.Journal)
}

// TestRestore_DoesNotSwitchBeforeHeld (R2): while a session stays on the
// database, the maintenance is not held and the names are not swapped.
func TestRestore_DoesNotSwitchBeforeHeld(t *testing.T) {
	// held の待ちが期限で切れることを検査する。接続が残り続けるので、期限が負荷で
	// 遅れても結末は変わらない。
	e := newEnv(t, func(o *Options) { o.HeldTimeout = 50 * time.Millisecond })
	e.process("p1")
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	got := e.waitStep(rec.OpID, StepAborted)
	assert.Contains(t, got.Error, "did not let go")
	assert.Equal(t, 0, e.db.switches)
	assert.Equal(t, "current", e.db.content(testDB))
	assert.NotContains(t, e.db.names(), rec.Target, "the new database is dropped")
	require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
}

// TestRestore_SwitchRefusedKeepsCurrent (R1 at this level): when the name
// swap is refused (a session came back after held), the current database
// stays and the processes resume on it.
func TestRestore_SwitchRefusedKeepsCurrent(t *testing.T) {
	e := newEnv(t)
	e.process("p1")
	e.db.switchErr = func(int) error {
		return errors.New("database \"app\" is being accessed by other users")
	}
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepAborted)
	assert.Equal(t, "current", e.db.content(testDB))
	assert.NotContains(t, e.db.names(), rec.Target)
	assert.NotContains(t, e.db.drops, testDB, "the current database is never dropped")
	require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
}

// TestRestore_PrepareFailureDropsOnlyTheNewDatabase (R3).
func TestRestore_PrepareFailureDropsOnlyTheNewDatabase(t *testing.T) {
	for _, op := range []string{"restoreInto", "verifyRestored", "settings", "migrate"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.db.fail[op] = errors.New(op + " failed")
			rec, _, err := e.m.Prepare(context.Background(), PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
			require.NoError(t, err)
			got := e.waitStep(rec.OpID, StepFailed)
			assert.Contains(t, got.Error, op+" failed")
			assert.Equal(t, []string{rec.Target}, e.db.drops)
			assert.Equal(t, "current", e.db.content(testDB))
			require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
			assert.Nil(t, e.maintenance())
		})
	}
}

// TestRestore_FailureAfterSwitchPutsThePreviousBack (R4).
func TestRestore_FailureAfterSwitchPutsThePreviousBack(t *testing.T) {
	for _, op := range []string{"cleanRedis", "check"} {
		t.Run(op, func(t *testing.T) {
			e := newEnv(t)
			e.process("p1")
			e.db.fail[op] = errors.New(op + " failed")
			rec, _ := e.prepare()
			require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
			got := e.waitStep(rec.OpID, StepReverted)
			names := e.db.names()
			assert.Equal(t, "current", names[testDB], "the previous database is back")
			assert.Equal(t, "restored:20261010T120000Z", names[got.Aside], "the restored one is kept aside")
			assert.True(t, strings.HasPrefix(got.Aside, testDB+"_rolled_back_"))
			assert.NotContains(t, names, rec.Before)
			require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
		})
	}
}

// TestRestore_ResumeFailurePutsThePreviousBack (A3): the processes do not
// come back on the restored database within ResumeTimeout.
func TestRestore_ResumeFailurePutsThePreviousBack(t *testing.T) {
	// 期限を検査するのは再開の待ち (ResumeTimeout) だけ。プロセスは戻らないので、
	// 期限が負荷で遅れても結末は変わらない。held の待ちは既定の切れない期限のまま、
	// 判定役が held にするのを待つ。
	e := newEnv(t, func(o *Options) { o.ResumeTimeout = 500 * time.Millisecond })
	p := e.process("p1")
	p.setFailBack(true)
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepReverting, StepReverted)
	// 戻す処理がメンテナンスに入り直してから (resuming でなくなってから) 戻れるように
	// する。reverting を見てすぐ戻れるようにすると、まだ resuming のうちに判定役が
	// メンテナンスを外し、戻す処理は「戻した DB で動いているかもしれない」として運営者に
	// 渡す (unknown)。
	require.Eventually(t, func() bool {
		got, err := e.store.Get(context.Background(), rec.OpID)
		if err == nil && got.Step == StepReverted {
			return true
		}
		st := e.maintenance()
		return st != nil && st.OpID == rec.OpID && st.Phase != maintmode.PhaseResuming
	}, 10*time.Second, 5*time.Millisecond, "the maintenance is not entered again")
	p.setFailBack(false)
	e.waitStep(rec.OpID, StepReverted)
	assert.Equal(t, "current", e.db.content(testDB))
	// reverted を書く前に再開 (resuming) を出している。外れるまでは待たない: 戻した後の
	// 待ちも同じ短い ResumeTimeout で、負荷で切れると操作の判定役が止まり、外すのは
	// 本番なら各プロセスの判定役になる。
	st := e.maintenance()
	assert.True(t, st == nil || (st.OpID == rec.OpID && st.Phase == maintmode.PhaseResuming),
		"the processes are let back on the previous database: %+v", st)
}

// TestRestore_UnknownSwitchStopsInMaintenance (R5).
func TestRestore_UnknownSwitchStopsInMaintenance(t *testing.T) {
	e := newEnv(t)
	e.process("p1")
	e.db.switchErr = func(int) error { return backup.ErrRestoreSwitchUnknown }
	rec, _ := e.prepare()
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepUnknown)
	// 操作が終わる (枠を返す) のを待つ。決まった時間だけ眠る形だと、負荷で操作が
	// まだ枠を持っていて、下の Prepare が ErrMaintenanceActive でなく枠の取り合い
	// (busy) で断られた (#3369)。
	require.Eventually(t, func() bool { return !e.slot.isHeld() }, 10*time.Second, 5*time.Millisecond)
	st := e.maintenance()
	require.NotNil(t, st, "the maintenance stays on")
	assert.Equal(t, rec.OpID, st.OpID)
	assert.Empty(t, e.db.drops, "nothing is dropped while it cannot tell")
	assert.Contains(t, e.db.names(), rec.Target)
	require.Len(t, e.notes, 1)
	assert.Contains(t, e.notes[0], "elythia maintenance exit")
	cur, err := e.store.Current(context.Background())
	require.NoError(t, err)
	assert.Nil(t, cur, "a new operation is refused by the maintenance, not by a stuck marker")
	_, _, err = e.m.Prepare(context.Background(), PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
	assert.ErrorIs(t, err, ErrMaintenanceActive)
}

func TestRestore_RecordSurvivesTheRedisCleanup(t *testing.T) {
	// R7: 記録は Redis の後始末に消されない名前に置く。
	e := newEnv(t)
	rec, _ := e.prepare()
	_, err := backup.CleanRedisAfterRestore(context.Background(), backup.RedisCleanupTargets{
		Default:   backup.RedisCleanupTarget{Client: e.rdb, Prefix: "p:"},
		Timelines: backup.RedisCleanupTarget{Client: e.rdb, Prefix: "p:"},
		JobQueue:  backup.RedisCleanupTarget{Client: e.rdb},
	})
	require.NoError(t, err)
	got, err := e.store.Get(context.Background(), rec.OpID)
	require.NoError(t, err)
	assert.Equal(t, StepReady, got.Step)
	cur, err := e.store.Current(context.Background())
	require.NoError(t, err)
	require.NotNil(t, cur)
	journal, err := e.store.Journal(context.Background(), rec.OpID)
	require.NoError(t, err)
	assert.NotEmpty(t, journal)
}

func TestRestore_Ticket(t *testing.T) {
	// R10
	e := newEnv(t)
	rec, ticket := e.prepare()
	ctx := context.Background()
	_, err := e.m.CheckTicket(ctx, rec.OpID, ticket, "session-token-01")
	require.NoError(t, err)
	for _, c := range []struct{ name, ticket, token string }{
		{"other session", ticket, "session-token-02"},
		{"wrong ticket", ticket + "x", "session-token-01"},
		{"no ticket", "", "session-token-01"},
	} {
		_, err := e.m.CheckTicket(ctx, rec.OpID, c.ticket, c.token)
		assert.ErrorIs(t, err, ErrTicket, c.name)
	}
	_, err = e.m.CheckTicket(ctx, "restore-nope", ticket, "session-token-01")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestRestore_PrepareRefusals(t *testing.T) {
	ctx := context.Background()
	req := PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"}
	t.Run("invalid id", func(t *testing.T) {
		e := newEnv(t)
		_, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "latest"})
		assert.ErrorIs(t, err, ErrInvalidID)
	})
	t.Run("not verified", func(t *testing.T) {
		e := newEnv(t)
		e.db.verified = false
		_, _, err := e.m.Prepare(ctx, req)
		assert.ErrorIs(t, err, ErrNotVerified)
		assert.False(t, e.slot.isHeld(), "the slot is released")
		assert.Empty(t, e.db.drops)
		assert.NotContains(t, e.db.names(), "app_restore_", "nothing is created")
	})
	t.Run("replicas", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.Replicas = true })
		_, _, err := e.m.Prepare(ctx, req)
		assert.ErrorIs(t, err, ErrReplicas)
	})
	t.Run("version mismatch", func(t *testing.T) {
		// R12
		e := newEnv(t)
		require.NoError(t, e.maint.Report(ctx, maintmode.ProcReport{ID: "old", Version: "0.9", State: maintmode.ProcRunning}))
		_, _, err := e.m.Prepare(ctx, req)
		assert.ErrorIs(t, err, ErrVersionMismatch)
		assert.False(t, e.slot.isHeld())
	})
	t.Run("commit mismatch", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.Commit = "aaaa" })
		require.NoError(t, e.maint.Report(ctx, maintmode.ProcReport{ID: "p", Version: "1.0", Commit: "bbbb", State: maintmode.ProcRunning}))
		_, _, err := e.m.Prepare(ctx, req)
		assert.ErrorIs(t, err, ErrVersionMismatch)
	})
	t.Run("maintenance active", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "manual-1", Kind: maintmode.KindManual}))
		_, _, err := e.m.Prepare(ctx, req)
		assert.ErrorIs(t, err, ErrMaintenanceActive)
	})
	t.Run("busy slot", func(t *testing.T) {
		// R11: 定期の take が動いている間は始めない。
		e := newEnv(t)
		release, err := e.slot.acquire()
		require.NoError(t, err)
		defer release()
		_, _, err = e.m.Prepare(ctx, req)
		assert.Error(t, err)
	})
	t.Run("another operation", func(t *testing.T) {
		e := newEnv(t)
		e.prepare()
		_, _, err := e.m.Prepare(ctx, req)
		assert.Error(t, err)
		require.NoError(t, e.rdb.Set(ctx, e.store.CurrentKey(), "restore-x", 0).Err())
		release := func() {}
		e.opts.Acquire = func() (func(), error) { return release, nil }
		m := New(e.opts)
		m.Start(e.ctx)
		_, _, err = m.Prepare(ctx, req)
		assert.Error(t, err)
	})
	t.Run("load and checks", func(t *testing.T) {
		for _, op := range []string{"load", "checkTarget", "checkCompatibility"} {
			e := newEnv(t)
			e.db.fail[op] = errors.New(op + " failed")
			_, _, err := e.m.Prepare(ctx, req)
			assert.ErrorContains(t, err, op+" failed")
			assert.False(t, e.slot.isHeld())
		}
	})
	t.Run("not started", func(t *testing.T) {
		m := New(Options{})
		_, _, err := m.Prepare(ctx, req)
		assert.ErrorIs(t, err, ErrNotRunning)
		assert.ErrorIs(t, m.Confirm(ctx, "x"), ErrNotRunning)
	})
}

func TestRestore_AbortAndExpiry(t *testing.T) {
	ctx := context.Background()
	t.Run("abort while ready", func(t *testing.T) {
		e := newEnv(t)
		rec, _ := e.prepare()
		require.NoError(t, e.m.Abort(ctx, rec.OpID))
		got := e.waitStep(rec.OpID, StepAborted)
		assert.Equal(t, "aborted by the administrator", got.Error)
		assert.Equal(t, []string{rec.Target}, e.db.drops)
		// 起動時の回復が、準備の作った記録を見て枠を待っていると、中止の後に一度だけ枠を
		// 取って読み直す (何もせずに返す)。
		require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
		assert.ErrorIs(t, e.m.Confirm(ctx, rec.OpID), ErrStep, "an aborted operation cannot be confirmed")
		assert.ErrorIs(t, e.m.Abort(ctx, rec.OpID), ErrStep)
	})
	t.Run("abort while preparing", func(t *testing.T) {
		e := newEnv(t)
		e.db.block = make(chan struct{})
		rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
		require.NoError(t, err)
		require.NoError(t, e.m.Abort(ctx, rec.OpID))
		e.waitStep(rec.OpID, StepAborted)
		assert.Equal(t, []string{rec.Target}, e.db.drops)
		require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
	})
	t.Run("expiry", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.ReadyTimeout = 30 * time.Millisecond })
		rec, _, err := e.m.Prepare(ctx, PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
		require.NoError(t, err)
		e.waitStep(rec.OpID, StepExpired)
		assert.Equal(t, []string{rec.Target}, e.db.drops)
		require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
	})
	t.Run("confirm twice", func(t *testing.T) {
		e := newEnv(t)
		e.process("p1")
		rec, _ := e.prepare()
		require.NoError(t, e.m.Confirm(ctx, rec.OpID))
		assert.ErrorIs(t, e.m.Confirm(ctx, rec.OpID), ErrStep)
		assert.ErrorIs(t, e.m.Abort(ctx, rec.OpID), ErrStep, "a confirmed operation cannot be aborted")
		e.waitStep(rec.OpID, StepDone)
	})
	t.Run("unknown operation", func(t *testing.T) {
		e := newEnv(t)
		assert.ErrorIs(t, e.m.Abort(ctx, "restore-nope"), ErrNotFound)
		assert.ErrorIs(t, e.m.Confirm(ctx, "restore-nope"), ErrStep)
	})
}

func TestRestore_EnterRefusedByAnotherMaintenance(t *testing.T) {
	e := newEnv(t)
	rec, _ := e.prepare()
	require.NoError(t, e.maint.Enter(context.Background(), maintmode.State{OpID: "manual-1", Kind: maintmode.KindManual}))
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepAborted)
	st := e.maintenance()
	require.NotNil(t, st)
	assert.Equal(t, "manual-1", st.OpID, "the other maintenance is left alone")
	assert.Equal(t, "current", e.db.content(testDB))
}

// TestPrepare_KeepsADatabaseItDidNotCreate checks that a failed prepare does
// not drop a database of the target's name that another one created after
// the checks (#3463 (6c) L-6).
func TestPrepare_KeepsADatabaseItDidNotCreate(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		db := o.DB.(*fakeDB)
		// 確かめた後、作る前に、同じ名前の DB を別の作業が作る。
		o.DB = createsFirst{db}
	})
	rec, _, err := e.m.Prepare(context.Background(), PrepareRequest{GenerationID: "20261010T120000Z", UserID: "u1", Token: "t"})
	require.NoError(t, err)
	got := e.waitStep(rec.OpID, StepFailed)
	assert.Contains(t, got.Error, "already exists")
	assert.Equal(t, "someone else's", e.db.content(rec.Target))
	assert.Empty(t, e.db.drops)
}

// createsFirst creates the target as another one's right before restoring.
type createsFirst struct{ *fakeDB }

func (c createsFirst) RestoreInto(ctx context.Context, plan *backup.RestorePlan, opts backup.RestoreOptions, dbname string) error {
	c.mu.Lock()
	c.dbs[dbname] = "someone else's"
	c.mu.Unlock()
	return c.fakeDB.RestoreInto(ctx, plan, opts, dbname)
}

func TestValidOpID(t *testing.T) {
	assert.True(t, ValidOpID(newOpID()))
	for _, bad := range []string{"", "restore-", "restore-0123456789ABCDEF", "restore-0123456789abcde", "restore-0123456789abcdef0", " restore-0123456789abcdef", "restore-0123456789abcdef\n"} {
		assert.False(t, ValidOpID(bad), bad)
	}
}

// 準備の後に記録の名前が書き換えられたら、確定しても名前を入れ替えずに運営者へ
// 渡すこと (Redis に書ける者が target を別の DB にしてから管理者が確定する経路)。
func TestRestore_ConfirmRefusesTamperedRecord(t *testing.T) {
	e := newEnv(t)
	e.process("p1")
	rec, ticket := e.prepare()
	e.db.mu.Lock()
	e.db.dbs["victim"] = "someone else's"
	e.db.mu.Unlock()
	require.NoError(t, e.rdb.HSet(context.Background(), e.store.key(rec.OpID), "target", "victim").Err())

	got, err := e.m.CheckTicket(context.Background(), rec.OpID, ticket, "session-token-01")
	require.NoError(t, err)
	require.Error(t, e.m.Confirm(context.Background(), got.OpID))
	e.waitStep(rec.OpID, StepUnknown)
	assert.Equal(t, 0, e.db.switches, "書き換えられた記録で名前を入れ替えた")
	assert.Equal(t, "someone else's", e.db.content("victim"))
	assert.Equal(t, "current", e.db.content(testDB))
}
