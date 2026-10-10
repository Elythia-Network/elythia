package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
	gomigrate "github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// このファイルは restore のテストの道具。名前は rs で始める (同じパッケージに
// 他の段階のテストが入るので、衝突させない)。

const (
	rsCoreMigrations = "../../migration"
	rsSuperUser      = "test"
	rsSuperPass      = "test"
)

// rsStorage is an in-memory Storage.
type rsStorage struct {
	mu   sync.Mutex
	objs map[string][]byte
	// getErr, when set, is returned by Get for that key.
	getErr map[string]error
}

func newRSStorage() *rsStorage {
	return &rsStorage{objs: map[string][]byte{}, getErr: map[string]error{}}
}

func (s *rsStorage) Put(_ context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = b
	return nil
}

func (s *rsStorage) Get(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.getErr[key]; err != nil {
		return nil, err
	}
	b, ok := s.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (s *rsStorage) Stat(_ context.Context, key string) (ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objs[key]
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{Key: key, Size: int64(len(b))}, nil
}

func (s *rsStorage) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ObjectInfo
	for k, b := range s.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, ObjectInfo{Key: k, Size: int64(len(b))})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *rsStorage) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objs, key)
	return nil
}

func (s *rsStorage) putJSON(t *testing.T, key string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objs[key] = b
}

// rsPG is a PostgreSQL container. Go connects through the mapped port;
// pg_dump / pg_restore run inside the container (docker exec) and reach the
// server at localhost:5432, like the backup image does.
type rsPG struct {
	c    *tcpostgres.PostgresContainer
	id   string
	host string
	port int
}

func rsStartPG(t *testing.T, image string, opts ...testcontainers.ContainerCustomizer) *rsPG {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker CLI is not available")
	}
	ctx := context.Background()
	opts = append([]testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername(rsSuperUser),
		tcpostgres.WithPassword(rsSuperPass),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60 * time.Second)),
	}, opts...)
	c, err := tcpostgres.Run(ctx, image, opts...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	require.NoError(t, err)
	mapped, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	return &rsPG{c: c, id: c.GetContainerID(), host: host, port: int(mapped.Num())}
}

func (p *rsPG) url(scheme, user, pass, db string) string {
	return fmt.Sprintf("%s://%s:%s@%s:%d/%s?sslmode=disable", scheme, user, pass, p.host, p.port, db)
}

func (p *rsPG) connect(t *testing.T, user, pass, db string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), p.url("postgres", user, pass, db))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func (p *rsPG) exec(t *testing.T, db string, sqls ...string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), p.url("postgres", rsSuperUser, rsSuperPass, db))
	require.NoError(t, err)
	defer conn.Close(context.Background())
	for _, s := range sqls {
		_, err := conn.Exec(context.Background(), s)
		require.NoError(t, err, s)
	}
}

func (p *rsPG) restoreConn(user, pass string) RestoreConn {
	return RestoreConn{
		SQL:     func(db string) string { return p.url("postgres", user, pass, db) },
		Migrate: func(db string) string { return p.url("pgx5", user, pass, db) },
		Tool: func(db string) (string, []string) {
			return fmt.Sprintf("host=localhost port=5432 user=%s dbname=%s", user, db), []string{"PGPASSWORD=" + pass}
		},
	}
}

// rsDockerExec runs the program inside the container.
func rsDockerExec(containerID string) RestoreExecFunc {
	return func(ctx context.Context, program string, args, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
		argv := []string{"exec", "-i"}
		for _, e := range env {
			argv = append(argv, "-e", e)
		}
		argv = append(argv, containerID, program)
		argv = append(argv, args...)
		cmd := exec.CommandContext(ctx, "docker", argv...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
		return cmd.Run()
	}
}

func (p *rsPG) restorer(storage Storage, user, pass string) *Restorer {
	return &Restorer{
		Storage:           storage,
		Conn:              p.restoreConn(user, pass),
		Exec:              rsDockerExec(p.id),
		CoreMigrationsDir: rsCoreMigrations,
		Now:               func() time.Time { return time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC) },
	}
}

