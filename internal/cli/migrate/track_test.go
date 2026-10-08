package migrate

import (
	"bytes"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/golang-migrate/migrate/v4/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// writeMigration writes an up/down pair named NNNNNN_name into dir.
func writeMigration(t *testing.T, dir, base, up, down string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, base+".up.sql"), []byte(up), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, base+".down.sql"), []byte(down), 0o644))
}

// openedTrack is one call to env.open.
type openedTrack struct {
	src, db string
	m       *fakeMigrator
}

// trackEnv returns an env that hands out a fresh fake per open and records
// which source / database URL each one was opened with. errFor maps a source
// URL to the error its migrator returns.
func trackEnv(t *testing.T, localDir string, errFor map[string]error) (env, *bytes.Buffer, *[]openedTrack) {
	t.Helper()
	e, out, _ := testEnv(t, nil, nil)
	var opened []openedTrack
	e.localDir = localDir
	e.open = func(src, db string) (migrator, error) {
		m := &fakeMigrator{err: errFor[src]}
		opened = append(opened, openedTrack{src: src, db: db, m: m})
		return m, nil
	}
	return e, out, &opened
}

func migrationsTableOf(t *testing.T, databaseURL string) string {
	t.Helper()
	u, err := url.Parse(databaseURL)
	require.NoError(t, err)
	return u.Query().Get("x-migrations-table")
}

func TestRun_LocalTrack(t *testing.T) {
	localDir := filepath.Join(t.TempDir(), "local")
	writeMigration(t, localDir, "000001_fork_column", "SELECT 1;", "SELECT 1;")

	t.Run("up without -track runs core, then local on its own table", func(t *testing.T) {
		e, out, opened := trackEnv(t, localDir, nil)
		require.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
		require.Len(t, *opened, 2)
		core, local := (*opened)[0], (*opened)[1]
		assert.Equal(t, "file://migration", core.src)
		assert.Empty(t, migrationsTableOf(t, core.db), "core must keep golang-migrate's default table")
		assert.Equal(t, "file://"+localDir, local.src)
		assert.Equal(t, "schema_migrations_local", migrationsTableOf(t, local.db))
		// 管理表以外の接続先は core と同じ。
		cu, _ := url.Parse(core.db)
		lu, _ := url.Parse(local.db)
		assert.Equal(t, cu.Host, lu.Host)
		assert.Equal(t, cu.Path, lu.Path)
		assert.Equal(t, cu.User.String(), lu.User.String())
		assert.Equal(t, cu.Query().Get("sslmode"), lu.Query().Get("sslmode"))
		for _, o := range *opened {
			assert.Equal(t, []string{"up"}, o.m.calls)
			assert.True(t, o.m.closed)
		}
		assert.Contains(t, out.String(), "track=core")
		assert.Contains(t, out.String(), "track=local")
	})

	t.Run("-track restricts up to one track", func(t *testing.T) {
		for trk, want := range map[string]string{"core": "file://migration", "local": "file://" + localDir} {
			e, _, opened := trackEnv(t, localDir, nil)
			require.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-track", trk}))
			require.Len(t, *opened, 1, trk)
			assert.Equal(t, want, (*opened)[0].src, trk)
		}
	})

	t.Run("down rolls back only the named track", func(t *testing.T) {
		e, _, opened := trackEnv(t, localDir, nil)
		require.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-direction", "down", "-track", "local", "-steps", "1"}))
		require.Len(t, *opened, 1)
		assert.Equal(t, "file://"+localDir, (*opened)[0].src)
		assert.Equal(t, "schema_migrations_local", migrationsTableOf(t, (*opened)[0].db))
		assert.Equal(t, []string{"steps"}, (*opened)[0].m.calls)
		assert.Equal(t, -1, (*opened)[0].m.steps)
	})

	t.Run("a core failure stops before local", func(t *testing.T) {
		e, out, opened := trackEnv(t, localDir, map[string]error{"file://migration": errors.New("boom")})
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
		require.Len(t, *opened, 1, "local must not run on top of a failed core")
		assert.Contains(t, out.String(), "migration failed")
		assert.Contains(t, out.String(), "track=core")
	})

	t.Run("a local failure is reported against the local track", func(t *testing.T) {
		e, out, opened := trackEnv(t, localDir, map[string]error{"file://" + localDir: errors.New("boom")})
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
		require.Len(t, *opened, 2)
		assert.Contains(t, out.String(), `level=ERROR msg="migration failed" track=local direction=up`)
	})

	t.Run("open error on local", func(t *testing.T) {
		e, out, _ := trackEnv(t, localDir, nil)
		e.open = func(src, _ string) (migrator, error) {
			if src == "file://"+localDir {
				return nil, errors.New("dial")
			}
			return &fakeMigrator{}, nil
		}
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
		assert.Contains(t, out.String(), `msg="failed to create migrator" track=local`)
	})
}

