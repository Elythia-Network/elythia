package migrate

import (
	"bytes"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
)

// dbFixture is a dedicated schema plus migration directories laid out like the
// repository (local/ under the core directory).
type dbFixture struct {
	db                *gorm.DB
	schema            string
	coreDir, localDir string
}

// newDBFixture recreates the sibling schema `<package>_<suffix>` from scratch.
//
// **毎回 schema を作り直す。** golang-migrate の管理表が前回の実行から残ると、
// 起点が狂って別の理由で落ちる。
func newDBFixture(t *testing.T, suffix string) *dbFixture {
	t.Helper()
	db, err := testutil.OpenTestDBSchema(suffix)
	require.NoError(t, err)
	var schema string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&schema).Error)
	// 名前そのものを要求する。search_path が効いていないと current_schema() が
	// public を返し、直後の DROP SCHEMA が共有の public を落とす。
	require.Equal(t, "internal_cli_migrate_"+suffix, schema)
	require.NoError(t, db.Exec(`DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`).Error)
	require.NoError(t, db.Exec(`CREATE SCHEMA "`+schema+`"`).Error)
	root := filepath.Join(t.TempDir(), "migration")
	return &dbFixture{db: db, schema: schema, coreDir: root, localDir: filepath.Join(root, "local")}
}

// migrateURL builds the golang-migrate URL for the fixture's schema.
// 組み立ては internal/repository の migrateURL と同じく net/url に任せる。
func (f *dbFixture) migrateURL() string {
	q := url.Values{}
	q.Set("sslmode", testutil.EnvOrDefault("TEST_DB_SSLMODE", "disable"))
	q.Set("search_path", f.schema)
	u := url.URL{
		Scheme: "pgx5",
		User: url.UserPassword(
			testutil.EnvOrDefault("TEST_DB_USER", "mk"),
			testutil.EnvOrDefault("TEST_DB_PASS", "mk"),
		),
		Host: net.JoinHostPort(
			testutil.EnvOrDefault("TEST_DB_HOST", "localhost"),
			testutil.EnvOrDefault("TEST_DB_PORT", "5432"),
		),
		Path:     "/" + testutil.EnvOrDefault("TEST_DB_NAME", "misskey_test"),
		RawQuery: q.Encode(),
	}
	return u.String()
}

// migrate runs "elythia migrate" with the real golang-migrate against the
// fixture and returns the exit code and log output.
func (f *dbFixture) migrate(t *testing.T, args ...string) (int, string) {
	t.Helper()
	e := defaultEnv()
	var out bytes.Buffer
	e.stdout = &out
	e.setLogger = func(*slog.Logger) {}
	e.coreDir, e.localDir = f.coreDir, f.localDir
	e.databaseURL = func(*config.Config) string { return f.migrateURL() }
	code := run(e, &bytes.Buffer{}, append([]string{"-config", writeConfig(t)}, args...))
	return code, out.String()
}

// versions returns every row of a golang-migrate tracking table.
func (f *dbFixture) versions(t *testing.T, table string) []int64 {
	t.Helper()
	var rows []int64
	require.NoError(t, f.db.Raw(`SELECT version FROM "`+table+`" ORDER BY version`).Scan(&rows).Error)
	return rows
}

func (f *dbFixture) tableExists(t *testing.T, table string) bool {
	t.Helper()
	var n int64
	require.NoError(t, f.db.Raw(`SELECT count(*) FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_name = ?`, table).Scan(&n).Error)
	return n == 1
}

func (f *dbFixture) columnExists(t *testing.T, table, column string) bool {
	t.Helper()
	var n int64
	require.NoError(t, f.db.Raw(`SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = ? AND column_name = ?`, table, column).Scan(&n).Error)
	return n == 1
}

// local の migration は core の後に流れ、core の管理表には記録されない。
// fork の migration は本体の表に列を足すので、core が先でないと当たらない。
func TestMigrateDB_LocalRunsAfterCoreOnItsOwnTable(t *testing.T) {
	f := newDBFixture(t, "tracks_order")
	writeMigration(t, f.coreDir, "000001_core_a",
		`CREATE TABLE core_a (id text PRIMARY KEY);`, `DROP TABLE core_a;`)
	// local も 000001 にして、番号が core と重なっても混ざらないことを見る。
	writeMigration(t, f.localDir, "000001_fork_col",
		`ALTER TABLE core_a ADD COLUMN fork_x text;`, `ALTER TABLE core_a DROP COLUMN fork_x;`)

	code, out := f.migrate(t)
	require.Equal(t, 0, code, out)
	assert.True(t, f.columnExists(t, "core_a", "fork_x"))
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations"))
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations_local"))

	// 2 回目は両方とも変化なしで成功する。
	code, out = f.migrate(t)
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, `msg="no migration changes to apply" track=core`)
	assert.Contains(t, out, `msg="no migration changes to apply" track=local`)
}

