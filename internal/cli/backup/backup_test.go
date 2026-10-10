package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
)

// pg_dump はホストの版 (15) では 18 のサーバーから取れないので、postgres:18-alpine
// のコンテナを立て、その中で動かす。

var (
	pgOnce sync.Once
	pgC    *tcpostgres.PostgresContainer
	pgErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if pgC != nil {
		_ = pgC.Terminate(context.Background())
	}
	if daemonPgC != nil {
		_ = daemonPgC.Terminate(context.Background())
	}
	os.Exit(code)
}

// dockerExecRunner runs programs inside a container with docker exec, as user
// when it is set (initdb refuses to run as root).
type dockerExecRunner struct{ container, user string }

func (r dockerExecRunner) Command(ctx context.Context, env []string, name string, args ...string) *exec.Cmd {
	a := []string{"exec", "-i"}
	if r.user != "" {
		a = append(a, "-u", r.user)
	}
	for _, e := range env {
		a = append(a, "-e", e)
	}
	a = append(append(a, r.container, name), args...)
	return exec.CommandContext(ctx, "docker", a...)
}

// startDB starts PostgreSQL once, seeds it and returns the container.
func startDB(t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	pgOnce.Do(func() {
		ctx := context.Background()
		pgC, pgErr = tcpostgres.Run(ctx, "postgres:18-alpine",
			tcpostgres.WithDatabase("elythia"),
			tcpostgres.WithUsername("test"),
			tcpostgres.WithPassword("test"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if pgErr != nil {
			return
		}
		var url string
		if url, pgErr = pgC.ConnectionString(ctx, "sslmode=disable"); pgErr != nil {
			return
		}
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			pgErr = err
			return
		}
		defer conn.Close(ctx)
		_, pgErr = conn.Exec(ctx, `CREATE TABLE note (id int);
INSERT INTO note SELECT generate_series(1, 42);
CREATE TABLE schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL);
INSERT INTO schema_migrations VALUES (121, false);`)
	})
	require.NoError(t, pgErr)
	return pgC
}

// writeConfig writes a config whose db points at the container and whose
// backup section is backupYAML.
func writeConfig(t *testing.T, c *tcpostgres.PostgresContainer, backupYAML string) string {
	t.Helper()
	ctx := context.Background()
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	body := fmt.Sprintf(`url: http://127.0.0.1:3000/
port: 3000
db:
  host: %s
  port: %s
  db: elythia
  user: test
  pass: test
%s`, host, port.Port(), backupYAML)
	path := filepath.Join(t.TempDir(), "default.yml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func testEnv(c *tcpostgres.PostgresContainer) (env, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	e := defaultEnv()
	e.stdout, e.stderr = &stdout, &stderr
	if c != nil {
		e.runner = dockerExecRunner{container: c.GetContainerID()}
		e.dumpConn = func(*config.Config) (backup.DumpConn, error) {
			return backup.DumpConn{URI: "postgresql://test@localhost:5432/elythia?sslmode=disable", Password: "test"}, nil
		}
	}
	return e, &stdout, &stderr
}

func dirBackupYAML(dir, extra string) string {
	return fmt.Sprintf("backup:\n  storage:\n    type: dir\n    dir:\n      path: %s\n%s", dir, extra)
}

func TestDefaultEnv(t *testing.T) {
	e := defaultEnv()
	assert.Equal(t, os.Stdout, e.stdout)
	assert.Equal(t, os.Stderr, e.stderr)
	assert.IsType(t, backup.LocalRunner{}, e.runner)
	assert.Nil(t, e.dumpConn, "pg_dump connects with the config's db settings")
}

func TestTakeAndList(t *testing.T) {
	c := startDB(t)
	dir := markedDir(t)
	cfg := writeConfig(t, c, dirBackupYAML(dir, ""))
	ctx := context.Background()

	e, stdout, stderr := testEnv(c)
	require.Equal(t, 0, take(ctx, e, []string{"-config", cfg}), stderr.String())
	out := stdout.String()
	assert.Contains(t, out, "generation ")
	assert.Contains(t, out, "encrypted: false")
	assert.Contains(t, out, "schema_migrations         121")
	assert.Contains(t, out, "schema_migrations_local   missing")

	st, err := backup.NewDirStorage(dir)
	require.NoError(t, err)
	gens, err := backup.ListGenerations(ctx, st)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	require.True(t, gens[0].Complete())
	assert.Equal(t, int64(42), gens[0].Meta.RowCounts["public.note"])
	assert.Contains(t, out, gens[0].ID)

	// 途中で止まった世代も一覧に出す。
	require.NoError(t, st.Put(ctx, backup.Key("20200101T000000Z", backup.DumpFile), strings.NewReader("partial")))

	e, stdout, stderr = testEnv(nil)
	require.Equal(t, 0, list(ctx, e, []string{"-config", cfg}), stderr.String())
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 3)
	assert.Regexp(t, `^ID\s+STATUS\s+SIZE\s+ENCRYPTED\s+VERIFIED\s+ELYTHIA\s+MIGRATION$`, lines[0])
	assert.Regexp(t, `^20200101T000000Z\s+incomplete\s+7\s+-\s+-\s+-\s+-$`, lines[1])
	assert.Regexp(t, `^`+gens[0].ID+`\s+complete\s+\d+\s+false\s+-\s+`+config.MkGoVersion+`\s+121$`, lines[2])

	e, stdout, stderr = testEnv(nil)
	require.Equal(t, 0, list(ctx, e, []string{"-config", cfg, "-json"}), stderr.String())
	var listed []listedGeneration
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &listed))
	require.Len(t, listed, 2)
	assert.False(t, listed[0].Complete)
	assert.Nil(t, listed[0].Meta)
	assert.True(t, listed[1].Complete)
	assert.Equal(t, gens[0].Meta.DumpSHA256, listed[1].Meta.DumpSHA256)
}

