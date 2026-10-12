package restoreop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// withKept adds the kept database the rollback tests swap back in.
func (e *env) withKept() {
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	e.db.dbs[keptDB] = "kept"
}

func (e *env) rollback() (*Record, string) {
	e.t.Helper()
	rec, ticket, err := e.m.PrepareRollback(context.Background(), RollbackRequest{Database: keptDB, UserID: "u1", Token: "session-token-01"})
	require.NoError(e.t, err)
	e.waitStep(rec.OpID, StepReady)
	return rec, ticket
}

// assertKept checks that the kept database is still there under its name,
// whatever happened.
func assertKept(t *testing.T, e *env) {
	t.Helper()
	assert.Equal(t, "kept", e.db.content(keptDB), "the kept database stays")
	e.db.mu.Lock()
	defer e.db.mu.Unlock()
	assert.NotContains(t, e.db.drops, keptDB)
	assert.NotContains(t, e.db.drops, testDB)
}

func TestRollback_HappyPath(t *testing.T) {
	e := newEnv(t)
	e.withKept()
	p := e.process("p1")
	rec, ticket := e.rollback()
	assert.Equal(t, KindRollback, rec.Kind)
	assert.Equal(t, keptDB, rec.Target)
	assert.True(t, strings.HasPrefix(rec.Before, testDB+"_rolled_back_"), rec.Before)
	_, _, ok := backup.ParseKeptName(testDB, rec.Before)
	assert.True(t, ok, "the current database is kept under a name a rollback can take")
	assert.True(t, e.slot.isHeld())
	assert.Empty(t, e.db.migratedNames(), "a kept database at the bundled version is not migrated")

	// 接続が残っている間に、メンテナンスの種類を見る。
	e.db.mu.Lock()
	e.db.conns[testDB] = 1
	e.db.mu.Unlock()
	got, err := e.m.CheckTicket(context.Background(), rec.OpID, ticket, "session-token-01")
	require.NoError(t, err)
	assert.Equal(t, KindRollback, got.Kind)
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	require.Eventually(t, func() bool { return e.maintenance() != nil }, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, maintmode.KindRollback, e.maintenance().Kind)
	e.db.mu.Lock()
	e.db.conns[testDB] = 0
	e.db.mu.Unlock()

	e.waitStep(rec.OpID, StepDone)
	assert.Equal(t, map[string]string{testDB: "kept", rec.Before: "current", "postgres": "maint"}, e.db.names())
	assert.Empty(t, e.db.drops)
	assert.Nil(t, e.maintenance())
	assert.Equal(t, 1, e.cleaned)
	assert.Equal(t, 1, e.checked)
	assert.Equal(t, []string{
		"rollback:app:current:prepare:" + keptDB + ":" + rec.Before,
		"rollback:app:kept:switch:" + keptDB + ":" + rec.Before,
	}, e.modlogs, "the prepare is logged in the current database, the switch in the kept one")
	require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
	p.mu.Lock()
	assert.Equal(t, "kept", p.serving)
	p.mu.Unlock()
	st, err := e.m.Status(context.Background(), got)
	require.NoError(t, err)
	assert.Contains(t, st.Journal[len(st.Journal)-1].Message, "rolled back to "+keptDB)

	// 戻したロールバックを、退避した側から戻し直せる (取り消し)。
	e.db.mu.Lock()
	e.db.dbs[rec.Before] = "current"
	e.db.mu.Unlock()
	undo, _, err := e.m.PrepareRollback(context.Background(), RollbackRequest{Database: rec.Before, UserID: "u1", Token: "t"})
	if err != nil {
		// 同じ秒の中では、脇へ置く名前が戻す先と同じになるので断られる。
		require.ErrorContains(t, err, "already exists")
		return
	}
	e.waitStep(undo.OpID, StepReady)
	require.NoError(t, e.m.Confirm(context.Background(), undo.OpID))
	e.waitStep(undo.OpID, StepDone)
	assert.Equal(t, "current", e.db.content(testDB))
}

func TestRollback_MigratesAnOlderKeptDatabase(t *testing.T) {
	e := newEnv(t)
	e.withKept()
	e.db.needsMigration = true
	e.process("p1")
	rec, _ := e.rollback()
	assert.Equal(t, []string{keptDB}, e.db.migratedNames(), "migrated before the switch")
	require.NoError(t, e.m.Confirm(context.Background(), rec.OpID))
	e.waitStep(rec.OpID, StepDone)
	assert.Equal(t, "kept", e.db.content(testDB))
}

