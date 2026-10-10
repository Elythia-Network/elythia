package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bkp "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/selfcheck"
)

const rsCoreMigrations = "../../../migration"

func rsDeadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	return port
}

// rsConfig writes a configuration file. extra is appended verbatim.
func rsConfig(t *testing.T, db string, extra string) string {
	t.Helper()
	dead := rsDeadPort(t)
	body := fmt.Sprintf(`url: http://127.0.0.1:%d/
port: 3000
db:
  host: 127.0.0.1
  port: %d
  db: %s
  user: none
  pass: none
redis:
  host: 127.0.0.1
  port: %d
%s`, dead, dead, db, dead, extra)
	path := filepath.Join(t.TempDir(), "default.yml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

const rsBackupSection = `backup:
  storage:
    type: dir
    dir:
      path: /mnt/backup
`

type rsCalls struct {
	restore  *bkp.RestoreOptions
	replace  []string
	cleaned  bool
	checked  []string
	restorer *bkp.Restorer
}

// rsEnv returns an env whose database and Redis steps are recorded fakes.
func rsEnv(calls *rsCalls) (restoreEnv, *bytes.Buffer, *bytes.Buffer) {
	var out, errOut bytes.Buffer
	return restoreEnv{
		stdout: &out, stderr: &errOut,
		openStorage: func(config.BackupStorageOptions) (bkp.Storage, error) { return nil, nil },
		restore: func(_ context.Context, r *bkp.Restorer, o bkp.RestoreOptions) (*bkp.RestoreResult, error) {
			calls.restore, calls.restorer = &o, r
			res := &bkp.RestoreResult{ID: "20261010T030000Z", Mode: o.Mode, Database: o.Database}
			if o.Mode == bkp.RestoreSwap {
				res.BeforeRestore = o.Database + "_before_restore_x"
			}
			return res, nil
		},
		replace: func(_ context.Context, r *bkp.Restorer, m, db, keepAs, repl string) error {
			calls.replace, calls.restorer = []string{m, db, keepAs, repl}, r
			return nil
		},
		cleanRedis: func(context.Context, *config.Config) (bkp.RedisCleanupResult, error) {
			calls.cleaned = true
			return bkp.RedisCleanupResult{Deleted: map[string]int64{"list:*": 3}}, nil
		},
		check: func(_ context.Context, _ *config.Config, core, local string) (selfcheck.Report, error) {
			calls.checked = []string{core, local}
			return selfcheck.Report{OK: true, Results: []selfcheck.Result{{Name: "database", Status: selfcheck.StatusOK}}}, nil
		},
		now: func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) },
	}, &out, &errOut
}

func TestRestoreFlags(t *testing.T) {
	cfg := rsConfig(t, "elythia", rsBackupSection)
	cases := map[string][]string{
		"no confirm":         {"-config", cfg, "-id", "latest", "-mode", "swap"},
		"no id":              {"-config", cfg, "-mode", "swap", "-confirm", "elythia"},
		"no mode":            {"-config", cfg, "-id", "latest", "-confirm", "elythia"},
		"bad mode":           {"-config", cfg, "-id", "latest", "-mode", "copy", "-confirm", "elythia"},
		"rollback with id":   {"-config", cfg, "-rollback", "x", "-id", "latest", "-confirm", "elythia"},
		"rollback with mode": {"-config", cfg, "-rollback", "x", "-mode", "swap", "-confirm", "elythia"},
		"stray argument":     {"-config", cfg, "-id", "latest", "-mode", "swap", "-confirm", "elythia", "now"},
		"unknown flag":       {"-force"},
	}
	for name, args := range cases {
		var calls rsCalls
		e, _, _ := rsEnv(&calls)
		assert.Equal(t, 2, restore(e, args), name)
		assert.Nil(t, calls.restore, name)
	}
	var calls rsCalls
	e, _, _ := rsEnv(&calls)
	assert.Equal(t, 0, restore(e, []string{"-h"}))
}