func TestTake_Encrypted(t *testing.T) {
	c := startDB(t)
	dir := markedDir(t)
	id, err := age.GenerateX25519Identity()
	require.NoError(t, err)
	cfg := writeConfig(t, c, dirBackupYAML(dir, fmt.Sprintf("  encryption:\n    enabled: true\n    recipients:\n      - %s\n", id.Recipient())))

	e, stdout, stderr := testEnv(c)
	require.Equal(t, 0, take(context.Background(), e, []string{"-config", cfg}), stderr.String())
	assert.Contains(t, stdout.String(), "encrypted: true")
	assert.Contains(t, stdout.String(), backup.DumpFileAge)
	matches, err := filepath.Glob(filepath.Join(dir, "generations", "*", backup.DumpFileAge))
	require.NoError(t, err)
	require.Len(t, matches, 1)
	body, err := os.ReadFile(matches[0])
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(body, []byte("age-encryption.org/v1\n")))
}

func TestTake_Failures(t *testing.T) {
	c := startDB(t)
	ctx := context.Background()
	dir := markedDir(t)
	for _, tc := range []struct {
		name, backupYAML, want string
		dumpConnErr            bool
	}{
		{name: "no backup section", backupYAML: "", want: "no backup: section"},
		{name: "unknown storage", backupYAML: "backup:\n  storage:\n    type: local\n", want: "unknown storage.type"},
		{name: "missing directory", backupYAML: dirBackupYAML(filepath.Join(dir, "unmounted"), ""), want: "storage directory"},
		{name: "directory without the marker", backupYAML: dirBackupYAML(t.TempDir(), ""), want: backup.DirMarkerFile},
		{name: "encryption without recipients", backupYAML: dirBackupYAML(dir, "  encryption:\n    enabled: true\n"), want: "recipients is empty"},
		{name: "pg_dump connection", backupYAML: dirBackupYAML(dir, ""), want: "pg_dump connection", dumpConnErr: true},
		{name: "pg_dump missing", backupYAML: dirBackupYAML(dir, "  tools:\n    pgDump: /nonexistent/pg_dump\n"), want: "/nonexistent/pg_dump"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, _, stderr := testEnv(c)
			if tc.dumpConnErr {
				e.dumpConn = func(*config.Config) (backup.DumpConn, error) { return backup.DumpConn{}, fmt.Errorf("broken") }
			}
			if tc.name == "pg_dump missing" {
				e.runner = backup.LocalRunner{}
			}
			assert.Equal(t, 1, take(ctx, e, []string{"-config", writeConfig(t, c, tc.backupYAML)}))
			assert.Contains(t, stderr.String(), tc.want)
		})
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "failed takes leave nothing in the storage")
	assert.Equal(t, backup.DirMarkerFile, entries[0].Name())

	// db.host が URL として読めなくても、ログに DB のパスワードを出さない。
	leak := filepath.Join(t.TempDir(), "default.yml")
	require.NoError(t, os.WriteFile(leak, []byte("url: http://127.0.0.1:3000/\nport: 3000\ndb:\n  host: db x\n  port: 5432\n  db: x\n  user: mk\n  pass: SECRETPW\n"+dirBackupYAML(dir, "")), 0o600))
	e, stdout, stderr := testEnv(nil)
	assert.Equal(t, 1, take(ctx, e, []string{"-config", leak}))
	assert.Contains(t, stderr.String(), "db.host")
	assert.NotContains(t, stderr.String()+stdout.String(), "SECRETPW")

	e, _, stderr = testEnv(nil)
	assert.Equal(t, 1, take(ctx, e, []string{"-config", filepath.Join(t.TempDir(), "absent.yml")}))
	assert.Contains(t, stderr.String(), "failed to load config")
}

