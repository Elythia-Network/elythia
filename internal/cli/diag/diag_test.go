package diag

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/fsck"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/elythia-network/elythia/internal/testutil"
)

// unreachablePort is a TCP port nothing listens on, so dialing it fails fast.
//
// 実際に listen して閉じたポートを使う。固定の 1 番などは環境によって
// フィルタされ、拒否ではなくタイムアウトになることがある。
func unreachablePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// writeConfig writes a minimal configuration file and returns its path.
func writeConfig(t *testing.T, port int, extra string) string {
	t.Helper()
	dead := unreachablePort(t)
	body := fmt.Sprintf(`url: http://127.0.0.1:%d/
port: %d
db:
  host: 127.0.0.1
  port: %d
  db: none
  user: none
  pass: none
%s`, dead, port, dead, extra)
	path := filepath.Join(t.TempDir(), "default.yml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// testEnv returns an env that writes to buffers and never touches globals.
func testEnv() (env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	e := defaultEnv()
	e.stdout = &stdout
	e.stderr = &stderr
	e.silenceRedis = func() {}
	return e, &stdout, &stderr
}

func TestDefaultEnv_UsesProcessStreamsAndRealDB(t *testing.T) {
	e := defaultEnv()
	assert.Equal(t, os.Stdout, e.stdout)
	assert.Equal(t, os.Stderr, e.stderr)
	assert.NotNil(t, e.openDB)
	assert.NotNil(t, e.closeDB)
	assert.NotNil(t, e.silenceRedis)
	assert.Equal(t, "migration", e.migrationsDir)
}

func TestSubcommands_FlagHandling(t *testing.T) {
	runners := map[string]func(env, []string) int{
		"config-dump": configDump,
		"healthcheck": healthcheck,
		"doctor":      doctor,
		"fsck":        runFsck,
	}
	for name, run := range runners {
		t.Run(name+" help", func(t *testing.T) {
			e, _, stderr := testEnv()
			assert.Equal(t, 0, run(e, []string{"-h"}))
			assert.Contains(t, stderr.String(), "Usage: elythia "+name)
			assert.Contains(t, stderr.String(), "-config")
		})
		t.Run(name+" bad flag", func(t *testing.T) {
			e, _, _ := testEnv()
			assert.Equal(t, 2, run(e, []string{"-nope"}))
		})
		t.Run(name+" missing config", func(t *testing.T) {
			e, _, stderr := testEnv()
			assert.Equal(t, 1, run(e, []string{"-config", filepath.Join(t.TempDir(), "absent.yml")}))
			assert.Contains(t, stderr.String(), name+": ")
		})
	}
}

// 公開している関数が実際に既定の env で動くことを、副作用の無い経路で確かめる。
func TestExportedEntryPoints_ReturnCodes(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.yml")
	assert.Equal(t, 1, ConfigDump([]string{"-config", missing}))
	assert.Equal(t, 1, Healthcheck([]string{"-config", missing}))
	assert.Equal(t, 1, Doctor([]string{"-config", missing}))
	assert.Equal(t, 1, Fsck([]string{"-config", missing}))
}

func TestFsck_HasFixFlag(t *testing.T) {
	e, _, stderr := testEnv()
	assert.Equal(t, 0, runFsck(e, []string{"-h"}))
	assert.Contains(t, stderr.String(), "-fix")
}

func TestConfigDump_PrintsResolvedConfig(t *testing.T) {
	e, stdout, stderr := testEnv()
	path := writeConfig(t, 4321, "")
	assert.Equal(t, 0, configDump(e, []string{"-config", path}))
	assert.Contains(t, stdout.String(), "4321")
	assert.Empty(t, stderr.String())
}

func TestConfigDump_RoleConflictStillDumps(t *testing.T) {
	t.Setenv(config.EnvOnlyServer, "1")
	t.Setenv(config.EnvOnlyQueue, "1")
	e, stdout, stderr := testEnv()
	path := writeConfig(t, 4321, "")
	assert.Equal(t, 0, configDump(e, []string{"-config", path}))
	assert.Contains(t, stderr.String(), "config-dump: ")
	assert.NotEmpty(t, stdout.String())
}

func TestHealthcheck(t *testing.T) {
	serve := func(t *testing.T, status int) int {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/healthz", r.URL.Path)
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
		require.NoError(t, err)
		port, err := strconv.Atoi(portStr)
		require.NoError(t, err)
		return port
	}

	t.Run("ok", func(t *testing.T) {
		e, _, stderr := testEnv()
		path := writeConfig(t, serve(t, http.StatusOK), "")
		assert.Equal(t, 0, healthcheck(e, []string{"-config", path}))
		assert.Empty(t, stderr.String())
	})
	t.Run("non-200", func(t *testing.T) {
		e, _, stderr := testEnv()
		path := writeConfig(t, serve(t, http.StatusServiceUnavailable), "")
		assert.Equal(t, 1, healthcheck(e, []string{"-config", path}))
		assert.Contains(t, stderr.String(), "healthcheck: status 503")
	})
	t.Run("nothing listening", func(t *testing.T) {
		e, _, stderr := testEnv()
		path := writeConfig(t, unreachablePort(t), "")
		assert.Equal(t, 1, healthcheck(e, []string{"-config", path}))
		assert.Contains(t, stderr.String(), "healthcheck: ")
	})
}

func TestDoctor_UnreachableDependenciesFail(t *testing.T) {
	e, stdout, _ := testEnv()
	path := writeConfig(t, 3000, "")
	assert.Equal(t, 1, doctor(e, []string{"-config", path}))
	assert.Contains(t, stdout.String(), "FAIL")
}

func TestDoctor_WithDatabaseAndRedis(t *testing.T) {
	e, stdout, _ := testEnv()
	db := testutil.MustOpenTestDB()
	e.openDB = func(*config.Config) (*gorm.DB, error) { return db, nil }
	closed := false
	e.closeDB = func(got *gorm.DB) {
		closed = true
		closeDB(got)
	}
	// Redis は繋がらない宛先にする。接続を作る枝を通すことが目的で、
	// 検査そのものは FAIL になる。
	path := writeConfig(t, 3000, fmt.Sprintf("redis:\n  host: 127.0.0.1\n  port: %d\n", unreachablePort(t)))
	assert.Equal(t, 1, doctor(e, []string{"-config", path}))
	assert.True(t, closed, "doctor must close the DB it opened")
	assert.Contains(t, stdout.String(), "FAIL")
}

func TestCountMigrations(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"000001_a.up.sql", "000001_a.down.sql", "000002_b.up.sql"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), nil, 0o600))
	}
	assert.Equal(t, 2, countMigrations(dir))
	assert.Equal(t, 0, countMigrations(filepath.Join(dir, "absent")))
	// 不正なパターンは Glob がエラーを返す。数えられないときは 0 に倒す。
	assert.Equal(t, 0, countMigrations("["))
}