func TestRestoreRunsTheSteps(t *testing.T) {
	cfg := rsConfig(t, "elythia", rsBackupSection+`  tools:
    pgRestore: /usr/local/bin/pg_restore
`)
	var calls rsCalls
	e, out, errOut := rsEnv(&calls)
	code := restore(e, []string{"-config", cfg, "-id", "latest", "-mode", "swap", "-confirm", "elythia",
		"-maintenance-db", "template1", "-migrations", "mig", "-tmp-dir", "/var/tmp"})
	require.Equal(t, 0, code, errOut.String())
	require.NotNil(t, calls.restore)
	assert.Equal(t, bkp.RestoreOptions{ID: "latest", Mode: bkp.RestoreSwap, Database: "elythia", Confirm: "elythia", MaintenanceDB: "template1"}, *calls.restore)
	r := calls.restorer
	assert.Equal(t, "mig", r.CoreMigrationsDir)
	assert.Equal(t, filepath.Join("mig", "local"), r.LocalMigrationsDir)
	assert.Equal(t, "/var/tmp", r.TempDir)
	assert.Equal(t, "/usr/local/bin/pg_restore", r.PgRestore)
	assert.True(t, calls.cleaned)
	assert.Equal(t, []string{"mig", filepath.Join("mig", "local")}, calls.checked)
	assert.Contains(t, out.String(), "kept as elythia_before_restore_x")
	assert.Contains(t, out.String(), "-rollback elythia_before_restore_x -confirm elythia")
	assert.Contains(t, out.String(), "cleared 3 redis keys")

	// 空の DB へ戻す形では、退避した DB の案内を出さず、既定で Redis を消さない
	// (版上げ・引っ越しでは、戻す DB は止めた時点の DB そのもの)。検査は流す。
	calls = rsCalls{}
	e, out, _ = rsEnv(&calls)
	require.Equal(t, 0, restore(e, []string{"-config", cfg, "-id", "latest", "-mode", "empty", "-confirm", "elythia"}))
	assert.False(t, calls.cleaned)
	assert.NotNil(t, calls.checked)
	assert.Contains(t, out.String(), "kept redis as it is")
	assert.NotContains(t, out.String(), "-rollback")
}

func TestRestoreFailures(t *testing.T) {
	withBackup := rsConfig(t, "elythia", rsBackupSection)
	args := func(cfg string) []string {
		return []string{"-config", cfg, "-id", "latest", "-mode", "swap", "-confirm", "elythia"}
	}
	cases := map[string]struct {
		args   []string
		mutate func(*restoreEnv)
		want   string
	}{
		"config":        {args(filepath.Join(t.TempDir(), "absent.yml")), nil, "load config"},
		"no backup":     {args(rsConfig(t, "elythia", "")), nil, "no backup: section"},
		"storage type":  {args(rsConfig(t, "elythia", "backup:\n  storage:\n    type: ftp\n")), func(e *restoreEnv) { e.openStorage = bkp.OpenStorage }, `unknown storage.type "ftp"`},
		"no marker":     {args(withBackup), func(e *restoreEnv) { e.openStorage = bkp.OpenStorage }, "open storage"},
		"storage error": {args(withBackup), func(e *restoreEnv) { e.openStorage = rsFailStorage }, "open storage"},
		"identity":      {args(rsConfig(t, "elythia", rsBackupSection+"  encryption:\n    identityFile: /nonexistent/key\n")), nil, "identityFile"},
		"restore": {args(withBackup), func(e *restoreEnv) {
			e.restore = func(context.Context, *bkp.Restorer, bkp.RestoreOptions) (*bkp.RestoreResult, error) {
				return nil, bkp.ErrRestoreConnections
			}
		}, "open connections"},
		"redis": {args(withBackup), func(e *restoreEnv) {
			e.cleanRedis = func(context.Context, *config.Config) (bkp.RedisCleanupResult, error) {
				return bkp.RedisCleanupResult{}, errors.New("redis down")
			}
		}, "redis down"},
		"check error": {args(withBackup), func(e *restoreEnv) {
			e.check = func(context.Context, *config.Config, string, string) (selfcheck.Report, error) {
				return selfcheck.Report{}, errors.New("db down")
			}
		}, "db down"},
		"check fails": {args(withBackup), func(e *restoreEnv) {
			e.check = func(context.Context, *config.Config, string, string) (selfcheck.Report, error) {
				return selfcheck.Report{Results: []selfcheck.Result{{Name: "database", Status: selfcheck.StatusFail, Hint: "run migrate"}}}, nil
			}
		}, "failed the checks"},
	}
	for name, c := range cases {
		var calls rsCalls
		e, out, errOut := rsEnv(&calls)
		if c.mutate != nil {
			c.mutate(&e)
		}
		assert.Equal(t, 1, restore(e, c.args), name)
		assert.Contains(t, errOut.String(), c.want, name)
		if name == "check fails" {
			assert.Contains(t, out.String(), "run migrate")
		}
		if name == "redis" {
			assert.Contains(t, errOut.String(), "already switched")
		}
	}
}

