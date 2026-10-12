package backup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gomigrate "github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseKeptName(t *testing.T) {
	at := time.Date(2026, 10, 1, 2, 3, 4, 0, time.UTC)
	for _, c := range []struct {
		name string
		kind string
	}{
		{"app_before_restore_20261001020304", KeptBeforeRestore},
		{"app_rolled_back_20261001020304", KeptRolledBack},
	} {
		kind, got, ok := ParseKeptName("app", c.name)
		require.True(t, ok, c.name)
		assert.Equal(t, c.kind, kind, c.name)
		assert.True(t, at.Equal(got), c.name)
		assert.Equal(t, time.UTC, got.Location())
	}
	for _, name := range []string{
		"app",
		"app_restore_20261001020304",
		// LIKE の _ は任意の 1 文字なので、LIKE で拾うとこれも当たる。
		"appXbefore_restore_20261001020304",
		"app_beforeXrestore_20261001020304",
		"app2_before_restore_20261001020304",
		"other_before_restore_20261001020304",
		"app_before_restore_2026100102030",
		"app_before_restore_202610010203045",
		"app_before_restore_2026100102030x",
		"app_before_restore_+2026100102030",
		"app_before_restore_20261301020304",
		"app_rolled_back_",
		"",
	} {
		_, _, ok := ParseKeptName("app", name)
		assert.False(t, ok, name)
	}
}

func TestRollbackName(t *testing.T) {
	at := time.Date(2026, 10, 10, 21, 4, 5, 0, time.FixedZone("JST", 9*3600))
	name, err := RollbackName("app", at)
	require.NoError(t, err)
	assert.Equal(t, "app_rolled_back_20261010120405", name, "the time is in UTC")
	_, _, ok := ParseKeptName("app", name)
	assert.True(t, ok, "a rollback can be undone from the admin page")

	long := strings.Repeat("a", maxIdentifierLen-len("_rolled_back_20261010120405"))
	_, err = RollbackName(long, at)
	require.NoError(t, err)
	_, err = RollbackName(long+"a", at)
	assert.ErrorContains(t, err, "longer than 63 bytes")
}

func TestKeptProblem(t *testing.T) {
	assert.Equal(t, "", KeptProblem(nil))
	assert.Equal(t, KeptProblemTooNew, KeptProblem(fmt.Errorf("x: %w", ErrRestoreMigrationTooNew)))
	assert.Equal(t, KeptProblemDirty, KeptProblem(fmt.Errorf("x: %w", ErrRestoreMigrationDirty)))
	assert.Equal(t, KeptProblemNotElythia, KeptProblem(fmt.Errorf("x: %w", ErrRestoreNotElythia)))
	assert.Equal(t, KeptProblemUnreadable, KeptProblem(errors.New("connection refused")))
}

