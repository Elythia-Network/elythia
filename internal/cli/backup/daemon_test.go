package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	backupkit "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/daemon"
	"github.com/elythia-network/elythia/internal/backup/restoreop"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintmode"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// emptyStorage is a Storage with nothing in it.
type emptyStorage struct{}

func (emptyStorage) Put(context.Context, string, io.Reader) error { return nil }
func (emptyStorage) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, backupkit.ErrNotFound
}
func (emptyStorage) Stat(context.Context, string) (backupkit.ObjectInfo, error) {
	return backupkit.ObjectInfo{}, backupkit.ErrNotFound
}
func (emptyStorage) List(context.Context, string) ([]backupkit.ObjectInfo, error) { return nil, nil }
func (emptyStorage) Delete(context.Context, string) error                         { return nil }

type nopTaker struct{}

func (nopTaker) Take(context.Context) (backupkit.Meta, error) {
	return backupkit.Meta{}, errors.New("no")
}

func daemonTestEnv(t *testing.T, cfg *config.Config) (daemonEnv, *syncBuffer, context.CancelFunc) {
	t.Helper()
	var stderr syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e := daemonEnv{
		stderr: &stderr,
		loadConfig: func(path string) (*config.Config, error) {
			if path == "missing.yml" {
				return nil, errors.New("no such file")
			}
			return cfg, nil
		},
		openDeps: func(*config.Config, *slog.Logger) (Deps, error) {
			return Deps{Storage: emptyStorage{}, Taker: nopTaker{}}, nil
		},
		signalContext: func() (context.Context, context.CancelFunc) { return ctx, cancel },
		listen:        func(string) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") },
		location:      time.UTC,
	}
	return e, &stderr, cancel
}

func cfgWith(b *config.BackupOptions) *config.Config {
	return &config.Config{URL: "https://example.tld", Backup: b}
}

func TestDaemonFlagsAndConfigErrors(t *testing.T) {
	e, stderr, _ := daemonTestEnv(t, cfgWith(nil))
	assert.Equal(t, 0, runDaemon(e, []string{"-h"}))
	assert.Contains(t, stderr.String(), "Usage: elythia backup daemon")
	assert.Equal(t, 2, runDaemon(e, []string{"stray"}))
	assert.Equal(t, 1, runDaemon(e, []string{"-config", "missing.yml"}))
	assert.Contains(t, stderr.String(), "cannot read the config: no such file")

	for _, tc := range []struct {
		name string
		b    *config.BackupOptions
		want string
	}{
		{"no section", nil, "no backup: section"},
		{"bad interval", &config.BackupOptions{Schedule: config.BackupScheduleOptions{Interval: "1"}}, "backup.schedule.interval"},
		{"nothing to do", &config.BackupOptions{}, "nothing to do"},
		{"listen without token", &config.BackupOptions{Schedule: config.BackupScheduleOptions{Listen: ":0"}}, "serviceToken is empty"},
		{"bad webhook", &config.BackupOptions{Schedule: config.BackupScheduleOptions{Interval: "24h"}, Notify: config.BackupNotifyOptions{WebhookURL: "ftp://x"}}, "webhookUrl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, stderr, _ := daemonTestEnv(t, cfgWith(tc.b))
			assert.Equal(t, 1, runDaemon(e, nil))
			assert.Contains(t, stderr.String(), tc.want)
		})
	}
}