// TestRollback_RefusedBeforeStart checks the refusals that leave nothing
// behind: no record, the job slot given back, no database touched.
func TestRollback_RefusedBeforeStart(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		kept  string
		setup func(*env)
		want  error
		text  string
	}{
		{name: "not a kept name", kept: "app_restore_20261001000000", want: ErrInvalidKept},
		{name: "the server's database", kept: testDB, want: ErrInvalidKept},
		{name: "kept of another database", kept: "other_before_restore_20261001000000", want: ErrInvalidKept},
		{name: "missing", kept: testDB + "_before_restore_20200101000000", want: ErrNoSuchKept},
		{name: "someone on it", kept: keptDB, setup: func(e *env) { e.db.conns[keptDB] = 1 }, want: backup.ErrRestoreConnections},
		{name: "newer", kept: keptDB, setup: func(e *env) { e.db.fail["migrations"] = fmt.Errorf("x: %w", backup.ErrRestoreMigrationTooNew) }, want: backup.ErrRestoreMigrationTooNew},
		{name: "dirty", kept: keptDB, setup: func(e *env) { e.db.fail["migrations"] = fmt.Errorf("x: %w", backup.ErrRestoreMigrationDirty) }, want: backup.ErrRestoreMigrationDirty},
		{name: "no privilege", kept: keptDB, setup: func(e *env) { e.db.fail["checkRollback"] = backup.ErrRestoreNoPrivilege }, want: backup.ErrRestoreNoPrivilege},
		{name: "pg_database unreadable", kept: keptDB, setup: func(e *env) { e.db.fail["existing"] = errors.New("down") }, text: "down"},
		{name: "maintenance on", kept: keptDB, setup: func(e *env) {
			require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "manual-1", Kind: maintmode.KindManual}))
		}, want: ErrMaintenanceActive},
		{name: "another version", kept: keptDB, setup: func(e *env) {
			require.NoError(t, e.maint.Report(ctx, maintmode.ProcReport{ID: "p9", Version: "0.9", State: maintmode.ProcRunning}))
		}, want: ErrVersionMismatch},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.withKept()
			if c.setup != nil {
				c.setup(e)
			}
			_, _, err := e.m.PrepareRollback(ctx, RollbackRequest{Database: c.kept, UserID: "u1", Token: "t"})
			require.Error(t, err)
			if c.want != nil {
				require.ErrorIs(t, err, c.want)
			} else {
				assert.ErrorContains(t, err, c.text)
			}
			cur, err := e.store.Current(ctx)
			require.NoError(t, err)
			assert.Nil(t, cur, "no record is left")
			assert.False(t, e.slot.isHeld(), "the job slot is given back")
			assert.Empty(t, e.db.migratedNames())
			assertKept(t, e)
		})
	}
	t.Run("the name is checked before the job slot", func(t *testing.T) {
		e := newEnv(t)
		release, err := e.slot.acquire()
		require.NoError(t, err)
		defer release()
		_, _, err = e.m.PrepareRollback(ctx, RollbackRequest{Database: "nope"})
		assert.ErrorIs(t, err, ErrInvalidKept)
	})
	t.Run("not running", func(t *testing.T) {
		_, _, err := New(Options{}).PrepareRollback(ctx, RollbackRequest{Database: keptDB})
		assert.ErrorIs(t, err, ErrNotRunning)
	})
	t.Run("job slot busy", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		release, err := e.slot.acquire()
		require.NoError(t, err)
		defer release()
		_, _, err = e.m.PrepareRollback(ctx, RollbackRequest{Database: keptDB})
		assert.Error(t, err)
	})
	t.Run("record not written", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		require.NoError(t, e.rdb.Set(ctx, e.store.CurrentKey(), "restore-other", 0).Err())
		// canStart は Current を読むので、記録の無い印は読めない誤りになる。
		_, _, err := e.m.PrepareRollback(ctx, RollbackRequest{Database: keptDB})
		assert.Error(t, err)
		assert.False(t, e.slot.isHeld())
	})
}