func rsFailStorage(config.BackupStorageOptions) (bkp.Storage, error) {
	return nil, errors.New("bucket missing")
}

func TestRestoreRedisFlag(t *testing.T) {
	cfg := rsConfig(t, "elythia", rsBackupSection)
	cases := []struct {
		args  []string
		clean bool
	}{
		{[]string{"-mode", "swap"}, true},
		{[]string{"-mode", "empty"}, false},
		{[]string{"-mode", "swap", "-redis", "auto"}, true},
		{[]string{"-mode", "empty", "-redis", "clean"}, true},
		{[]string{"-mode", "swap", "-redis", "keep"}, false},
	}
	for _, c := range cases {
		var calls rsCalls
		e, _, errOut := rsEnv(&calls)
		args := append([]string{"-config", cfg, "-id", "latest", "-confirm", "elythia"}, c.args...)
		require.Equal(t, 0, restore(e, args), "%v: %s", c.args, errOut.String())
		assert.Equal(t, c.clean, calls.cleaned, "%v", c.args)
		assert.NotNil(t, calls.checked, "%v", c.args)
	}
	var calls rsCalls
	e, _, errOut := rsEnv(&calls)
	assert.Equal(t, 2, restore(e, []string{"-config", cfg, "-id", "latest", "-confirm", "elythia", "-mode", "swap", "-redis", "flush"}))
	assert.Contains(t, errOut.String(), "-redis takes auto, clean or keep")
	assert.Nil(t, calls.restore)
	// ロールバックは swap と同じく既定で消し、keep なら消さない。
	e, _, _ = rsEnv(&calls)
	require.Equal(t, 0, restore(e, []string{"-config", cfg, "-rollback", "elythia_before_restore_1", "-confirm", "elythia", "-redis", "keep"}))
	assert.False(t, calls.cleaned)
}

func TestRestoreRollback(t *testing.T) {
	// ロールバックは保存先を使わないので、backup: の節が無くても動く。
	cfg := rsConfig(t, "elythia", "")
	var calls rsCalls
	e, out, errOut := rsEnv(&calls)
	code := restore(e, []string{"-config", cfg, "-rollback", "elythia_before_restore_20261010030000", "-confirm", "elythia", "-migrations", "mig"})
	require.Equal(t, 0, code, errOut.String())
	assert.Equal(t, []string{"postgres", "elythia", "elythia_rolled_back_20261010120000", "elythia_before_restore_20261010030000"}, calls.replace)
	assert.True(t, calls.cleaned)
	assert.Equal(t, []string{"mig", filepath.Join("mig", "local")}, calls.checked)
	assert.Contains(t, out.String(), "kept as elythia_rolled_back_20261010120000")

	calls = rsCalls{}
	e, _, errOut = rsEnv(&calls)
	assert.Equal(t, 1, restore(e, []string{"-config", cfg, "-rollback", "elythia_before_restore_1", "-confirm", "other"}))
	assert.Contains(t, errOut.String(), "-confirm elythia")
	assert.Nil(t, calls.replace)

	// ロールバックで脇へ置いた DB を戻す (ロールバックの取り消し)。
	calls = rsCalls{}
	e, _, errOut = rsEnv(&calls)
	require.Equal(t, 0, restore(e, []string{"-config", cfg, "-rollback", "elythia_rolled_back_20261010030000", "-confirm", "elythia"}), errOut.String())
	assert.Equal(t, []string{"postgres", "elythia", "elythia_rolled_back_20261010120000", "elythia_rolled_back_20261010030000"}, calls.replace)

	// 退避した DB の形でない名前は受けない。
	calls = rsCalls{}
	for _, name := range []string{"misskey", "other_before_restore_1", "elythia_restore_1", "other_rolled_back_1"} {
		e, _, errOut = rsEnv(&calls)
		assert.Equal(t, 1, restore(e, []string{"-config", cfg, "-rollback", name, "-confirm", "elythia"}), name)
		assert.Contains(t, errOut.String(), "_before_restore_<time>", name)
		assert.Nil(t, calls.replace, name)
	}

	e, _, errOut = rsEnv(&calls)
	e.replace = func(context.Context, *bkp.Restorer, string, string, string, string) error {
		return bkp.ErrRestoreConnections
	}
	assert.Equal(t, 1, restore(e, []string{"-config", cfg, "-rollback", "elythia_before_restore_1", "-confirm", "elythia"}))
	assert.Contains(t, errOut.String(), "open connections")
}

