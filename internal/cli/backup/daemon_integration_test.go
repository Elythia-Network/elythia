package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/daemon"
	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
)

// daemonSandboxPort is the port of verify's throwaway server inside the
// container.
const daemonSandboxPort = "5433"

// containerSandbox starts verify's throwaway server inside the test
// container and lets this process reach it through the mapped port.
//
// ホストの pg_* は 15 で 18 のサーバーを扱えないので、postgres:18-alpine の中で
// 動かす。プログラムは本番と同じ LocalSandbox.Run に、docker exec で中へ入る
// Runner を渡して動かす (initdb は root では動かないので postgres で)。
type containerSandbox struct {
	run        backup.LocalSandbox
	host       string
	mappedPort int
	// removeErr makes RemoveAll fail after removing the directory, to imitate
	// a cleanup that fails once the verdict is in.
	removeErr error
}

func (s containerSandbox) Run(ctx context.Context, cmd backup.SandboxCmd) error {
	return s.run.Run(ctx, cmd)
}

func (s containerSandbox) sh(ctx context.Context, script string, args ...string) (string, error) {
	var out bytes.Buffer
	err := s.Run(ctx, backup.SandboxCmd{Program: "sh", Args: append([]string{"-c", script, "sh"}, args...), Stdout: &out})
	return out.String(), err
}

func (s containerSandbox) MkdirTemp(ctx context.Context) (string, error) {
	// このプロセスから TCP で繋ぐので、hba は全ての接続を trust にする。待ち受けは
	// container の中だけで、ホストへは testcontainers の割り当てた port だけが出る。
	out, err := s.sh(ctx, `d=$(mktemp -d /tmp/verify-XXXXXX) && mkdir "$d/sock" && printf 'local all all trust\nhost all all all trust\n' >"$d/pg_hba.conf" && printf %s "$d"`)
	return strings.TrimSpace(out), err
}

func (s containerSandbox) RemoveAll(ctx context.Context, dir string) error {
	if _, err := s.sh(ctx, `rm -rf "$1"`, dir); err != nil {
		return err
	}
	return s.removeErr
}

func (s containerSandbox) Server(dir string) backup.SandboxServer {
	local := backup.LocalSandbox{}.Server(dir)
	opts := []string{"-c", "listen_addresses=0.0.0.0", "-p", daemonSandboxPort, "-c", "hba_file=" + dir + "/pg_hba.conf"}
	opts = append(opts, local.Options[2:]...)
	p, _ := strconv.Atoi(daemonSandboxPort)
	return backup.SandboxServer{Options: opts, ToolHost: local.ToolHost, ToolPort: p, Host: s.host, Port: s.mappedPort}
}

var (
	daemonPgOnce sync.Once
	daemonPgC    *tcpostgres.PostgresContainer
	daemonPgErr  error
)

// startDaemonDB starts, once per test binary, a PostgreSQL 18 container whose
// database "elythia" has Elythia's schema, as verify's usable stage requires.
func startDaemonDB(t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	daemonPgOnce.Do(func() { daemonPgC, daemonPgErr = runDaemonDB() })
	require.NoError(t, daemonPgErr)
	return daemonPgC
}