// TestRollback_NeverDropsTheKeptDatabase walks every path that drops the
// new database of a restore (prepare failure, abort, expiry, A1) with a
// rollback.
func TestRollback_NeverDropsTheKeptDatabase(t *testing.T) {
	ctx := context.Background()
	t.Run("migration fails", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		e.db.needsMigration = true
		e.db.fail["migrate"] = errors.New("migrate failed")
		rec, _, err := e.m.PrepareRollback(ctx, RollbackRequest{Database: keptDB, UserID: "u1", Token: "t"})
		require.NoError(t, err)
		got := e.waitStep(rec.OpID, StepFailed)
		assert.Contains(t, got.Error, "migrate failed")
		assertKept(t, e)
		assert.Equal(t, "current", e.db.content(testDB))
		require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
	})
	t.Run("abort while preparing", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		e.db.needsMigration = true
		e.db.migrateBlock = make(chan struct{})
		rec, _, err := e.m.PrepareRollback(ctx, RollbackRequest{Database: keptDB, UserID: "u1", Token: "t"})
		require.NoError(t, err)
		require.NoError(t, e.m.Abort(ctx, rec.OpID))
		close(e.db.migrateBlock)
		e.waitStep(rec.OpID, StepAborted)
		assertKept(t, e)
		require.Eventually(t, func() bool { return !e.slot.isHeld() }, time.Second, 5*time.Millisecond)
	})
	t.Run("abort while ready", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		rec, _ := e.rollback()
		require.NoError(t, e.m.Abort(ctx, rec.OpID))
		e.waitStep(rec.OpID, StepAborted)
		assertKept(t, e)
	})
	t.Run("expiry", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.ReadyTimeout = 30 * time.Millisecond })
		e.withKept()
		rec, _, err := e.m.PrepareRollback(ctx, RollbackRequest{Database: keptDB, UserID: "u1", Token: "t"})
		require.NoError(t, err)
		e.waitStep(rec.OpID, StepExpired)
		assertKept(t, e)
	})
	t.Run("not let go (A1)", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		e.process("p1")
		e.db.mu.Lock()
		e.db.conns[testDB] = 1
		e.db.mu.Unlock()
		rec, _ := e.rollback()
		require.NoError(t, e.m.Confirm(ctx, rec.OpID))
		got := e.waitStep(rec.OpID, StepAborted)
		assert.Contains(t, got.Error, "did not let go")
		assertKept(t, e)
		assert.Equal(t, "current", e.db.content(testDB))
		require.Eventually(t, func() bool { return e.maintenance() == nil }, 2*time.Second, 5*time.Millisecond)
	})
	t.Run("switch refused (A1)", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		e.process("p1")
		e.db.switchErr = func(int) error { return errors.New("database is being accessed by other users") }
		rec, _ := e.rollback()
		require.NoError(t, e.m.Confirm(ctx, rec.OpID))
		e.waitStep(rec.OpID, StepAborted)
		assertKept(t, e)
	})
	t.Run("failure after the switch puts the kept database back", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		e.process("p1")
		e.db.fail["check"] = errors.New("check failed")
		rec, _ := e.rollback()
		require.NoError(t, e.m.Confirm(ctx, rec.OpID))
		got := e.waitStep(rec.OpID, StepReverted)
		assert.Equal(t, keptDB, got.Aside, "the kept database goes back under its own name")
		assert.Equal(t, map[string]string{testDB: "current", keptDB: "kept", "postgres": "maint"}, e.db.names())
		assert.Empty(t, e.db.drops)
	})
}

func TestRollback_GiveUpNamesTheRollback(t *testing.T) {
	weird := map[string]string{testDB: "x", keptDB: "y", asideDB: "z"}
	e, rec := crashedRollback(t, StepEntering, weird, maintmode.PhaseHeld)
	e.waitStep(rec.OpID, StepUnknown)
	require.Eventually(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.notes) == 1
	}, 5*time.Second, 5*time.Millisecond)
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.Contains(t, e.notes[0], "rollback restore-rollback stopped")
	assert.Contains(t, e.notes[0], keptDB)
	assert.Equal(t, weird, e.db.names())
}

