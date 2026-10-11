package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"filippo.io/age"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
)

// listStorage is a Storage that lists fixed keys and serves a minimal
// meta.json for each generation.
type listStorage struct {
	keys []string
	err  error
}

func (s listStorage) Put(context.Context, string, io.Reader) error { return errors.New("read-only") }
func (s listStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	id := backup.GenerationIDFromKey(key)
	if id == "" || key != backup.Key(id, backup.MetaFile) {
		return nil, backup.ErrNotFound
	}
	body, err := json.Marshal(backup.Meta{FormatVersion: backup.MetaFormatVersion, ID: id})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}
func (s listStorage) Stat(context.Context, string) (backup.ObjectInfo, error) {
	return backup.ObjectInfo{}, backup.ErrNotFound
}
func (s listStorage) List(context.Context, string) ([]backup.ObjectInfo, error) {
	out := make([]backup.ObjectInfo, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, backup.ObjectInfo{Key: k})
	}
	return out, s.err
}
func (s listStorage) Delete(context.Context, string) error { return nil }

type harness struct {
	env      verifyEnv
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	cfg      *config.Config
	gotID    string
	gotOpts  backup.VerifyOptions
	result   backup.VerifyResult
	err      error
	verified bool
}

func writeMigrations(t *testing.T, dir string, versions ...string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	for _, v := range versions {
		require.NoError(t, os.WriteFile(filepath.Join(dir, v+"_x.up.sql"), nil, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, v+"_x.down.sql"), nil, 0o644))
	}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	core := filepath.Join(root, "migration")
	writeMigrations(t, core, "000001", "000002", "000007")
	h := &harness{cfg: &config.Config{Backup: &config.BackupOptions{}}}
	h.result = backup.VerifyResult{ID: "20261010T000000Z", OK: true, Stages: []backup.StageResult{
		{Stage: backup.StageReadable, OK: true}, {Stage: backup.StageRestorable, OK: true}, {Stage: backup.StageUsable, OK: true},
	}}
	h.env = verifyEnv{
		stdout:     &h.stdout,
		stderr:     &h.stderr,
		loadConfig: func(string) (*config.Config, error) { return h.cfg, nil },
		openStorage: func(config.BackupStorageOptions) (backup.Storage, error) {
			return listStorage{keys: []string{
				backup.Key("20261009T000000Z", backup.MetaFile),
				backup.Key("20261010T000000Z", backup.MetaFile),
			}}, nil
		},
		newSandbox: func(*config.BackupOptions) backup.Sandbox { return backup.LocalSandbox{} },
		verify: func(_ context.Context, _ backup.Storage, id string, o backup.VerifyOptions) (backup.VerifyResult, error) {
			h.verified = true
			h.gotID, h.gotOpts = id, o
			return h.result, h.err
		},
		coreDir:  core,
		localDir: filepath.Join(core, "local"),
	}
	return h
}

func (h *harness) run(args ...string) int {
	return runVerify(context.Background(), h.env, args)
}

func TestRunVerifyLatestOK(t *testing.T) {
	h := newHarness(t)
	assert.Equal(t, 0, h.run("latest"))
	assert.Equal(t, "20261010T000000Z", h.gotID)
	assert.Equal(t, backup.BundledMigrations{Core: 7, Local: 0}, h.gotOpts.Bundled)
	assert.NotNil(t, h.gotOpts.Sandbox)
	assert.Empty(t, h.gotOpts.Identities)
	assert.Contains(t, h.stdout.String(), "[ok  ] usable")
	assert.Contains(t, h.stdout.String(), "This backup can be restored.")
}

func TestRunVerifyPassesLocalMigrationsAndIdentity(t *testing.T) {
	h := newHarness(t)
	writeMigrations(t, h.env.localDir, "900001", "900003")
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	keyFile := filepath.Join(t.TempDir(), "key.txt")
	require.NoError(t, os.WriteFile(keyFile, []byte(id.String()+"\n"), 0o600))
	h.cfg.Backup.Encryption.IdentityFile = keyFile

	assert.Equal(t, 0, h.run("-config", "x.yml", "20261001T000000Z"))
	assert.Equal(t, "20261001T000000Z", h.gotID)
	assert.Equal(t, backup.BundledMigrations{Core: 7, Local: 900003}, h.gotOpts.Bundled)
	require.Len(t, h.gotOpts.Identities, 1)
}

func TestRunVerifyReportsFailure(t *testing.T) {
	h := newHarness(t)
	h.result = backup.VerifyResult{ID: "20261010T000000Z", Stages: []backup.StageResult{
		{Stage: backup.StageReadable, OK: true},
		{Stage: backup.StageRestorable, Error: "row counts of 2 table(s) do not match meta.json"},
		{Stage: backup.StageUsable, Skipped: true},
	}, Mismatches: []backup.RowMismatch{
		{Table: "public.note", Expected: 10, Actual: 9},
		{Table: "public.gone", Expected: 1, Actual: -1},
	}}
	assert.Equal(t, 1, h.run("latest"))
	out := h.stdout.String()
	assert.Contains(t, out, "[FAIL] restorable: row counts")
	assert.Contains(t, out, "[skip] usable")
	assert.Contains(t, out, "public.note: meta.json 10 rows / restored 9")
	assert.Contains(t, out, "public.gone: meta.json 1 rows / restored missing")
	assert.Contains(t, out, "This backup cannot be restored.")
}