func TestConnFor(t *testing.T) {
	cfg := &config.Config{DB: config.DBOptions{Host: "db.internal", Port: 5432, DB: "elythia", User: "app", Pass: "p@ss word"}}
	c := connFor(cfg)
	assert.Contains(t, c.SQL("other"), "dbname=other")
	assert.Contains(t, c.Migrate("other"), "pgx5://app:p%40ss%20word@db.internal:5432/other")
	tool, env := c.Tool("other")
	assert.Equal(t, "postgresql://app@db.internal:5432/other?sslmode=disable", tool)
	assert.Equal(t, []string{"PGPASSWORD=p@ss word"}, env)
	assert.Equal(t, "elythia", cfg.DB.DB, "connFor must not change the config")

	uds := &config.Config{DB: config.DBOptions{Host: "/var/run/postgresql", Port: 5432, DB: "elythia", User: "app"}}
	tool, env = connFor(uds).Tool("elythia")
	assert.True(t, strings.HasPrefix(tool, "postgresql://app@/elythia?"), tool)
	assert.Contains(t, tool, "host=%2Fvar%2Frun%2Fpostgresql")
	assert.Equal(t, []string{"PGPASSWORD="}, env)

	tool, env = toolConn("postgresql://host/db")
	assert.Equal(t, "postgresql://host/db", tool)
	assert.Nil(t, env)
	tool, env = toolConn("postgresql://u@host/db")
	assert.Equal(t, "postgresql://u@host/db", tool)
	assert.Nil(t, env)
	tool, _ = toolConn("::bad")
	assert.Equal(t, "::bad", tool)
}

func TestRestoreReadsIdentityFile(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	dir := t.TempDir()
	good := filepath.Join(dir, "key.txt")
	require.NoError(t, os.WriteFile(good, []byte("# created: now\n"+id.String()+"\n"), 0o600))
	bad := filepath.Join(dir, "bad.txt")
	require.NoError(t, os.WriteFile(bad, []byte("not a key\n"), 0o600))

	// 設定に書いた鍵が、Restorer に渡る。
	cfg := rsConfig(t, "elythia", rsBackupSection+"  encryption:\n    identityFile: "+good+"\n")
	var calls rsCalls
	e, _, errOut := rsEnv(&calls)
	require.Equal(t, 0, restore(e, []string{"-config", cfg, "-id", "latest", "-mode", "swap", "-confirm", "elythia"}), errOut.String())
	assert.Len(t, calls.restorer.Identities, 1)

	calls = rsCalls{}
	cfg = rsConfig(t, "elythia", rsBackupSection+"  encryption:\n    identityFile: "+bad+"\n")
	e, _, errOut = rsEnv(&calls)
	assert.Equal(t, 1, restore(e, []string{"-config", cfg, "-id", "latest", "-mode", "swap", "-confirm", "elythia"}))
	assert.Contains(t, errOut.String(), "parse identity file")
	assert.Nil(t, calls.restore)
}

func TestDefaultsReachUnreachableServices(t *testing.T) {
	path := rsConfig(t, "elythia", "")
	cfg, err := config.Load(path)
	require.NoError(t, err)
	ctx := context.Background()
	_, err = cleanRedis(ctx, cfg)
	assert.Error(t, err)
	_, err = checkRestored(ctx, cfg, rsCoreMigrations, filepath.Join(rsCoreMigrations, "local"))
	assert.Error(t, err)

	e := defaultRestoreEnv()
	// 保存先は (1) #3458 の実装で開く。
	assert.Equal(t, reflect.ValueOf(bkp.OpenStorage).Pointer(), reflect.ValueOf(e.openStorage).Pointer())
	assert.NotNil(t, e.restore)
	assert.NotNil(t, e.replace)
	assert.NotNil(t, e.now)
}