// TestRollbackChecks lists the kept databases and runs the stop conditions
// of a rollback against a real PostgreSQL (#3463 (6d)).
func TestRollbackChecks(t *testing.T) {
	pg := rsStartPG(t, "postgres:18-alpine")
	rsSetupInstance(t, pg, true)
	ctx := context.Background()
	_, count, err := latestMigration(rsCoreMigrations)
	require.NoError(t, err)
	kept := func(name string, steps int, sqls ...string) {
		t.Helper()
		pg.exec(t, "postgres", "CREATE DATABASE "+name+" OWNER "+rsAppUser)
		if steps > 0 {
			m, err := gomigrate.New("file://"+rsCoreMigrations, pg.url("pgx5", rsAppUser, rsAppPass, name))
			require.NoError(t, err)
			require.NoError(t, m.Steps(steps))
			_, _ = m.Close()
		}
		if len(sqls) > 0 {
			pg.exec(t, name, sqls...)
		}
	}
	const (
		older   = rsAppDB + "_before_restore_20261001000000"
		current = rsAppDB + "_rolled_back_20261003000000"
		dirty   = rsAppDB + "_rolled_back_20261002000000"
		empty   = rsAppDB + "_before_restore_20261004000000"
	)
	kept(older, count-1)
	kept(current, count)
	kept(dirty, count, `UPDATE schema_migrations SET dirty = true`)
	kept(empty, 0)
	// 一覧に出ないもの: LIKE なら当たる名前、時刻の形でない名前、別の DB の退避。
	kept(rsAppDB+"Xbefore_restore_20261005000000", 0)
	kept(rsAppDB+"_before_restore_2026", 0)
	kept("other_before_restore_20261006000000", 0)
	// 繋げない DB も、一覧ごと落とさずに出す (大きさと版は分からない)。
	const locked = rsAppDB + "_before_restore_20261008000000"
	pg.exec(t, "postgres", "CREATE DATABASE "+locked, "REVOKE CONNECT ON DATABASE "+locked+" FROM PUBLIC")

	app := pg.restorer(newRSStorage(), rsAppUser, rsAppPass)
	list, err := app.KeptDatabases(ctx, "", rsAppDB)
	require.NoError(t, err)
	var names []string
	for _, k := range list {
		names = append(names, k.Name)
	}
	assert.Equal(t, []string{locked, empty, current, dirty, older}, names, "newest first")
	byName := map[string]KeptDatabase{}
	for _, k := range list {
		byName[k.Name] = k
	}
	assert.Equal(t, KeptBeforeRestore, byName[older].Kind)
	assert.Equal(t, KeptRolledBack, byName[current].Kind)
	assert.True(t, byName[older].NeedsMigration)
	assert.Empty(t, byName[older].Problem)
	assert.False(t, byName[current].NeedsMigration)
	assert.Empty(t, byName[current].Problem)
	require.NotNil(t, byName[current].Size)
	assert.Positive(t, *byName[current].Size)
	assert.Len(t, byName[current].Migrations, 2)
	assert.Equal(t, KeptProblemDirty, byName[dirty].Problem)
	assert.Equal(t, KeptProblemNotElythia, byName[empty].Problem)
	assert.Nil(t, byName[locked].Size)
	assert.Nil(t, byName[locked].Migrations)
	assert.Equal(t, KeptProblemUnreadable, byName[locked].Problem)
	assert.Equal(t, time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), byName[current].At)

	// 前の版の daemon がロールバックの記録を読むと、戻す先の名前は空になる
	// (restoreop の keptTarget)。空の名前では何も消えず、何も無いことになる。
	before := rsDatabasesLike(t, pg, "%")
	require.Error(t, app.DropDatabase(ctx, "", ""))
	assert.Equal(t, before, rsDatabasesLike(t, pg, "%"), "nothing is dropped")
	has, err := app.ExistingDatabases(ctx, "", []string{rsAppDB, ""})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{rsAppDB: true}, has)

	none, err := app.KeptDatabases(ctx, "", "nothing_kept")
	require.NoError(t, err)
	assert.NotNil(t, none)
	assert.Empty(t, none)

	t.Run("check rollback", func(t *testing.T) {
		keepAs := rsAppDB + "_rolled_back_20261010120000"
		require.NoError(t, app.CheckRollback(ctx, "", rsAppDB, current, keepAs))
		// 脇へ置く名前が取られている。
		err := app.CheckRollback(ctx, "", rsAppDB, current, older)
		assert.ErrorContains(t, err, "already exists")
		// 戻す先が無い。
		err = app.CheckRollback(ctx, "", rsAppDB, rsAppDB+"_rolled_back_20200101000000", keepAs)
		assert.ErrorContains(t, err, "does not exist")
		// 戻す先に人が繋いでいる。
		held := pg.connect(t, rsSuperUser, rsSuperPass, current)
		err = app.CheckRollback(ctx, "", rsAppDB, current, keepAs)
		require.ErrorIs(t, err, ErrRestoreConnections)
		assert.Contains(t, err.Error(), current)
		_ = held.Close(ctx)
		require.Eventually(t, func() bool { return app.CheckRollback(ctx, "", rsAppDB, current, keepAs) == nil },
			5*time.Second, 50*time.Millisecond)
		// 戻す先の持ち主でなければ、名前を変えられない。
		pg.exec(t, "postgres", "CREATE DATABASE "+rsAppDB+"_before_restore_20261007000000")
		err = app.CheckRollback(ctx, "", rsAppDB, rsAppDB+"_before_restore_20261007000000", keepAs)
		require.ErrorIs(t, err, ErrRestoreNoPrivilege)
		assert.Contains(t, err.Error(), "ownership")
		// CREATEDB が無ければ、どちらも変えられない。
		pg.exec(t, "postgres", "ALTER ROLE "+rsAppUser+" NOCREATEDB")
		t.Cleanup(func() { pg.exec(t, "postgres", "ALTER ROLE "+rsAppUser+" CREATEDB") })
		err = app.CheckRollback(ctx, "", rsAppDB, current, keepAs)
		require.ErrorIs(t, err, ErrRestoreNoPrivilege)
	})
}