func TestFlags(t *testing.T) {
	ctx := context.Background()
	for _, run := range []func(context.Context, env, []string) int{take, list} {
		e, _, _ := testEnv(nil)
		assert.Equal(t, 0, run(ctx, e, []string{"-h"}))
		assert.Equal(t, 2, run(ctx, e, []string{"-nope"}))
		assert.Equal(t, 2, run(ctx, e, []string{"stray"}))
	}
	// 本物の入口も flag の誤りで 2 を返し、何も始めない。
	assert.Equal(t, 2, Take([]string{"-nope"}))
	assert.Equal(t, 2, List([]string{"-nope"}))
}

func TestList_Failures(t *testing.T) {
	ctx := context.Background()
	e, _, stderr := testEnv(nil)
	assert.Equal(t, 1, list(ctx, e, []string{"-config", filepath.Join(t.TempDir(), "absent.yml")}))
	assert.Contains(t, stderr.String(), "failed to load config")

	cfgPath := func(backupYAML string) string {
		path := filepath.Join(t.TempDir(), "default.yml")
		require.NoError(t, os.WriteFile(path, []byte("url: http://127.0.0.1:3000/\nport: 3000\ndb:\n  host: 127.0.0.1\n  port: 5432\n  db: x\n  user: x\n  pass: x\n"+backupYAML), 0o600))
		return path
	}
	e, _, stderr = testEnv(nil)
	assert.Equal(t, 1, list(ctx, e, []string{"-config", cfgPath("")}))
	assert.Contains(t, stderr.String(), "no backup: section")

	dir := markedDir(t)
	e, _, stderr = testEnv(nil)
	e.openStorage = func(config.BackupStorageOptions) (backup.Storage, error) { return failingList{}, nil }
	assert.Equal(t, 1, list(ctx, e, []string{"-config", cfgPath(dirBackupYAML(dir, ""))}))
	assert.Contains(t, stderr.String(), "list failed")
}

func TestList_UnreadableAndVerified(t *testing.T) {
	ctx := context.Background()
	dir := markedDir(t)
	st, err := backup.NewDirStorage(dir)
	require.NoError(t, err)
	put := func(key, body string) { require.NoError(t, st.Put(ctx, key, strings.NewReader(body))) }
	put(backup.Key("20261001T000000Z", backup.MetaFile), `{"formatVersion":99,"id":"20261001T000000Z"}`)
	put(backup.Key("20261002T000000Z", backup.DumpFileAge), "enc")
	put(backup.Key("20261002T000000Z", backup.MetaFile), `{"formatVersion":1,"id":"20261002T000000Z","elythiaVersion":"2.0.0","encrypted":true,"dumpFile":"dump.pgc.age","migrations":[{"table":"schema_migrations","version":5,"dirty":true}]}`)
	put(backup.Key("20261002T000000Z", backup.VerifyFile), `{"id":"20261002T000000Z","ok":false}`)
	put(backup.Key("20261003T000000Z", backup.MetaFile), `{"formatVersion":1,"id":"20261003T000000Z","migrations":[{"table":"schema_migrations","version":-1}]}`)
	put(backup.Key("20261003T000000Z", backup.VerifyFile), `{"id":"20261003T000000Z","ok":true}`)
	// meta.json だけあって dump が無い世代。
	put(backup.Key("20261004T000000Z", backup.MetaFile), `{"formatVersion":1,"id":"20261004T000000Z","dumpFile":"dump.pgc"}`)
	cfg := filepath.Join(t.TempDir(), "default.yml")
	require.NoError(t, os.WriteFile(cfg, []byte("url: http://127.0.0.1:3000/\nport: 3000\ndb:\n  host: 127.0.0.1\n  port: 5432\n  db: x\n  user: x\n  pass: x\n"+dirBackupYAML(dir, "")), 0o600))

	e, stdout, stderr := testEnv(nil)
	require.Equal(t, 0, list(ctx, e, []string{"-config", cfg}), stderr.String())
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	require.Len(t, lines, 5)
	assert.Regexp(t, `^20261004T000000Z\s+no-dump\s`, lines[4])
	assert.Regexp(t, `^20261001T000000Z\s+unreadable\s`, lines[1])
	assert.Regexp(t, `^20261002T000000Z\s+complete\s+\d+\s+true\s+failed\s+2\.0\.0\s+5 \(dirty\)$`, lines[2])
	assert.Regexp(t, `^20261003T000000Z\s+no-dump\s+\d+\s+false\s+ok\s+\S*\s*empty$`, lines[3])

	e, stdout, stderr = testEnv(nil)
	require.Equal(t, 0, list(ctx, e, []string{"-config", cfg, "-json"}), stderr.String())
	var listed []listedGeneration
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &listed))
	require.Len(t, listed, 4)
	assert.False(t, listed[3].Complete, "no dump")
	assert.Contains(t, listed[0].MetaError, "formatVersion 99")
	require.NotNil(t, listed[1].Verify)
	assert.False(t, listed[1].Verify.OK)
}