// migrate applies the bundled migrations to db as user.
func (p *rsPG) migrate(t *testing.T, user, pass, db string) {
	t.Helper()
	m, err := gomigrate.New("file://"+rsCoreMigrations, p.url("pgx5", user, pass, db))
	require.NoError(t, err)
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, gomigrate.ErrNoChange) {
		require.NoError(t, err)
	}
}

// rsBackupOptions changes how rsTakeBackup builds a generation.
type rsBackupOptions struct {
	// dumper runs pg_dump (defaults to src); dumpHost is the server
	// address seen from inside dumper (defaults to localhost).
	dumper   *rsPG
	dumpHost string
	encrypt  age.Recipient
	// mutateMeta edits meta.json after Take stored it.
	mutateMeta func(*Meta)
	verified   bool
}

// rsTakeBackup takes generation id of db with Take (#3458), running pg_dump
// inside the dumper container, and returns its Meta.
func rsTakeBackup(t *testing.T, src *rsPG, storage Storage, db, id string, o rsBackupOptions) Meta {
	t.Helper()
	ctx := context.Background()
	dumper, host := o.dumper, o.dumpHost
	if dumper == nil {
		dumper = src
	}
	if host == "" {
		host = "localhost"
	}
	at, err := IDTime(id)
	require.NoError(t, err)
	opts := TakeOptions{
		Storage:     storage,
		DatabaseURL: src.url("postgres", rsSuperUser, rsSuperPass, db),
		Dump:        DumpConn{URI: fmt.Sprintf("postgresql://%s@%s:5432/%s?sslmode=disable", rsSuperUser, host, db), Password: rsSuperPass},
		Runner:      dockerExecRunner{container: dumper.id},
		Now:         func() time.Time { return at },
	}
	if o.encrypt != nil {
		opts.Recipients = []age.Recipient{o.encrypt}
	}
	meta, err := Take(ctx, opts)
	require.NoError(t, err)
	if o.mutateMeta != nil {
		o.mutateMeta(meta)
		rsPutJSON(t, storage, Key(id, MetaFile), meta)
	}
	if o.verified {
		rsPutJSON(t, storage, Key(id, VerifyFile), VerifyResult{ID: id, OK: true})
	}
	// Take が閉じた接続は、サーバーの側で少し遅れて消える。戻すときに「接続が
	// 残っている」で止まらないよう、消えるまで待つ。
	conn := src.connect(t, rsSuperUser, rsSuperPass, "postgres")
	defer conn.Close(ctx)
	require.Eventually(t, func() bool {
		var n int64
		err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND backend_type = 'client backend'`, db).Scan(&n)
		return err == nil && n == 0
	}, 10*time.Second, 50*time.Millisecond)
	return *meta
}

func rsPutJSON(t *testing.T, storage Storage, key string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	require.NoError(t, storage.Put(context.Background(), key, bytes.NewReader(b)))
}

func rsCount(t *testing.T, conn *pgx.Conn, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	require.NoError(t, conn.QueryRow(context.Background(), sql, args...).Scan(&n))
	return n
}

// rsRehash makes meta.json of id match the stored (unencrypted) dump again.
func rsRehash(t *testing.T, storage *rsStorage, id string) {
	t.Helper()
	var m Meta
	require.NoError(t, json.Unmarshal(storage.objs[Key(id, MetaFile)], &m))
	b := storage.objs[Key(id, m.DumpFile)]
	sum := sha256.Sum256(b)
	m.DumpSHA256, m.PlainSHA256, m.DumpSize = hex.EncodeToString(sum[:]), hex.EncodeToString(sum[:]), int64(len(b))
	storage.putJSON(t, Key(id, MetaFile), m)
}

func rsSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