func TestCandidates(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.withKept()
	e.db.dbs[asideDB] = "old"
	e.db.dbs[testDB+"_restore_20261010120000"] = "restoring"
	list, err := e.m.Candidates(ctx)
	require.NoError(t, err)
	var names []string
	for _, k := range list {
		names = append(names, k.Name)
	}
	assert.ElementsMatch(t, []string{keptDB, asideDB}, names)
	assert.Equal(t, testDB, e.m.Database())

	delete(e.db.dbs, keptDB)
	delete(e.db.dbs, asideDB)
	list, err = e.m.Candidates(ctx)
	require.NoError(t, err)
	assert.NotNil(t, list, "an empty list is [] in JSON")

	e.db.fail["kept"] = errors.New("down")
	_, err = e.m.Candidates(ctx)
	assert.ErrorContains(t, err, "down")
	delete(e.db.fail, "kept")

	// 操作の途中とメンテナンス中は、退避した DB へ繋がない。
	e.withKept()
	rec, _ := e.rollback()
	_, err = e.m.Candidates(ctx)
	assert.ErrorIs(t, err, ErrBusy)
	require.NoError(t, e.m.Abort(ctx, rec.OpID))
	e.waitStep(rec.OpID, StepAborted)
	require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "manual-1", Kind: maintmode.KindManual}))
	_, err = e.m.Candidates(ctx)
	assert.ErrorIs(t, err, ErrMaintenanceActive)
	require.NoError(t, e.rdb.Set(ctx, e.store.CurrentKey(), "restore-gone", 0).Err())
	_, err = e.m.Candidates(ctx)
	assert.ErrorContains(t, err, "has no record")
}

func TestKindHelpers(t *testing.T) {
	rb := &Record{Kind: KindRollback}
	rs := &Record{Kind: KindRestore}
	old := &Record{}
	assert.Equal(t, "rollback", kindWord(rb))
	assert.Equal(t, "restore", kindWord(rs))
	assert.Equal(t, "restore", kindWord(old))
	assert.Equal(t, maintmode.KindRollback, maintKind(rb))
	assert.Equal(t, maintmode.KindRestore, maintKind(rs))
	assert.Equal(t, maintmode.KindRestore, maintKind(old))
	assert.Equal(t, "kept", swappedIn(rb))
	assert.Equal(t, "restored", swappedIn(rs))
}

// journalText joins the journal of opID.
func (e *env) journalText(opID string) string {
	e.t.Helper()
	j, err := e.store.Journal(context.Background(), opID)
	require.NoError(e.t, err)
	var b strings.Builder
	for _, x := range j {
		b.WriteString(x.Message + "\n")
	}
	return b.String()
}

// TestRollback_JournalSaysKept checks that the journal of a rollback that
// ended before the switch says the kept database stays, not that it was
// dropped (the restore's wording).
func TestRollback_JournalSaysKept(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, text string) {
		t.Helper()
		assert.NotContains(t, text, "dropped")
		assert.Contains(t, text, "kept "+keptDB+" as it is")
	}
	t.Run("abort while ready", func(t *testing.T) {
		e := newEnv(t)
		e.withKept()
		rec, _ := e.rollback()
		require.NoError(t, e.m.Abort(ctx, rec.OpID))
		e.waitStep(rec.OpID, StepAborted)
		check(t, e.journalText(rec.OpID))
	})
	t.Run("expiry", func(t *testing.T) {
		e := newEnv(t, func(o *Options) { o.ReadyTimeout = 30 * time.Millisecond })
		e.withKept()
		rec, _, err := e.m.PrepareRollback(ctx, RollbackRequest{Database: keptDB, UserID: "u1", Token: "t"})
		require.NoError(t, err)
		e.waitStep(rec.OpID, StepExpired)
		check(t, e.journalText(rec.OpID))
	})
	t.Run("recovery", func(t *testing.T) {
		e, rec := crashedRollback(t, StepReady, map[string]string{testDB: "current", keptDB: "kept"}, "")
		e.waitStep(rec.OpID, StepAborted)
		check(t, e.journalText(rec.OpID))
	})
	t.Run("a restore still says dropped", func(t *testing.T) {
		e := newEnv(t)
		rec, _ := e.prepare()
		require.NoError(t, e.m.Abort(ctx, rec.OpID))
		e.waitStep(rec.OpID, StepAborted)
		assert.Contains(t, e.journalText(rec.OpID), "dropped "+rec.Target)
	})
}