func TestMigrationLabel(t *testing.T) {
	assert.Equal(t, "missing", migrationLabel(backup.MigrationState{Missing: true}))
	assert.Equal(t, "empty", migrationLabel(backup.MigrationState{Version: backup.NilMigrationVersion}))
	assert.Equal(t, "7 (dirty)", migrationLabel(backup.MigrationState{Version: 7, Dirty: true}))
	assert.Equal(t, "7", migrationLabel(backup.MigrationState{Version: 7}))
}

type failingList struct{ backup.Storage }

func (failingList) List(context.Context, string) ([]backup.ObjectInfo, error) {
	return nil, fmt.Errorf("list failed")
}

// TestConfigExampleKeysDecode loads the backup: section as written in
// .config/default.yml.example (uncommented), so the camelCase keys reach the
// struct fields.
func TestConfigExampleKeysDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", ".config", "default.yml.example"))
	require.NoError(t, err)
	_, block, ok := strings.Cut(string(raw), "#backup:\n")
	require.True(t, ok, "the example has a backup: block")
	// 例に値を書くと、コメントを外しただけで公開の値をtokenにして動いてしまう。
	// 例は空のまま保ち、キーが読めることは差し替えた値で確かめる。
	require.Contains(t, block, "#    serviceToken: \"\"\n", "the example leaves serviceToken empty")
	block = strings.Replace(block, "#    serviceToken: \"\"\n", "#    serviceToken: \"example-token\"\n", 1)
	var lines []string
	lines = append(lines, "backup:")
	for _, l := range strings.Split(block, "\n") {
		if !strings.HasPrefix(l, "#  ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(l, "#"))
	}
	path := filepath.Join(t.TempDir(), "default.yml")
	body := "url: http://127.0.0.1:3000/\nport: 3000\ndb:\n  host: 127.0.0.1\n  port: 5432\n  db: x\n  user: x\n  pass: x\n" + strings.Join(lines, "\n") + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	require.NotNil(t, cfg.Backup)
	b := cfg.Backup
	assert.Equal(t, "s3", b.Storage.Type)
	assert.Equal(t, config.BackupS3Options{
		Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "elythia-backup", Prefix: "example-instance",
		AccessKey: "...", SecretKey: "...", ForcePathStyle: false,
	}, b.Storage.S3)
	assert.Equal(t, "/backup", b.Storage.Dir.Path)
	assert.Equal(t, []string{"age1..."}, b.Encryption.Recipients)
	assert.Equal(t, "/run/secrets/backup-identity.txt", b.Encryption.IdentityFile)
	assert.Equal(t, config.BackupScheduleOptions{
		Interval: "24h", At: "04:00", Keep: 7, Verify: true, DelayAfter: "36h", Listen: "127.0.0.1:3010",
	}, b.Schedule)
	assert.Equal(t, config.BackupNotifyOptions{WebhookURL: "https://hooks.example.com/...", Format: "generic"}, b.Notify)
	assert.Equal(t, "example-token", b.Server.ServiceToken)
}

// markedDir returns a new directory that a DirStorage accepts.
func markedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, backup.CreateDirMarker(dir))
	return dir
}