// local の系列が無い環境 (Elythia 本体の利用者) では、何も開かない =
// `schema_migrations_local` も作られない。
func TestRun_NoLocalMigrations(t *testing.T) {
	empty := t.TempDir()
	// golang-migrate が読まない名前だけのディレクトリも「空」と同じ。
	onlyNoise := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(onlyNoise, "README.md"), []byte("x"), 0o644))
	for name, dir := range map[string]string{
		"absent":     filepath.Join(t.TempDir(), "absent"),
		"empty":      empty,
		"only noise": onlyNoise,
	} {
		t.Run(name, func(t *testing.T) {
			e, out, opened := trackEnv(t, dir, nil)
			require.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
			require.Len(t, *opened, 1, "only core may be opened")
			assert.Equal(t, "file://migration", (*opened)[0].src)
			assert.Contains(t, out.String(), "no local migrations; skipping")

			e, _, opened = trackEnv(t, dir, nil)
			require.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-track", "local"}))
			assert.Empty(t, *opened)

			// 戻せと明示されたのに戻せないのは失敗にする (cwd の取り違えに気付けるように)。
			e, out, opened = trackEnv(t, dir, nil)
			assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-direction", "down", "-track", "local", "-steps", "1"}))
			assert.Empty(t, *opened)
			assert.Contains(t, out.String(), "no local migrations to roll back")
		})
	}
}

func TestRun_LocalDirUnreadable(t *testing.T) {
	// ディレクトリの代わりにファイルを置くと ReadDir が ErrNotExist 以外で落ちる。
	notDir := filepath.Join(t.TempDir(), "local")
	require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o644))
	e, out, opened := trackEnv(t, notDir, nil)
	assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-track", "local"}))
	assert.Empty(t, *opened)
	assert.Contains(t, out.String(), "failed to read local migrations")
}

func TestLatestVersion(t *testing.T) {
	dir := t.TempDir()
	writeMigration(t, dir, "000002_a", "", "")
	writeMigration(t, dir, "900010_b", "", "")
	// down だけのもの・番号の無いもの・サブディレクトリは数えない。
	require.NoError(t, os.WriteFile(filepath.Join(dir, "999999_only_down.down.sql"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), nil, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "fork.up.sql"), nil, 0o644))
	writeMigration(t, filepath.Join(dir, "nested"), "999998_nested", "", "")

	latest, n, err := LatestVersion(dir)
	require.NoError(t, err)
	assert.Equal(t, uint(900010), latest)
	assert.Equal(t, 2, n)

	latest, n, err = LatestVersion(filepath.Join(dir, "absent"))
	require.NoError(t, err)
	assert.Zero(t, latest)
	assert.Zero(t, n)
}

func TestWithMigrationsTable(t *testing.T) {
	got, err := withMigrationsTable("pgx5://mk:p%40ss@/misskey?host=%2Fvar%2Frun%2Fpostgresql&port=5432&sslmode=disable", LocalTable)
	require.NoError(t, err)
	u, err := url.Parse(got)
	require.NoError(t, err)
	assert.Equal(t, "schema_migrations_local", u.Query().Get("x-migrations-table"))
	// 既存の query と資格情報を壊さない (UDS は host query で渡している)。
	assert.Equal(t, "/var/run/postgresql", u.Query().Get("host"))
	assert.Equal(t, "5432", u.Query().Get("port"))
	pass, _ := u.User.Password()
	assert.Equal(t, "p@ss", pass)

	_, err = withMigrationsTable("://bad", LocalTable)
	assert.Error(t, err)
}

// **core の系列に local の migration が混ざらないこと。** local は migration/ の
// 下に置くので、golang-migrate の file source がサブディレクトリを読むと、fork の
// 番号が本体の系列に入り込む (管理表を分けた意味が無くなる)。
func TestCoreSourceIgnoresLocalDir(t *testing.T) {
	core := t.TempDir()
	writeMigration(t, core, "000001_core", "SELECT 1;", "SELECT 1;")
	writeMigration(t, filepath.Join(core, "local"), "000001_fork", "SELECT 1;", "SELECT 1;")
	writeMigration(t, filepath.Join(core, "local"), "900001_fork", "SELECT 1;", "SELECT 1;")

	drv, err := source.Open("file://" + core)
	require.NoError(t, err)
	t.Cleanup(func() { _ = drv.Close() })
	first, err := drv.First()
	require.NoError(t, err)
	assert.Equal(t, uint(1), first)
	_, err = drv.Next(first)
	assert.ErrorIs(t, err, os.ErrNotExist, "the core source must see only 000001")
}

// local 用の URL を組めないときも、URL (パスワードを含む) をログに出さない。
func TestRun_LocalURLErrorDoesNotLeakPassword(t *testing.T) {
	localDir := filepath.Join(t.TempDir(), "local")
	writeMigration(t, localDir, "000001_fork", "SELECT 1;", "SELECT 1;")
	e, out, opened := trackEnv(t, localDir, nil)
	e.databaseURL = func(*config.Config) string { return "pgx5://mk:s3cretpw@db.example:5432/%zz" }
	assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-track", "local"}))
	assert.Empty(t, *opened)
	assert.Contains(t, out.String(), "failed to build database URL for the local track")
	assert.NotContains(t, out.String(), "s3cretpw")
}