// **#3428 の本題。** fork が大きな番号の migration を当てた後に、本体が小さな
// 番号の migration を足しても、本体の側が適用される。管理表を共有すると、
// ディレクトリも共有していれば core の 000002 は 900001 より小さいので黙って
// 飛ばされ、ディレクトリだけ分けていれば `no migration found for version
// 900001` で止まる。このテストはどちらの退行でも落ちる (後者は変異で確認)。
func TestMigrateDB_CoreAddedAfterLocalStillApplies(t *testing.T) {
	f := newDBFixture(t, "tracks_skip")
	writeMigration(t, f.coreDir, "000001_core_a",
		`CREATE TABLE core_a (id text PRIMARY KEY);`, `DROP TABLE core_a;`)
	writeMigration(t, f.localDir, "900001_fork_table",
		`CREATE TABLE fork_t (id text PRIMARY KEY);`, `DROP TABLE fork_t;`)

	code, out := f.migrate(t)
	require.Equal(t, 0, code, out)
	require.True(t, f.tableExists(t, "fork_t"))

	// 本体を取り込んだ = core に新しい migration が増えた。
	writeMigration(t, f.coreDir, "000002_core_b",
		`CREATE TABLE core_b (id text PRIMARY KEY);`, `DROP TABLE core_b;`)
	code, out = f.migrate(t)
	require.Equal(t, 0, code, out)
	assert.True(t, f.tableExists(t, "core_b"), "the new core migration was skipped")
	assert.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))
	assert.Equal(t, []int64{900001}, f.versions(t, "schema_migrations_local"))
}

// local の系列が無い環境では、動作も管理表も今までと変わらない。
func TestMigrateDB_NoLocalTrackLeavesNoTable(t *testing.T) {
	f := newDBFixture(t, "tracks_none")
	writeMigration(t, f.coreDir, "000001_core_a",
		`CREATE TABLE core_a (id text PRIMARY KEY);`, `DROP TABLE core_a;`)

	code, out := f.migrate(t)
	require.Equal(t, 0, code, out)
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations"))
	assert.False(t, f.tableExists(t, "schema_migrations_local"), "absent local dir must not create the local table")

	// 空のディレクトリでも同じ。
	require.NoError(t, os.MkdirAll(f.localDir, 0o755))
	code, out = f.migrate(t)
	require.Equal(t, 0, code, out)
	assert.False(t, f.tableExists(t, "schema_migrations_local"), "empty local dir must not create the local table")
}

// down は -track で指定した系列だけを戻す。
func TestMigrateDB_DownRollsBackOnlyTheNamedTrack(t *testing.T) {
	f := newDBFixture(t, "tracks_down")
	writeMigration(t, f.coreDir, "000001_core_a",
		`CREATE TABLE core_a (id text PRIMARY KEY);`, `DROP TABLE core_a;`)
	writeMigration(t, f.coreDir, "000002_core_b",
		`CREATE TABLE core_b (id text PRIMARY KEY);`, `DROP TABLE core_b;`)
	writeMigration(t, f.localDir, "000001_fork_col",
		`ALTER TABLE core_a ADD COLUMN fork_x text;`, `ALTER TABLE core_a DROP COLUMN fork_x;`)
	code, out := f.migrate(t)
	require.Equal(t, 0, code, out)

	// 系列を指定しない down は DB に触れずに拒否する。
	code, _ = f.migrate(t, "-direction", "down", "-steps", "1")
	require.Equal(t, 2, code)
	assert.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations_local"))

	code, out = f.migrate(t, "-direction", "down", "-track", "local")
	require.Equal(t, 0, code, out)
	assert.False(t, f.columnExists(t, "core_a", "fork_x"), "local down did not run")
	assert.Empty(t, f.versions(t, "schema_migrations_local"))
	assert.True(t, f.tableExists(t, "core_b"), "local down must not touch core")
	assert.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))

	code, out = f.migrate(t, "-direction", "down", "-track", "core", "-steps", "1")
	require.Equal(t, 0, code, out)
	assert.False(t, f.tableExists(t, "core_b"))
	assert.True(t, f.tableExists(t, "core_a"))
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations"))
	assert.Empty(t, f.versions(t, "schema_migrations_local"))
}

// dirtyOf returns the dirty flag of a golang-migrate tracking table.
func (f *dbFixture) dirtyOf(t *testing.T, table string) bool {
	t.Helper()
	var dirty []bool
	require.NoError(t, f.db.Raw(`SELECT dirty FROM "`+table+`"`).Scan(&dirty).Error)
	require.Len(t, dirty, 1)
	return dirty[0]
}