func TestRunVerifyPrintsWarningsAndErrors(t *testing.T) {
	h := newHarness(t)
	h.result.Stages[2].Warnings = []string{"fsck: 1 counter drift(s)"}
	h.err = errors.New("remove the sandbox directory: busy")
	assert.Equal(t, 1, h.run("latest"))
	assert.Contains(t, h.stdout.String(), "warning: fsck: 1 counter drift(s)")
	assert.Contains(t, h.stderr.String(), "busy")
}

func TestRunVerifyEnvironmentErrorIsNotAVerdict(t *testing.T) {
	h := newHarness(t)
	h.result = backup.VerifyResult{ID: "20261010T000000Z", Stages: []backup.StageResult{{Stage: backup.StageReadable, OK: true}}}
	h.err = errors.New("the backup uses extension(s) pg_bigm that the throwaway PostgreSQL server cannot install")
	assert.Equal(t, 1, h.run("latest"))
	assert.Contains(t, h.stdout.String(), "This backup was not judged")
	assert.NotContains(t, h.stdout.String(), "cannot be restored")
	assert.Contains(t, h.stderr.String(), "pg_bigm")
}

func TestRunVerifyErrorWithoutStagesPrintsNoTable(t *testing.T) {
	h := newHarness(t)
	h.result = backup.VerifyResult{}
	h.err = errors.New("generation has no meta.json")
	assert.Equal(t, 1, h.run("latest"))
	assert.Empty(t, h.stdout.String())
	assert.Contains(t, h.stderr.String(), "no meta.json")
}

func TestRunVerifyArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"-h"}, 0},
		{[]string{"-nope", "latest"}, 2},
		{nil, 2},
		{[]string{"latest", "20261010T000000Z"}, 2},
	} {
		h := newHarness(t)
		assert.Equal(t, tc.code, h.run(tc.args...), "%v", tc.args)
		assert.False(t, h.verified, "%v", tc.args)
		if tc.code == 2 && len(tc.args) != 1 {
			assert.Contains(t, h.stderr.String(), "Usage: elythia backup verify", "%v", tc.args)
		}
	}
}

func TestRunVerifyStopsBeforeVerifying(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, h *harness)
		arg   string
		want  string
	}{
		{"config error", func(_ *testing.T, h *harness) {
			h.env.loadConfig = func(string) (*config.Config, error) { return nil, errors.New("bad yaml") }
		}, "latest", "bad yaml"},
		{"no backup section", func(_ *testing.T, h *harness) { h.cfg.Backup = nil }, "latest", "no backup: section"},
		{"no bundled migrations", func(t *testing.T, h *harness) { h.env.coreDir = filepath.Join(t.TempDir(), "none") }, "latest", "bundled migrations"},
		{"empty bundled migrations", func(t *testing.T, h *harness) { h.env.coreDir = t.TempDir() }, "latest", "bundled migrations"},
		{"unreadable local migrations", func(t *testing.T, h *harness) {
			f := filepath.Join(t.TempDir(), "file")
			require.NoError(t, os.WriteFile(f, nil, 0o644))
			h.env.localDir = f
		}, "latest", "fork migrations"},
		{"identity file missing", func(t *testing.T, h *harness) {
			h.cfg.Backup.Encryption.IdentityFile = filepath.Join(t.TempDir(), "none")
		}, "latest", "identityFile"},
		{"identity file malformed", func(t *testing.T, h *harness) {
			f := filepath.Join(t.TempDir(), "key.txt")
			require.NoError(t, os.WriteFile(f, []byte("not a key\n"), 0o600))
			h.cfg.Backup.Encryption.IdentityFile = f
		}, "latest", "identityFile"},
		{"storage error", func(_ *testing.T, h *harness) {
			h.env.openStorage = func(config.BackupStorageOptions) (backup.Storage, error) { return nil, errors.New("no bucket") }
		}, "latest", "no bucket"},
		{"invalid id", func(*testing.T, *harness) {}, "../x", "invalid generation id"},
		{"no generation", func(_ *testing.T, h *harness) {
			h.env.openStorage = func(config.BackupStorageOptions) (backup.Storage, error) { return listStorage{}, nil }
		}, "latest", "no complete generation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.setup(t, h)
			assert.Equal(t, 1, h.run(tc.arg))
			assert.False(t, h.verified)
			assert.Contains(t, h.stderr.String(), tc.want)
		})
	}
}

func TestDefaultVerifyEnv(t *testing.T) {
	e := defaultVerifyEnv()
	tools := config.BackupToolsOptions{PgRestore: "/usr/local/bin/pg_restore"}
	assert.Equal(t, backup.LocalSandbox{Tools: tools}, e.newSandbox(&config.BackupOptions{Tools: tools}))
	assert.Equal(t, "migration", e.coreDir)
	assert.Equal(t, "migration/local", e.localDir)
	// 保存先は #3458 の OpenStorage で開く。
	_, err := e.openStorage(config.BackupStorageOptions{})
	require.ErrorContains(t, err, "storage.type is empty")
	dir := t.TempDir()
	require.NoError(t, backup.CreateDirMarker(dir))
	st, err := e.openStorage(config.BackupStorageOptions{Type: backup.StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: dir}})
	require.NoError(t, err)
	assert.IsType(t, &backup.DirStorage{}, st)
}

func TestVerifyHelp(t *testing.T) {
	assert.Equal(t, 0, Verify([]string{"-h"}))
}
