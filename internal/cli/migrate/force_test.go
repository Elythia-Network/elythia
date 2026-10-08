package migrate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gomigrate "github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// forceEnv returns an env whose two tracks live in temp dirs: core has
// 000001 and 000002, local has 900001.
func forceEnv(t *testing.T, m *fakeMigrator) (env, *bytes.Buffer, *[]string) {
	t.Helper()
	e, out, opened := testEnv(t, m, nil)
	root := filepath.Join(t.TempDir(), "migration")
	e.coreDir, e.localDir = root, filepath.Join(root, "local")
	writeMigration(t, e.coreDir, "000001_a", "SELECT 1;", "SELECT 1;")
	writeMigration(t, e.coreDir, "000002_b", "SELECT 1;", "SELECT 1;")
	writeMigration(t, e.localDir, "900001_fork", "SELECT 1;", "SELECT 1;")
	return e, out, opened
}

func TestRun_ForceRejectsFlagsBeforeConnecting(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no track", []string{"-force", "1"}, "-force requires -track core or -track local"},
		{"unknown track", []string{"-force", "1", "-track", "fork"}, "-force requires -track core or -track local"},
		// 既定値と同じ値を明示しても拒否する (書かれたこと自体が意図の取り違え)。
		{"with -direction", []string{"-force", "1", "-track", "core", "-direction", "up"}, "-force cannot be combined with -direction or -steps"},
		{"with -steps", []string{"-force", "1", "-track", "core", "-steps", "0"}, "-force cannot be combined with -direction or -steps"},
		{"not a number", []string{"-force", "abc", "-track", "core"}, `invalid -force "abc"`},
		{"nil version", []string{"-force", "-1", "-track", "core"}, `invalid -force "-1"`},
		{"empty", []string{"-force", "", "-track", "core"}, `invalid -force ""`},
		{"overflows int", []string{"-force", "18446744073709551615", "-track", "core"}, `invalid -force "18446744073709551615"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &fakeMigrator{}
			e, _, opened := forceEnv(t, m)
			var flagOut bytes.Buffer
			assert.Equal(t, 2, run(e, &flagOut, append([]string{"-config", writeConfig(t)}, tt.args...)))
			assert.Contains(t, flagOut.String(), tt.want)
			assert.Empty(t, *opened, "must not connect")
			assert.Empty(t, m.calls)
		})
	}
}

// 同梱に無い番号は、DB に繋ぐ前に拒否する。打ち間違えた番号を書くと、間の
// migration が飛ばされるか、当たったものが流れ直す (#3453)。
func TestRun_ForceRejectsVersionNotInTrack(t *testing.T) {
	tests := []struct {
		name, track, version string
	}{
		{"core lacks the version", "core", "3"},
		// 番号は系列ごとに見る。fork の番号を本体の管理表に書かせない (逆も)。
		{"local version on core", "core", "900001"},
		{"core version on local", "local", "2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &fakeMigrator{}
			e, out, opened := forceEnv(t, m)
			assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", tt.version, "-track", tt.track}))
			assert.Contains(t, out.String(), "no such migration in this track; refusing to force")
			assert.Empty(t, *opened, "must not connect")
		})
	}

	t.Run("down-only file does not count", func(t *testing.T) {
		m := &fakeMigrator{}
		e, out, opened := forceEnv(t, m)
		require.NoError(t, os.WriteFile(filepath.Join(e.coreDir, "000003_c.down.sql"), nil, 0o644))
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "3", "-track", "core"}))
		assert.Contains(t, out.String(), "no such migration in this track")
		assert.Empty(t, *opened)
	})

	t.Run("unreadable dir", func(t *testing.T) {
		m := &fakeMigrator{}
		e, out, opened := forceEnv(t, m)
		notDir := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(notDir, []byte("x"), 0o644))
		e.localDir = notDir
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "900001", "-track", "local"}))
		assert.Contains(t, out.String(), "failed to read migrations")
		assert.Empty(t, *opened)
	})
}

func TestRun_ForceSetsOnlyTheNamedTrack(t *testing.T) {
	t.Run("core", func(t *testing.T) {
		m := &fakeMigrator{version: 2, dirty: true}
		e, out, opened := forceEnv(t, m)
		require.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "1", "-track", "core"}))
		require.Len(t, *opened, 2)
		assert.Equal(t, "file://"+e.coreDir, (*opened)[0])
		assert.Empty(t, migrationsTableOf(t, (*opened)[1]), "core must use golang-migrate's default table")
		// migration は流さない。
		assert.Equal(t, []string{"force"}, m.calls)
		assert.Equal(t, 1, m.forced)
		assert.True(t, m.closed)
		assert.Contains(t, out.String(), `msg="tracking table before force" track=core version=2 dirty=true`)
		assert.Contains(t, out.String(), `msg="forced the tracking table" track=core version=1 dirty=false`)
	})

	t.Run("local", func(t *testing.T) {
		m := &fakeMigrator{version: 900002, dirty: true}
		e, out, opened := forceEnv(t, m)
		require.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "900001", "-track", "local"}))
		require.Len(t, *opened, 2)
		assert.Equal(t, "file://"+e.localDir, (*opened)[0])
		assert.Equal(t, "schema_migrations_local", migrationsTableOf(t, (*opened)[1]))
		assert.Equal(t, []string{"force"}, m.calls)
		assert.Equal(t, 900001, m.forced)
		assert.Contains(t, out.String(), `msg="tracking table before force" track=local version=900002 dirty=true`)
	})
}

// dirty でない管理表と空の管理表は書き換えない。流れている migrate と並べて
// 打つと、その完了を待ってから当たった版を巻き戻すことになる。
func TestRun_ForceRefusesTableThatIsNotDirty(t *testing.T) {
	t.Run("clean", func(t *testing.T) {
		m := &fakeMigrator{version: 2}
		e, out, _ := forceEnv(t, m)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "1", "-track", "core"}))
		assert.Contains(t, out.String(), `msg="the tracking table is not dirty; refusing to force (-force only recovers a dirty table)" track=core version=2`)
		assert.Empty(t, m.calls)
		assert.True(t, m.closed)
	})

	t.Run("no version", func(t *testing.T) {
		m := &fakeMigrator{versionErr: gomigrate.ErrNilVersion}
		e, out, _ := forceEnv(t, m)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "900001", "-track", "local"}))
		assert.Contains(t, out.String(), "the tracking table is empty, or -1 after a failed down of the first migration; refusing to force")
		assert.Empty(t, m.calls)
	})

	// 記録された番号そのものへの -force は、up の失敗では当たっていない
	// migration を飛ばし、down の失敗では当たっている migration を流し直させる。
	t.Run("the recorded version itself", func(t *testing.T) {
		m := &fakeMigrator{version: 2, dirty: true}
		e, out, _ := forceEnv(t, m)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "2", "-track", "core"}))
		assert.Contains(t, out.String(), "refusing to force the recorded version itself")
		assert.Empty(t, m.calls)
	})
}

func TestRun_ForceErrors(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		e, out, _ := forceEnv(t, nil)
		e.open = func(string, string) (migrator, error) { return nil, errors.New("dial") }
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "1", "-track", "core"}))
		assert.Contains(t, out.String(), "failed to create migrator")
	})

	t.Run("reading the current version", func(t *testing.T) {
		m := &fakeMigrator{versionErr: errors.New("boom")}
		e, out, _ := forceEnv(t, m)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "1", "-track", "core"}))
		assert.Contains(t, out.String(), "failed to read the current version")
		assert.Empty(t, m.calls, "must not force without logging the value it overwrites")
	})

	t.Run("force", func(t *testing.T) {
		m := &fakeMigrator{err: errors.New("locked"), version: 2, dirty: true}
		e, out, _ := forceEnv(t, m)
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "1", "-track", "core"}))
		assert.Contains(t, out.String(), "force failed")
		assert.NotContains(t, out.String(), "forced the tracking table")
	})

	t.Run("local URL does not leak the password", func(t *testing.T) {
		e, out, opened := forceEnv(t, &fakeMigrator{})
		e.databaseURL = func(_ *config.Config) string { return "pgx5://mk:s3cretpw@db.example:5432/%zz" }
		assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "900001", "-track", "local"}))
		assert.Empty(t, *opened)
		assert.Contains(t, out.String(), "failed to build database URL for the local track")
		assert.NotContains(t, out.String(), "s3cretpw")
	})
}
