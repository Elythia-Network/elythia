package restoreop

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/maintmode"
)

// Names of a rollback in the tests: the kept database it swaps back in, and
// the name the current database is kept under.
const (
	keptDB  = testDB + "_before_restore_20261001000000"
	asideDB = testDB + "_rolled_back_20261010120000"
)

// crashedRollback is crashed for a rollback record.
func crashedRollback(t *testing.T, step string, names map[string]string, phase string) (*env, *Record) {
	t.Helper()
	e := newEnv(t)
	e.stop()
	e.process("p1")
	ctx := context.Background()
	now := time.Now()
	rec := &Record{
		OpID: "restore-rollback", Kind: KindRollback, Step: step, Database: testDB,
		Target: keptDB, Before: asideDB, UserID: "u1", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, e.store.create(ctx, rec))
	e.db.mu.Lock()
	e.db.dbs = map[string]string{}
	for k, v := range names {
		e.db.dbs[k] = v
	}
	markSwitchedOut(e, rec)
	e.db.mu.Unlock()
	if phase != "" {
		require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: rec.OpID, Kind: maintmode.KindRollback, Phase: phase}))
	}
	e.start()
	return e, rec
}

// TestRecover_RollbackKeepsTheKeptDatabase runs the recovery table for a
// rollback: every row that drops the new database of a restore keeps the
// database of a rollback, which is the kept one it was going to swap in.
func TestRecover_RollbackKeepsTheKeptDatabase(t *testing.T) {
	notSwapped := map[string]string{testDB: "current", keptDB: "kept"}
	swapped := map[string]string{testDB: "kept", asideDB: "current"}

	for _, c := range []struct {
		name, step, phase, final string
	}{
		{"preparing", StepPreparing, "", StepFailed},
		{"ready", StepReady, "", StepAborted},
		{"aborting in the maintenance", StepAborting, maintmode.PhaseEntering, StepAborted},
		{"entering before the switch", StepEntering, maintmode.PhaseHeld, StepAborted},
		{"entering before Enter", StepEntering, "", StepAborted},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, rec := crashedRollback(t, c.step, notSwapped, c.phase)
			e.waitStep(rec.OpID, c.final)
			assert.Equal(t, notSwapped, e.db.names(), "the kept database stays")
			assert.Empty(t, e.db.drops)
			e.waitMaintenanceGone(2 * time.Second)
		})
	}

	t.Run("switched puts the kept database back under its own name", func(t *testing.T) {
		e, rec := crashedRollback(t, StepSwitched, swapped, maintmode.PhaseHeld)
		got := e.waitStep(rec.OpID, StepReverted)
		assert.Equal(t, keptDB, got.Aside)
		assert.Equal(t, notSwapped, e.db.names())
		assert.Empty(t, e.db.drops)
	})
	t.Run("entering after the switch puts the kept database back", func(t *testing.T) {
		e, rec := crashedRollback(t, StepEntering, swapped, maintmode.PhaseHeld)
		e.waitStep(rec.OpID, StepReverted)
		assert.Equal(t, notSwapped, e.db.names())
	})
	t.Run("reverting after the swap back", func(t *testing.T) {
		e := newEnv(t)
		e.stop()
		ctx := context.Background()
		require.NoError(t, e.store.create(ctx, &Record{
			OpID: "restore-rollback", Kind: KindRollback, Step: StepReverting, Database: testDB,
			Target: keptDB, Before: asideDB, Aside: keptDB,
		}))
		e.db.dbs = map[string]string{testDB: "current", keptDB: "kept"}
		require.NoError(t, e.maint.Enter(ctx, maintmode.State{OpID: "restore-rollback", Kind: maintmode.KindRollback, Phase: maintmode.PhaseHeld}))
		e.start()
		e.process("p1")
		e.waitStep("restore-rollback", StepReverted)
		assert.Equal(t, 0, e.db.switches)
		assert.Equal(t, map[string]string{testDB: "current", keptDB: "kept"}, e.db.names())
	})
	t.Run("resuming is done on the kept database", func(t *testing.T) {
		e, rec := crashedRollback(t, StepResuming, swapped, "")
		e.waitStep(rec.OpID, StepDone)
		assert.Equal(t, swapped, e.db.names())
	})
}