func TestWireDepsErrors(t *testing.T) {
	core := filepath.Join(t.TempDir(), "migration")
	writeMigrations(t, core, "000001")
	dir := markedDir(t)
	dirStorage := config.BackupStorageOptions{Type: backupkit.StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: dir}}
	for _, tc := range []struct {
		name  string
		b     config.BackupOptions
		setup func(*env, *verifyEnv)
		want  string
	}{
		{"storage", config.BackupOptions{}, nil, "cannot open the storage: backup: storage.type is empty"},
		{"unmarked directory", config.BackupOptions{Storage: config.BackupStorageOptions{Type: backupkit.StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: t.TempDir()}}}, nil, backupkit.DirMarkerFile},
		{"recipients", config.BackupOptions{Storage: dirStorage, Encryption: config.BackupEncryptionOptions{Enabled: true}}, nil, "invalid encryption settings"},
		{"pg_dump connection", config.BackupOptions{Storage: dirStorage}, func(te *env, _ *verifyEnv) {
			te.dumpConn = func(*config.Config) (backupkit.DumpConn, error) { return backupkit.DumpConn{}, errors.New("broken") }
		}, "pg_dump connection: broken"},
		{"bundled migrations", config.BackupOptions{Storage: dirStorage}, func(_ *env, ve *verifyEnv) {
			ve.coreDir = filepath.Join(t.TempDir(), "none")
		}, "bundled migrations"},
		{"identity file", config.BackupOptions{Storage: dirStorage, Encryption: config.BackupEncryptionOptions{IdentityFile: filepath.Join(t.TempDir(), "none")}}, nil, "identityFile"},
		{"verify without identity file", config.BackupOptions{Storage: dirStorage,
			Encryption: config.BackupEncryptionOptions{Enabled: true, Recipients: []string{testRecipient(t)}},
			Schedule:   config.BackupScheduleOptions{Verify: true}}, nil, "needs backup.encryption.identityFile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			te := defaultEnv()
			ve := defaultVerifyEnv()
			ve.coreDir, ve.localDir = core, filepath.Join(core, "local")
			if tc.setup != nil {
				tc.setup(&te, &ve)
			}
			b := tc.b
			_, err := wireDeps(te, ve)(cfgWith(&b), discardLogger())
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// recordingRunner fails every program and records what was run.
type recordingRunner struct{ names *[]string }

func (r recordingRunner) Command(ctx context.Context, _ []string, name string, args ...string) *exec.Cmd {
	*r.names = append(*r.names, name)
	return exec.CommandContext(ctx, "false")
}

// The daemon warns at start when scheduled verification would use the
// default temporary directory.
func TestWireDepsWarnsWithoutTMPDIR(t *testing.T) {
	core := filepath.Join(t.TempDir(), "migration")
	writeMigrations(t, core, "000001")
	ve := defaultVerifyEnv()
	ve.coreDir, ve.localDir = core, filepath.Join(core, "local")
	for _, tc := range []struct {
		name   string
		verify bool
		listen string
		tmpdir string
		warn   bool
	}{
		{"scheduled verification without TMPDIR", true, "", "", true},
		{"scheduled verification with TMPDIR", true, "", "/scratch", false},
		{"control API without TMPDIR", false, "0.0.0.0:3010", "", true},
		{"control API with TMPDIR", false, "0.0.0.0:3010", "/scratch", false},
		{"neither", false, "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ve.getenv = func(k string) string {
				if k == "TMPDIR" {
					return tc.tmpdir
				}
				return ""
			}
			b := &config.BackupOptions{
				Storage:  config.BackupStorageOptions{Type: backupkit.StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: markedDir(t)}},
				Schedule: config.BackupScheduleOptions{Verify: tc.verify, Listen: tc.listen},
			}
			var log bytes.Buffer
			_, err := wireDeps(defaultEnv(), ve)(cfgWith(b), slog.New(slog.NewTextHandler(&log, nil)))
			require.NoError(t, err)
			if tc.warn {
				assert.Contains(t, log.String(), "TMPDIR is not set")
			} else {
				assert.NotContains(t, log.String(), "TMPDIR")
			}
		})
	}
}

func TestWireDepsBuildsTakerAndVerifier(t *testing.T) {
	core := filepath.Join(t.TempDir(), "migration")
	writeMigrations(t, core, "000001", "000009")
	dir := markedDir(t)
	var ran []string
	te := defaultEnv()
	te.runner = recordingRunner{names: &ran}
	te.dumpConn = func(*config.Config) (backupkit.DumpConn, error) {
		return backupkit.DumpConn{URI: "postgresql://test@localhost:5432/elythia"}, nil
	}
	ve := defaultVerifyEnv()
	ve.coreDir, ve.localDir = core, filepath.Join(core, "local")
	var gotID string
	var gotOpts backupkit.VerifyOptions
	var gotStorage backupkit.Storage
	ve.verify = func(_ context.Context, st backupkit.Storage, id string, o backupkit.VerifyOptions) (backupkit.VerifyResult, error) {
		gotStorage, gotID, gotOpts = st, id, o
		return backupkit.VerifyResult{ID: id, OK: true}, errors.New("cleanup failed")
	}
	b := &config.BackupOptions{
		Storage: config.BackupStorageOptions{Type: backupkit.StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: dir}},
		Tools:   config.BackupToolsOptions{PgDump: "/opt/pg/pg_dump", Initdb: "/opt/pg/initdb"},
	}
	deps, err := wireDeps(te, ve)(cfgWith(b), discardLogger())
	require.NoError(t, err)
	require.IsType(t, &backupkit.DirStorage{}, deps.Storage)

	// 取る処理は backup.Take を、設定の pg_dump で呼ぶ。DB を待つのは別のテストで見る。
	tk, ok := deps.Taker.(taker)
	require.True(t, ok)
	require.NotNil(t, tk.ping)
	assert.Equal(t, defaultDBWait, tk.dbWait)
	tk.dbWait = 0
	_, err = tk.Take(context.Background())
	require.Error(t, err)
	require.NotEmpty(t, ran)
	assert.Equal(t, "/opt/pg/pg_dump", ran[0])

	// 確かめる処理は backup.Verify に、同じ保存先と verify と同じ options を渡し、
	// 結果と誤りをそのまま返す。
	res, err := deps.Verifier.Verify(context.Background(), "20261010T040000Z")
	require.EqualError(t, err, "cleanup failed")
	assert.True(t, res.OK)
	assert.Equal(t, "20261010T040000Z", gotID)
	assert.Same(t, deps.Storage, gotStorage)
	assert.Equal(t, backupkit.BundledMigrations{Core: 9}, gotOpts.Bundled)
	assert.Equal(t, backupkit.LocalSandbox{Tools: b.Tools}, gotOpts.Sandbox)
}