func runDaemonDB() (*tcpostgres.PostgresContainer, error) {
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase("elythia"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithExposedPorts(daemonSandboxPort+"/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		return c, err
	}
	core, _, err := migrate.LatestVersion(filepath.Join("..", "..", "..", migrate.CoreDir))
	if err != nil || core == 0 {
		return c, fmt.Errorf("bundled migrations: %d, %v", core, err)
	}
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return c, err
	}
	db, err := gorm.Open(postgres.Open(url), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return c, err
	}
	defer func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}()
	testutil.ApplyMigrations(db)
	for _, q := range []string{
		// golang-migrate が当てた後と同じ 1 行にする。
		`CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`DELETE FROM schema_migrations`,
		fmt.Sprintf(`INSERT INTO schema_migrations (version, dirty) VALUES (%d, false)`, core),
		`CREATE TABLE daemon_fixture (id int PRIMARY KEY)`,
		`INSERT INTO daemon_fixture SELECT generate_series(1, 5)`,
	} {
		if err := db.Exec(q).Error; err != nil {
			return c, fmt.Errorf("%s: %w", q, err)
		}
	}
	return c, nil
}

// daemonRig is the real storage, taker and verifier wiring of "backup daemon"
// against the daemon test database.
type daemonRig struct {
	c       *tcpostgres.PostgresContainer
	cfgPath string
	cfg     *config.Config
	dir     string
	st      backup.Storage
	te      env
	ve      verifyEnv
}

func newDaemonRig(t *testing.T, removeErr error) *daemonRig {
	t.Helper()
	c := startDaemonDB(t)
	ctx := context.Background()
	r := &daemonRig{c: c, dir: markedDir(t)}
	r.cfgPath = writeConfig(t, c, dirBackupYAML(r.dir, "  schedule:\n    interval: 24h\n    keep: 1\n    verify: true\n"))
	var err error
	r.te, _, _ = testEnv(c)
	r.ve = defaultVerifyEnv()
	r.ve.coreDir = filepath.Join("..", "..", "..", migrate.CoreDir)
	r.ve.localDir = filepath.Join("..", "..", "..", migrate.LocalDir)
	host, err := c.Host(ctx)
	require.NoError(t, err)
	mapped, err := c.MappedPort(ctx, daemonSandboxPort+"/tcp")
	require.NoError(t, err)
	r.ve.newSandbox = func(b *config.BackupOptions) backup.Sandbox {
		return containerSandbox{
			run:        backup.LocalSandbox{Tools: b.Tools, Runner: dockerExecRunner{container: c.GetContainerID(), user: "postgres"}},
			host:       host,
			mappedPort: int(mapped.Num()),
			removeErr:  removeErr,
		}
	}
	r.cfg, err = config.Load(r.cfgPath)
	require.NoError(t, err)
	r.st, err = backup.OpenStorage(r.cfg.Backup.Storage)
	require.NoError(t, err)
	return r
}

// take stores a generation made age ago.
func (r *daemonRig) take(t *testing.T, age time.Duration) *backup.Meta {
	t.Helper()
	opts, err := takeOptions(r.te, r.cfg, r.st, discardLogger())
	require.NoError(t, err)
	opts.Now = func() time.Time { return time.Now().Add(-age) }
	m, err := backup.Take(context.Background(), opts)
	require.NoError(t, err)
	return m
}

// runUntil runs the daemon until done reports true, then stops it and
// returns its log.
func (r *daemonRig) runUntil(t *testing.T, done func([]backup.Generation) bool) ([]backup.Generation, string) {
	t.Helper()
	ctx := context.Background()
	var stderr syncBuffer
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	e := daemonEnv{
		stderr:        &stderr,
		loadConfig:    config.Load,
		openDeps:      wireDeps(r.te, r.ve),
		signalContext: func() (context.Context, context.CancelFunc) { return runCtx, cancel },
		listen:        func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) },
		location:      time.UTC,
	}
	code := make(chan int, 1)
	go func() { code <- runDaemon(e, []string{"-config", r.cfgPath}) }()
	var gens []backup.Generation
	require.Eventually(t, func() bool {
		var err error
		gens, err = backup.ListGenerations(ctx, r.st)
		return err == nil && done(gens)
	}, 3*time.Minute, 200*time.Millisecond, "log:\n%s", &stderr)
	cancel()
	require.Equal(t, 0, <-code, stderr.String())
	return gens, stderr.String()
}

// The daemon takes a backup with backup.Take into a directory storage,
// verifies it with backup.Verify in a throwaway server, and prunes the old
// generation, all through the wiring "backup daemon" uses.
func TestDaemonTakesVerifiesAndPrunesWithRealImplementations(t *testing.T) {
	r := newDaemonRig(t, nil)
	ctx := context.Background()
	// 2 日前の世代を 1 つ置く。起動したとき、最新の世代が間隔より古いので、すぐに取る。
	old := r.take(t, 48*time.Hour)
	gens, log := r.runUntil(t, func(gens []backup.Generation) bool {
		return len(gens) == 1 && gens[0].ID != old.ID && gens[0].Verify != nil
	})

	g := gens[0]
	assert.True(t, g.Complete())
	require.NotNil(t, g.Verify)
	assert.True(t, g.Verify.OK, "%+v", g.Verify)
	assert.Equal(t, int64(5), g.Meta.RowCounts["public.daemon_fixture"])
	assert.Empty(t, g.Verify.Mismatches)
	assert.Contains(t, log, "backup: deleted an old generation")
	assert.Contains(t, log, "generation="+old.ID)
	assert.NotContains(t, log, "level=ERROR")
	// 使い捨てのサーバーは残らない。
	sb := r.ve.newSandbox(r.cfg.Backup).(containerSandbox)
	out, err := sb.sh(ctx, `ls -d /tmp/verify-* 2>/dev/null || true`)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out))
	_, err = os.Stat(filepath.Join(r.dir, backup.DirMarkerFile))
	require.NoError(t, err, "pruning never touches the marker")
}

// 実物の backup.Verify は、判定の後に後始末が失敗すると、3 段が全て通っていても
// OK を false にし、verify.json を書かずに誤りと一緒に返す。daemon のテストの偽物
// (internal/backup/daemon の fakeVerifier.afterErr) はこの形を真似ているので、
// 実物がこの形であることをここで確かめる。
func TestRealVerifyCleanupFailureShape(t *testing.T) {
	r := newDaemonRig(t, errors.New("rm: device busy"))
	m := r.take(t, time.Hour)
	vopts, err := verifyOptions(r.ve, r.cfg.Backup)
	require.NoError(t, err)
	res, err := backup.Verify(context.Background(), r.st, m.ID, vopts)
	require.ErrorContains(t, err, "device busy")
	require.Len(t, res.Stages, 3)
	for _, s := range res.Stages {
		assert.True(t, s.OK, "%+v", s)
		assert.False(t, s.Skipped)
	}
	assert.False(t, res.OK, "the real Verify reports OK false when cleaning up fails")
	_, err = backup.ReadVerify(context.Background(), r.st, m.ID)
	require.ErrorIs(t, err, backup.ErrNotFound, "and stores no verify.json")
}

// 後始末に失敗しても、通った世代は ok:true として残り、整理まで進む (H-1)。
func TestDaemonKeepsPassingVerdictWhenRealCleanupFails(t *testing.T) {
	r := newDaemonRig(t, errors.New("rm: device busy"))
	old := r.take(t, 48*time.Hour)
	gens, log := r.runUntil(t, func(gens []backup.Generation) bool {
		return len(gens) == 1 && gens[0].ID != old.ID && gens[0].Verify != nil
	})
	require.NotNil(t, gens[0].Verify)
	assert.True(t, gens[0].Verify.OK, "%+v", gens[0].Verify)
	assert.Contains(t, log, "verification reached a verdict but did not finish cleanly")
	assert.Contains(t, log, "device busy")
}

// cancelBeforeRestore cancels the daemon's context just before pg_restore
// loads the dump, as a SIGTERM during verification would.
type cancelBeforeRestore struct {
	containerSandbox
	cancel context.CancelFunc
}

func (s cancelBeforeRestore) Run(ctx context.Context, cmd backup.SandboxCmd) error {
	if cmd.Program == "pg_restore" && !slices.Contains(cmd.Args, "--list") {
		s.cancel()
	}
	return s.containerSandbox.Run(ctx, cmd)
}

// 止める途中で切れた検証は、判定として verify.json に残さない (M-1)。
func TestDaemonInterruptedRealVerificationLeavesNoVerdict(t *testing.T) {
	r := newDaemonRig(t, nil)
	m := r.take(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sb := r.ve.newSandbox(r.cfg.Backup).(containerSandbox)
	vopts, err := verifyOptions(r.ve, r.cfg.Backup)
	require.NoError(t, err)
	vopts.Sandbox = cancelBeforeRestore{containerSandbox: sb, cancel: cancel}
	var logs syncBuffer
	d := daemon.New(daemon.Options{Storage: r.st, Verifier: verifier{st: r.st, opts: vopts, verify: backup.Verify},
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool {
		_, err := d.Start(daemon.JobVerify, m.ID)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond)
	select {
	case <-done:
	case <-time.After(3 * time.Minute):
		t.Fatalf("the verification was never interrupted; log:\n%s", &logs)
	}
	_, err = backup.ReadVerify(context.Background(), r.st, m.ID)
	require.ErrorIs(t, err, backup.ErrNotFound, "an interrupted verification leaves no verdict; log:\n%s", &logs)
	st := d.Status()
	require.NotNil(t, st.LastVerify)
	assert.Contains(t, st.LastVerify.Error, "interrupted")
	out, err := sb.sh(context.Background(), `ls -d /tmp/verify-* 2>/dev/null || true`)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(out), "the throwaway server is still removed")
}