func TestStatusMark(t *testing.T) {
	assert.Equal(t, "ok  ", statusMark(selfcheck.StatusOK))
	assert.Equal(t, "warn", statusMark(selfcheck.StatusWarn))
	assert.Equal(t, "FAIL", statusMark(selfcheck.StatusFail))
	assert.Equal(t, "skip", statusMark(selfcheck.StatusSkip))
}

func TestPrintReport(t *testing.T) {
	var ok bytes.Buffer
	printReport(&ok, selfcheck.Report{OK: true, Results: []selfcheck.Result{
		{Name: "db", Status: selfcheck.StatusOK, Detail: "fine", Hint: "never shown"},
	}})
	assert.Contains(t, ok.String(), "問題は見つかりませんでした")
	assert.NotContains(t, ok.String(), "never shown")

	var bad bytes.Buffer
	printReport(&bad, selfcheck.Report{OK: false, Results: []selfcheck.Result{
		{Name: "redis", Status: selfcheck.StatusWarn, Detail: "slow", Hint: "check redis"},
		{Name: "db", Status: selfcheck.StatusFail, Detail: "down", Hint: "start postgres"},
	}})
	assert.Contains(t, bad.String(), "check redis")
	assert.Contains(t, bad.String(), "start postgres")
	assert.Contains(t, bad.String(), "FAIL の項目があります")
}

func TestFsck_CleanDatabase(t *testing.T) {
	e, stdout, _ := testEnv()
	db := testutil.MustOpenTestDB()
	testutil.ApplyMigrations(db)
	e.openDB = func(*config.Config) (*gorm.DB, error) { return db, nil }
	path := writeConfig(t, 3000, "")
	assert.Equal(t, 0, runFsck(e, []string{"-config", path}))
	assert.Contains(t, stdout.String(), "カウンタのずれは見つかりませんでした")
}

func TestFsck_OpenFailure(t *testing.T) {
	e, _, stderr := testEnv()
	path := writeConfig(t, 3000, "")
	// 既定の openDB で、誰も listen していない DB へ繋ぎに行く。
	assert.Equal(t, 1, runFsck(e, []string{"-config", path}))
	assert.Contains(t, stderr.String(), "fsck: DB に接続できない")
}