func testRecipient(t *testing.T) string {
	t.Helper()
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	return id.Recipient().String()
}

// pingSeq returns errors from errs in order, then nil.
type pingSeq struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (p *pingSeq) ping(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.errs) == 0 {
		return nil
	}
	err := p.errs[0]
	p.errs = p.errs[1:]
	return err
}

func TestTakerWaitsForDatabase(t *testing.T) {
	var ran []string
	run := recordingRunner{names: &ran}
	opts := backupkit.TakeOptions{Storage: emptyStorage{}, Dump: backupkit.DumpConn{URI: "postgresql://x@localhost/x"}, Runner: run}
	var logs syncBuffer
	opts.Logger = slog.New(slog.NewTextHandler(&logs, nil))

	// 2 回繋がらず、3 回目で繋がってから取りにいく。
	p := &pingSeq{errs: []error{errors.New("refused"), errors.New("refused")}}
	tk := taker{opts: opts, ping: p.ping, dbWait: time.Hour, retry: time.Millisecond, now: time.Now}
	_, err := tk.Take(context.Background())
	require.Error(t, err, "the fake pg_dump fails")
	assert.Equal(t, 3, p.calls)
	assert.Len(t, ran, 1, "the take runs once the database answers")
	assert.Contains(t, logs.String(), "waiting for the database")

	// 待ちきれなければ、そのまま取りにいく (失敗として知らせるため)。
	ran = nil
	p = &pingSeq{errs: []error{errors.New("refused"), errors.New("refused"), errors.New("refused")}}
	clock := time.Now()
	tk = taker{opts: opts, ping: p.ping, dbWait: 2 * time.Second, retry: time.Millisecond, now: func() time.Time {
		clock = clock.Add(time.Second)
		return clock
	}}
	_, err = tk.Take(context.Background())
	require.Error(t, err)
	assert.Len(t, ran, 1)
	assert.Contains(t, logs.String(), "still does not accept connections")

	// ctx が切れたら待たない。
	ran = nil
	p = &pingSeq{errs: []error{errors.New("refused")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tk = taker{opts: opts, ping: p.ping, dbWait: time.Hour, retry: time.Hour, now: time.Now}
	_, err = tk.Take(ctx)
	require.Error(t, err)
	assert.Equal(t, 1, p.calls)

	// Logger が無くても落ちない。
	tk = taker{opts: backupkit.TakeOptions{Storage: emptyStorage{}}, ping: (&pingSeq{errs: []error{errBoomCLI}}).ping, dbWait: 0, now: time.Now}
	_, err = tk.Take(context.Background())
	require.Error(t, err)
}

var errBoomCLI = errors.New("boom")

func TestNewTakerPingsTheDatabase(t *testing.T) {
	tk := newTaker(backupkit.TakeOptions{DatabaseURL: "postgres://x:y@127.0.0.1:1/x?sslmode=disable&connect_timeout=1"})
	require.Error(t, tk.ping(context.Background()))
	assert.Equal(t, defaultDBRetry, tk.retry)
}

func TestDefaultDaemonEnv(t *testing.T) {
	e := defaultDaemonEnv()
	require.NotNil(t, e.openDeps)
	assert.Equal(t, time.Local, e.location)
	ctx, cancel := e.signalContext()
	cancel()
	<-ctx.Done()
	ln, err := e.listen("127.0.0.1:0")
	require.NoError(t, err)
	ln.Close()
	_, err = e.loadConfig("does-not-exist.yml")
	require.Error(t, err)
}

func TestDaemonServesControlAPIUntilSignal(t *testing.T) {
	b := &config.BackupOptions{
		Schedule: config.BackupScheduleOptions{Listen: "127.0.0.1:0"},
		Server:   config.BackupServerOptions{ServiceToken: "tok"},
		Notify:   config.BackupNotifyOptions{WebhookURL: "https://hooks.example.tld/x", Format: "slack"},
	}
	e, stderr, cancel := daemonTestEnv(t, cfgWith(b))
	addr := make(chan string, 1)
	e.listen = func(string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			addr <- ln.Addr().String()
		}
		return ln, err
	}
	code := make(chan int, 1)
	go func() { code <- runDaemon(e, nil) }()
	url := "http://" + <-addr + "/status"
	var resp *http.Response
	require.Eventually(t, func() bool {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer tok")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		resp = r
		return true
	}, 5*time.Second, 10*time.Millisecond)
	resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	cancel()
	assert.Equal(t, 0, <-code)
	assert.Contains(t, stderr.String(), "control API listening")
	assert.NotContains(t, stderr.String(), "webhookUrl is empty")
}

func TestDaemonScheduleOnlyWarnsWithoutWebhook(t *testing.T) {
	e, stderr, cancel := daemonTestEnv(t, cfgWith(&config.BackupOptions{Schedule: config.BackupScheduleOptions{Interval: "24h"}}))
	code := make(chan int, 1)
	go func() { code <- runDaemon(e, nil) }()
	require.Eventually(t, func() bool { return strings.Contains(stderr.String(), "schedule started") }, 5*time.Second, 10*time.Millisecond)
	cancel()
	assert.Equal(t, 0, <-code)
	assert.Contains(t, stderr.String(), "webhookUrl is empty")
	assert.NotContains(t, stderr.String(), "control API listening")
}

func TestDaemonListenErrors(t *testing.T) {
	b := &config.BackupOptions{Schedule: config.BackupScheduleOptions{Listen: "bad"}, Server: config.BackupServerOptions{ServiceToken: "tok"}}
	e, stderr, _ := daemonTestEnv(t, cfgWith(b))
	e.listen = func(string) (net.Listener, error) { return nil, errors.New("address in use") }
	assert.Equal(t, 1, runDaemon(e, nil))
	assert.Contains(t, stderr.String(), "cannot listen on bad: address in use")
}

func TestDaemonStopsWhenControlAPIFails(t *testing.T) {
	b := &config.BackupOptions{Schedule: config.BackupScheduleOptions{Interval: "24h", Listen: "x"}, Server: config.BackupServerOptions{ServiceToken: "tok"}}
	e, stderr, _ := daemonTestEnv(t, cfgWith(b))
	e.listen = func(string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			ln.Close()
		}
		return ln, err
	}
	assert.Equal(t, 1, runDaemon(e, nil))
	assert.Contains(t, stderr.String(), "control API:")
}

