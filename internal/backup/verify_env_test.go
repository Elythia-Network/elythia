package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/elythia-network/elythia/internal/cli/migrate"
	"github.com/elythia-network/elythia/internal/testutil"
)

// memStorage is an in-memory Storage for the verify tests.
//
// #3458 のテストは DirStorage と brokenStorage を使うが、verify のテストは
// 「verify.json 以外を書かない」ことを Put の記録で、保存先の読み出しの失敗を
// key ごとの Get のエラーで試すので、それができるこの形を使う。
type memStorage struct {
	mu      sync.Mutex
	objects map[string][]byte
	// getErr / putErr, when set, are returned for keys with that suffix.
	getErr map[string]error
	putErr map[string]error
	// failAfter makes Get of keys with that suffix fail after that many bytes,
	// like a connection lost in the middle of a download.
	failAfter map[string]int
	puts      []string
}

func newMemStorage() *memStorage { return &memStorage{objects: map[string][]byte{}} }

func (m *memStorage) Put(_ context.Context, key string, r io.Reader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for suffix, err := range m.putErr {
		if strings.HasSuffix(key, suffix) {
			return err
		}
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.objects[key] = b
	m.puts = append(m.puts, key)
	return nil
}

func (m *memStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for suffix, err := range m.getErr {
		if strings.HasSuffix(key, suffix) {
			return nil, err
		}
	}
	b, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	for suffix, n := range m.failAfter {
		if strings.HasSuffix(key, suffix) {
			return io.NopCloser(io.MultiReader(bytes.NewReader(b[:n]), iotest.ErrReader(errors.New("connection reset by peer")))), nil
		}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStorage) Stat(_ context.Context, key string) (ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (m *memStorage) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.getErr["<list>"]; err != nil {
		return nil, err
	}
	var out []ObjectInfo
	for k, b := range m.objects {
		if strings.HasPrefix(k, prefix) {
			out = append(out, ObjectInfo{Key: k, Size: int64(len(b))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (m *memStorage) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// sandboxPort is the port of the throwaway server inside the container.
const sandboxPort = "5433"

// containerSandbox starts the throwaway server inside the test container.
//
// ホストの pg_* は 15 で 18 のサーバーを扱えないので、postgres:18-alpine の中で
// 動かす。プログラムは本番と同じ LocalSandbox.Run に、docker exec で中へ入る
// Runner を渡して動かす。initdb は root では動かないので postgres で動かす。
type containerSandbox struct {
	run  LocalSandbox
	host string
	port int

	mu   sync.Mutex
	dirs []string
}

func newContainerSandbox(container, host string, port int) *containerSandbox {
	return &containerSandbox{
		run:  LocalSandbox{Runner: dockerExecRunner{container: container, user: "postgres"}},
		host: host,
		port: port,
	}
}

func (s *containerSandbox) Run(ctx context.Context, cmd SandboxCmd) error { return s.run.Run(ctx, cmd) }

// sh runs a shell script inside the container and returns its stdout.
func (s *containerSandbox) sh(ctx context.Context, script string, args ...string) (string, error) {
	var out bytes.Buffer
	err := s.Run(ctx, SandboxCmd{Program: "sh", Args: append([]string{"-c", script, "sh"}, args...), Stdout: &out})
	return out.String(), err
}

func (s *containerSandbox) MkdirTemp(ctx context.Context) (string, error) {
	// 外 (テストのプロセス) から TCP で繋ぐので、hba は全ての接続を trust にする。
	// 待ち受けは container の中だけで、ホストへは testcontainers の割り当てた port だけが出る。
	out, err := s.sh(ctx, `d=$(mktemp -d /tmp/verify-XXXXXX) && mkdir "$d/sock" && printf 'local all all trust\nhost all all all trust\n' >"$d/pg_hba.conf" && printf %s "$d"`)
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(out)
	s.mu.Lock()
	s.dirs = append(s.dirs, dir)
	s.mu.Unlock()
	return dir, nil
}

func (s *containerSandbox) RemoveAll(ctx context.Context, dir string) error {
	_, err := s.sh(ctx, `rm -rf "$1"`, dir)
	return err
}

func (s *containerSandbox) Server(dir string) SandboxServer {
	local := LocalSandbox{}.Server(dir)
	opts := []string{"-c", "listen_addresses=0.0.0.0", "-p", sandboxPort, "-c", "hba_file=" + dir + "/pg_hba.conf"}
	// unix socket の置き場所は本番の LocalSandbox と同じ形にする。
	opts = append(opts, local.Options[2:]...)
	p, _ := strconv.Atoi(sandboxPort)
	return SandboxServer{Options: opts, ToolHost: local.ToolHost, ToolPort: p, Host: s.host, Port: s.port}
}

// assertNoLeftovers checks that no throwaway server or directory remains.
func (s *containerSandbox) assertNoLeftovers(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	s.mu.Lock()
	dirs := append([]string(nil), s.dirs...)
	s.mu.Unlock()
	require.NotEmpty(t, dirs, "the sandbox was never used")
	for _, d := range dirs {
		out, err := s.sh(ctx, `test -e "$1" && echo remains || true`, d)
		require.NoError(t, err)
		require.Empty(t, strings.TrimSpace(out), "%s remains", d)
	}
	out, err := s.sh(ctx, `pg_isready -h 127.0.0.1 -p "$1" >/dev/null && echo accepting || true`, sandboxPort)
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(out), "the throwaway server still accepts connections")
	// [/] は pgrep を呼ぶ sh 自身の引数に一致させないため。
	out, err = s.sh(ctx, `pgrep -f '[/]tmp/verify-' || true`)
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(out), "a process of the throwaway server remains")
}

// verifyEnv is a source database with Elythia's schema plus a fixture table,
// and the sandbox, both in the shared PostgreSQL container (harness_test.go).
type verifyEnv struct {
	db      *gorm.DB
	dbName  string
	url     string
	sandbox *containerSandbox
	bundled BundledMigrations
}

var (
	envOnce sync.Once
	envVal  *verifyEnv
	envErr  error
)

// fixtureRows is the number of rows in the fixture table.
const fixtureRows = 3

func getVerifyEnv(t *testing.T) *verifyEnv {
	t.Helper()
	c := startPostgres(t)
	envOnce.Do(func() { envVal, envErr = startVerifyEnv(c) })
	require.NoError(t, envErr)
	return envVal
}

// bundledMigrations reads the latest core version shipped in migration/.
func bundledMigrations() (BundledMigrations, error) {
	latest, _, err := migrate.LatestVersion(filepath.Join("..", "..", migrate.CoreDir))
	if err != nil || latest == 0 {
		return BundledMigrations{}, fmt.Errorf("bundled migrations: %d, %v", latest, err)
	}
	return BundledMigrations{Core: int64(latest)}, nil
}

// migrateDatabase applies Elythia's schema to url and sets the core tracking
// table to the bundled version, as golang-migrate leaves it.
func migrateDatabase(url string, bundled BundledMigrations) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(url), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		return nil, err
	}
	testutil.ApplyMigrations(db)
	for _, q := range []string{
		// 000001 が管理表を作るので、golang-migrate が当てた後と同じ 1 行にする。
		`CREATE TABLE IF NOT EXISTS schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`DELETE FROM schema_migrations`,
		fmt.Sprintf(`INSERT INTO schema_migrations (version, dirty) VALUES (%d, false)`, bundled.Core),
	} {
		if err := db.Exec(q).Error; err != nil {
			closeGorm(db)
			return nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	return db, nil
}

func startVerifyEnv(c *tcpostgres.PostgresContainer) (*verifyEnv, error) {
	ctx := context.Background()
	admin, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return nil, err
	}
	env := &verifyEnv{dbName: "verify_src"}
	if err := execOnce(ctx, admin, "CREATE DATABASE "+env.dbName); err != nil {
		return nil, err
	}
	host, err := c.Host(ctx)
	if err != nil {
		return nil, err
	}
	pgPort, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return nil, err
	}
	env.url = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", pgUser, pgPassword, host, pgPort.Port(), env.dbName)
	if env.bundled, err = bundledMigrations(); err != nil {
		return nil, err
	}
	if env.db, err = migrateDatabase(env.url, env.bundled); err != nil {
		return nil, err
	}
	for _, q := range []string{
		`CREATE TABLE verify_fixture (id int PRIMARY KEY, body text)`,
		fmt.Sprintf(`INSERT INTO verify_fixture SELECT g, 'row ' || g FROM generate_series(1, %d) g`, fixtureRows),
	} {
		if err := env.db.Exec(q).Error; err != nil {
			return nil, fmt.Errorf("%s: %w", q, err)
		}
	}
	port, err := c.MappedPort(ctx, sandboxPort+"/tcp")
	if err != nil {
		return nil, err
	}
	env.sandbox = newContainerSandbox(c.GetContainerID(), host, int(port.Num()))
	return env, nil
}

func execOnce(ctx context.Context, url, sql string) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, sql)
	return err
}

// setMigration updates the source tracking table for the next dump. The
// caller restores it with t.Cleanup.
func (e *verifyEnv) setMigration(t *testing.T, version int64, dirty bool) {
	t.Helper()
	require.NoError(t, e.db.Exec(`UPDATE schema_migrations SET version = ?, dirty = ?`, version, dirty).Error)
	t.Cleanup(func() {
		require.NoError(t, e.db.Exec(`UPDATE schema_migrations SET version = ?, dirty = false`, e.bundled.Core).Error)
	})
}

// dump runs pg_dump inside the container and returns the custom-format bytes.
func (e *verifyEnv) dump(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	require.NoError(t, e.sandbox.Run(context.Background(), SandboxCmd{
		Program: "pg_dump", Args: []string{"-Fc", "-U", pgUser, "-d", e.dbName}, Stdout: &out,
	}))
	require.NotZero(t, out.Len())
	return out.Bytes()
}

// rowCounts counts the source with CountRows, as Take records it.
func (e *verifyEnv) rowCounts(t *testing.T) map[string]int64 {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), e.url)
	require.NoError(t, err)
	defer conn.Close(context.Background())
	out, err := CountRows(context.Background(), conn)
	require.NoError(t, err)
	require.Equal(t, int64(fixtureRows), out["public.verify_fixture"])
	return out
}

// buildMeta assembles meta.json for an unencrypted dump.
func (e *verifyEnv) buildMeta(t *testing.T, id string, dump []byte, version int64, dirty bool) Meta {
	t.Helper()
	sum := sha256.Sum256(dump)
	return Meta{
		FormatVersion: MetaFormatVersion,
		ID:            id,
		CreatedAt:     time.Now().UTC(),
		Database:      e.dbName,
		Migrations: []MigrationState{
			{Table: "schema_migrations", Version: version, Dirty: dirty},
			{Table: "schema_migrations_local", Missing: true},
		},
		RowCounts:  e.rowCounts(t),
		DumpFile:   DumpFile,
		DumpSize:   int64(len(dump)),
		DumpSHA256: hex.EncodeToString(sum[:]),
	}
}