// TestDropTarget_NeverDropsAKeptDatabase checks the guard of dropTarget by
// name, whatever the kind of the record says.
func TestDropTarget_NeverDropsAKeptDatabase(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, target := range []string{
		testDB,
		"",
		testDB + "_before_restore_20261001000000",
		testDB + "_rolled_back_20261001000000",
	} {
		e.db.dbs[target] = "kept"
		e.m.dropTarget(ctx, &Record{OpID: "restore-guard", Kind: KindRestore, Database: testDB, Target: target})
	}
	// 記録の Database が食い違っていても、サービスの DB の名前でも守る。
	e.m.dropTarget(ctx, &Record{OpID: "restore-guard", Kind: KindRestore, Database: "other", Target: testDB + "_before_restore_20261001000001"})
	assert.Empty(t, e.db.drops)

	// リストアが作った DB は消す。
	e.db.dbs[testDB+"_restore_20261010120000"] = "restored"
	e.m.dropTarget(ctx, &Record{OpID: "restore-guard", Kind: KindRestore, Database: testDB, Target: testDB + "_restore_20261010120000"})
	assert.Equal(t, []string{testDB + "_restore_20261010120000"}, e.db.drops)
	// ロールバックは、名前の形にかかわらず消さない。
	e.m.dropTarget(ctx, &Record{OpID: "restore-guard", Kind: KindRollback, Database: testDB, Target: "something_else"})
	assert.Equal(t, []string{testDB + "_restore_20261010120000"}, e.db.drops)
}

// TestRecordKind checks that a record keeps its kind and that one written
// before the field existed reads as a restore.
func TestRecordKind(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-kind", Kind: KindRollback, Step: StepPreparing}))
	rec, err := e.store.Get(ctx, "restore-kind")
	require.NoError(t, err)
	assert.Equal(t, KindRollback, rec.Kind)
	require.NoError(t, e.rdb.HDel(ctx, e.store.key("restore-kind"), "kind").Err())
	rec, err = e.store.Get(ctx, "restore-kind")
	require.NoError(t, err)
	assert.Equal(t, KindRestore, rec.Kind)
}

// TestRecord_OldReaderSeesNoTarget checks what a backup service of the
// previous version (#3463 (6c), which reads target and knows neither kind
// nor keptTarget) sees in a rollback record: no target, so its recovery and
// abort have nothing to drop.
func TestRecord_OldReaderSeesNoTarget(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-old", Kind: KindRollback, Step: StepReady, Database: testDB, Target: keptDB, Before: asideDB}))
	raw, err := e.rdb.HGetAll(ctx, e.store.key("restore-old")).Result()
	require.NoError(t, err)
	assert.Equal(t, "", raw["target"], "the previous version reads target as the database it created")
	assert.Equal(t, keptDB, raw["keptTarget"])
	rec, err := e.store.Get(ctx, "restore-old")
	require.NoError(t, err)
	assert.Equal(t, keptDB, rec.Target, "this version reads the kept database back")

	// 古い版が段を進めても (setStep は target を書かない)、空のまま。
	require.NoError(t, e.store.setStep(ctx, "restore-old", StepReady, StepAborting, time.Now(), "error", "x"))
	raw, err = e.rdb.HGetAll(ctx, e.store.key("restore-old")).Result()
	require.NoError(t, err)
	assert.Equal(t, "", raw["target"])

	// リストアの記録は今までどおり target に置く。
	require.NoError(t, e.rdb.Del(ctx, e.store.CurrentKey()).Err())
	require.NoError(t, e.store.create(ctx, &Record{OpID: "restore-new", Kind: KindRestore, Step: StepReady, Database: testDB, Target: tgt}))
	raw, err = e.rdb.HGetAll(ctx, e.store.key("restore-new")).Result()
	require.NoError(t, err)
	assert.Equal(t, tgt, raw["target"])
	assert.Equal(t, "", raw["keptTarget"])
}
