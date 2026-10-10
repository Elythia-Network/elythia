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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	backupkit "github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/daemon"
	"github.com/elythia-network/elythia/internal/config"
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

	// 取る処理は backup.Take を、設定の pg_dump で呼ぶ。
	_, err = deps.Taker.Take(context.Background())
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
	_, _, err := build(e, cfg, discardLogger())
	require.EqualError(t, err, "deps failed")

	e, _, _ = daemonTestEnv(t, nil)
	d, h, err := build(e, cfg, discardLogger())
	require.NoError(t, err)
	require.NotNil(t, h)
	st := d.Status()
	assert.Nil(t, st.Running)
	assert.Equal(t, daemon.ScheduleInfo{Interval: "24h0m0s", Timezone: "UTC", DelayAfter: "36h0m0s"}, *st.Schedule)
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