// **#3455 の本題。** migration が途中で落ちて dirty になった管理表を、原因を
// 直した後に -force で戻すと、次の up が続きから流れる。-force は指定した系列の
// 管理表だけを書き換え、migration は流さない。
func TestMigrateDB_ForceRecoversDirtyTrack(t *testing.T) {
	f := newDBFixture(t, "force_dirty")
	writeMigration(t, f.coreDir, "000001_core_a",
		`CREATE TABLE core_a (id text PRIMARY KEY);`, `DROP TABLE core_a;`)
	writeMigration(t, f.coreDir, "000002_core_b",
		`CREATE TABLE core_b (id text PRIMARY KEY); SELECT * FROM no_such_table;`, `DROP TABLE core_b;`)
	writeMigration(t, f.localDir, "000001_fork_col",
		`ALTER TABLE core_a ADD COLUMN fork_x text;`, `ALTER TABLE core_a DROP COLUMN fork_x;`)

	code, out := f.migrate(t)
	require.Equal(t, 1, code, out)
	require.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))
	require.True(t, f.dirtyOf(t, "schema_migrations"))
	// 記録された番号 (当たっていない 000002) をそのまま当たったことにはしない。
	code, out = f.migrate(t, "-force", "2", "-track", "core")
	require.Equal(t, 1, code, out)
	require.True(t, f.dirtyOf(t, "schema_migrations"))
	// dirty のままでは up が先へ進まない。
	code, out = f.migrate(t)
	require.Equal(t, 1, code, out)
	assert.Contains(t, out, "Dirty database version 2")

	// 原因を直す。000002 は 1 文目も含めて取り消されている (1 つの transaction)
	// ので、当たっているのは 000001 まで。
	require.False(t, f.tableExists(t, "core_b"))
	writeMigration(t, f.coreDir, "000002_core_b",
		`CREATE TABLE core_b (id text PRIMARY KEY);`, `DROP TABLE core_b;`)

	code, out = f.migrate(t, "-force", "1", "-track", "core")
	require.Equal(t, 0, code, out)
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations"))
	assert.False(t, f.dirtyOf(t, "schema_migrations"))
	assert.False(t, f.tableExists(t, "core_b"), "-force must not run migrations")
	assert.False(t, f.tableExists(t, "schema_migrations_local"), "-force on core must not touch local")

	code, out = f.migrate(t)
	require.Equal(t, 0, code, out)
	assert.True(t, f.tableExists(t, "core_b"))
	// dirty でない表はもう書き換えない (当たった 000002 を未適用にしない)。
	code, out = f.migrate(t, "-force", "1", "-track", "core")
	require.Equal(t, 1, code, out)
	assert.Contains(t, out, "the tracking table is not dirty")
	assert.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))
	assert.True(t, f.columnExists(t, "core_a", "fork_x"))
	assert.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations_local"))

	// local の -force は local の管理表だけを書き換える。
	// (fork の 000002 の up が落ちた状態を作る)
	require.NoError(t, f.db.Exec(`UPDATE schema_migrations_local SET version = 2, dirty = true`).Error)
	code, out = f.migrate(t, "-force", "1", "-track", "local")
	require.Equal(t, 0, code, out)
	assert.Equal(t, []int64{1}, f.versions(t, "schema_migrations_local"))
	assert.False(t, f.dirtyOf(t, "schema_migrations_local"))
	assert.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))
	assert.False(t, f.dirtyOf(t, "schema_migrations"))
}

// down が途中で落ちると、golang-migrate は失敗したファイルではなく、戻す先
// (1 つ前) の番号を dirty で記録する。失敗した down は取り消されているので、
// 当たっているのは記録の次の番号。docs/deployment.md の手順はこれを前提にする。
func TestMigrateDB_ForceAfterFailedDown(t *testing.T) {
	f := newDBFixture(t, "force_down")
	writeMigration(t, f.coreDir, "000001_core_a",
		`CREATE TABLE core_a (id text PRIMARY KEY);`, `DROP TABLE core_a;`)
	writeMigration(t, f.coreDir, "000002_core_b",
		`CREATE TABLE core_b (id text PRIMARY KEY);`, `DROP TABLE core_b; SELECT * FROM no_such_table;`)
	code, out := f.migrate(t)
	require.Equal(t, 0, code, out)

	code, out = f.migrate(t, "-direction", "down", "-track", "core", "-steps", "1")
	require.Equal(t, 1, code, out)
	require.Equal(t, []int64{1}, f.versions(t, "schema_migrations"))
	require.True(t, f.dirtyOf(t, "schema_migrations"))
	require.True(t, f.tableExists(t, "core_b"), "the failed down must have been rolled back")

	code, out = f.migrate(t, "-force", "2", "-track", "core")
	require.Equal(t, 0, code, out)
	assert.Equal(t, []int64{2}, f.versions(t, "schema_migrations"))
	assert.False(t, f.dirtyOf(t, "schema_migrations"))
	code, out = f.migrate(t)
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "no migration changes to apply")
}