func TestFsck_RunFailure(t *testing.T) {
	e, _, stderr := testEnv()
	path := writeConfig(t, 3000, "")
	e.openDB = func(cfg *config.Config) (*gorm.DB, error) {
		// ping を省いて開くと、最初のクエリで初めて失敗する。
		return gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
			DisableAutomaticPing: true,
			Logger:               gormlogger.Default.LogMode(gormlogger.Silent),
		})
	}
	assert.Equal(t, 1, runFsck(e, []string{"-config", path}))
	assert.Contains(t, stderr.String(), "fsck: ")
	assert.NotContains(t, stderr.String(), "DB に接続できない")
}

func TestFsckExitCode(t *testing.T) {
	drift := []fsck.Drift{{Table: "user", Column: "notesCount"}}
	orphan := []fsck.Orphan{{Table: "note", Reason: "missing user", Count: 1}}
	tests := []struct {
		name string
		r    fsck.Report
		fix  bool
		want int
	}{
		{name: "clean", r: fsck.Report{}, want: 0},
		{name: "drift without fix", r: fsck.Report{Drifts: drift}, want: 1},
		{name: "drift fixed", r: fsck.Report{Drifts: drift, Repaired: 1}, fix: true, want: 0},
		{name: "orphan remains after fix", r: fsck.Report{Orphans: orphan}, fix: true, want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, fsckExitCode(tt.r, tt.fix))
		})
	}
}

func TestPrintFsckReport(t *testing.T) {
	var drifts []fsck.Drift
	for i := range 7 {
		drifts = append(drifts, fsck.Drift{Table: "user", Column: "notesCount", ID: strconv.Itoa(i), Stored: 1, Actual: 2})
	}
	r := fsck.Report{Drifts: drifts, Orphans: []fsck.Orphan{{Table: "note", Reason: "missing user", Count: 3}}}

	var dry bytes.Buffer
	printFsckReport(&dry, r, false)
	assert.Contains(t, dry.String(), "カウンタのずれ: 7 件")
	assert.Contains(t, dry.String(), "... 他 2 件")
	assert.Contains(t, dry.String(), "孤児行")
	assert.Contains(t, dry.String(), "-fix を付けて再実行")

	r.Repaired = 7
	var fixed bytes.Buffer
	printFsckReport(&fixed, r, true)
	assert.Contains(t, fixed.String(), "7 件のカウンタを修正しました")
	assert.NotContains(t, fixed.String(), "-fix を付けて再実行")
}

func TestCloseDB_ToleratesBrokenHandle(t *testing.T) {
	// ConnPool を持たない gorm.DB では db.DB() がエラーを返す。閉じる側は
	// 何もせずに戻ること。
	assert.NotPanics(t, func() { closeDB(&gorm.DB{Config: &gorm.Config{}}) })
}

// -fix の有無が fsck に渡り、報告と終了コードがそれに従うこと。
func TestFsck_FixFlagReachesTheCheck(t *testing.T) {
	drift := fsck.Drift{Table: "user", Column: "notesCount", ID: "u1", Stored: 9, Actual: 2}
	for _, tt := range []struct {
		name     string
		args     []string
		wantFix  bool
		wantCode int
		wantOut  string
		notOut   string
	}{
		{name: "read-only by default", wantFix: false, wantCode: 1, wantOut: "-fix を付けて再実行", notOut: "修正しました"},
		{name: "-fix writes back", args: []string{"-fix"}, wantFix: true, wantCode: 0, wantOut: "1 件のカウンタを修正しました", notOut: "-fix を付けて再実行"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, stdout, _ := testEnv()
			e.openDB = func(*config.Config) (*gorm.DB, error) { return &gorm.DB{}, nil }
			e.closeDB = func(*gorm.DB) {}
			var got []fsck.Options
			e.runFsck = func(_ context.Context, _ *gorm.DB, opts fsck.Options) (fsck.Report, error) {
				got = append(got, opts)
				r := fsck.Report{Drifts: []fsck.Drift{drift}}
				if opts.Fix {
					r.Repaired = 1
				}
				return r, nil
			}
			args := append([]string{"-config", writeConfig(t, 3000, "")}, tt.args...)
			assert.Equal(t, tt.wantCode, runFsck(e, args))
			require.Len(t, got, 1)
			assert.Equal(t, tt.wantFix, got[0].Fix)
			assert.Contains(t, stdout.String(), tt.wantOut)
			assert.NotContains(t, stdout.String(), tt.notOut)
		})
	}
}