func TestBuildPassesOptions(t *testing.T) {
	e, _, _ := daemonTestEnv(t, nil)
	cfg := cfgWith(&config.BackupOptions{
		Schedule: config.BackupScheduleOptions{Interval: "24h", Listen: ":0"},
		Server:   config.BackupServerOptions{ServiceToken: "tok"},
	})
	e.openDeps = func(*config.Config, *slog.Logger) (Deps, error) { return Deps{}, errors.New("deps failed") }
	_, _, _, err := build(e, cfg, discardLogger())
	require.EqualError(t, err, "deps failed")

	e, _, _ = daemonTestEnv(t, nil)
	d, h, _, err := build(e, cfg, discardLogger())
	require.NoError(t, err)
	require.NotNil(t, h)
	st := d.Status()
	assert.Nil(t, st.Running)
	assert.Equal(t, daemon.ScheduleInfo{Interval: "24h0m0s", Timezone: "UTC", DelayAfter: "36h0m0s"}, *st.Schedule)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestBuildRestoreParts checks that the restore from the admin page is
// built only with the control API, and that a failure to build it leaves
// taking and verifying running.
func TestBuildRestoreParts(t *testing.T) {
	cfg := cfgWith(&config.BackupOptions{
		Schedule: config.BackupScheduleOptions{Interval: "24h", Listen: ":0"},
		Server:   config.BackupServerOptions{ServiceToken: "tok"},
	})
	var built int
	e, _, _ := daemonTestEnv(t, nil)
	e.openRestore = func(*config.Config, *daemon.Daemon, backupkit.Storage, *slog.Logger) (*RestoreParts, error) {
		built++
		return nil, errors.New("redis down")
	}
	d, h, parts, err := build(e, cfg, discardLogger())
	require.NoError(t, err, "the daemon starts without the restore")
	assert.NotNil(t, d)
	assert.NotNil(t, h)
	assert.Nil(t, parts)
	assert.Equal(t, 1, built)

	noAPI := cfgWith(&config.BackupOptions{Schedule: config.BackupScheduleOptions{Interval: "24h"}})
	_, h, parts, err = build(e, noAPI, discardLogger())
	require.NoError(t, err)
	assert.Nil(t, h)
	assert.Nil(t, parts)
	assert.Equal(t, 1, built, "not built without the control API")
}

// TestDaemonStartsAndClosesTheRestore checks that runDaemon starts the
// restore manager (which recovers an unfinished restore with the job slot
// reserved before Run) and closes it.
func TestDaemonStartsAndClosesTheRestore(t *testing.T) {
	cfg := cfgWith(&config.BackupOptions{
		Schedule: config.BackupScheduleOptions{Listen: ":0"},
		Server:   config.BackupServerOptions{ServiceToken: "tok"},
	})
	e, _, cancel := daemonTestEnv(t, cfg)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	store := restoreop.NewStore(rdb, "p:")
	closed := make(chan struct{})
	var acquired atomic.Int32
	e.openRestore = func(_ *config.Config, d *daemon.Daemon, _ backupkit.Storage, _ *slog.Logger) (*RestoreParts, error) {
		m := restoreop.New(restoreop.Options{
			Store: store, Maint: maintmode.NewStore(rdb, rdb, "p:"), DB: stubRestoreDB{},
			Acquire: func() (func(), error) {
				release, err := d.Acquire(daemon.JobRestore)
				if err == nil {
					acquired.Add(1)
				}
				return release, err
			},
			Logger: discardLogger(),
		})
		return &RestoreParts{Manager: m, Close: func() { close(closed) }}, nil
	}
	// 前の実行が残した、片付けるべき記録 (中止の途中) を置く。
	require.NoError(t, rdb.Set(context.Background(), store.CurrentKey(), "restore-left", 0).Err())
	require.NoError(t, rdb.HSet(context.Background(), "p:elythia:maintenance:restore:restore-left", "opId", "restore-left", "step", "aborting").Err())
	done := make(chan int, 1)
	go func() { done <- runDaemon(e, nil) }()
	require.Eventually(t, func() bool {
		step, _ := rdb.HGet(context.Background(), "p:elythia:maintenance:restore:restore-left", "step").Result()
		return step == restoreop.StepAborted
	}, 5*time.Second, 10*time.Millisecond, "the manager recovers the operation")
	// 枠は Run の前に預けたもので足り、後から取りに行かない (Run の最初の job より先に片付ける)。
	assert.Zero(t, acquired.Load())
	cancel()
	select {
	case code := <-done:
		assert.Equal(t, 0, code)
	case <-time.After(10 * time.Second):
		t.Fatal("runDaemon did not return")
	}
	select {
	case <-closed:
	default:
		t.Fatal("the restore parts were not closed")
	}
}

// stubRestoreDB does nothing.
type stubRestoreDB struct{}

func (stubRestoreDB) Load(context.Context, string) (*backupkit.RestorePlan, error) { return nil, nil }
func (stubRestoreDB) CheckTarget(context.Context, backupkit.RestoreOptions) error  { return nil }
func (stubRestoreDB) CheckCompatibility(context.Context, *backupkit.RestorePlan, backupkit.RestoreOptions) error {
	return nil
}
func (stubRestoreDB) RestoreInto(context.Context, *backupkit.RestorePlan, backupkit.RestoreOptions, string) error {
	return nil
}
func (stubRestoreDB) VerifyRestored(context.Context, string, backupkit.Meta) error { return nil }
func (stubRestoreDB) ApplyDatabaseSettings(context.Context, string, backupkit.Meta) ([]string, error) {
	return nil, nil
}
func (stubRestoreDB) Migrate(string) ([]backupkit.MigrationState, error) { return nil, nil }
func (stubRestoreDB) SwitchDatabase(context.Context, string, string, string, string) error {
	return nil
}
func (stubRestoreDB) DropDatabase(context.Context, string, string) error { return nil }
func (stubRestoreDB) ExistingDatabases(context.Context, string, []string) (map[string]bool, error) {
	return nil, nil
}
