package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
)

// テストの PostgreSQL は、本番と同じ postgres:18-alpine をコンテナで立てる。
// ホストの pg_dump は 15 で、18 のサーバーからは取れないので、pg_dump / pg_restore
// もコンテナの中で動かす (dockerExecRunner)。
const (
	postgresImage = "postgres:18-alpine"
	pgUser        = "test"
	pgPassword    = "test"
	// minioImage は MinIO のコミュニティ版。公式の minio/minio は Docker Hub から
	// 消え (2026-10 時点で repository が存在しない)、quay.io も認証を要求するので、
	// 取得できる fork を tag と digest で固定する。
	minioImage  = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"
	minioUser   = "minioadmin"
	minioSecret = "minioadmin-secret"
	minioBucket = "backups"
)

var (
	pgOnce sync.Once
	pgC    *tcpostgres.PostgresContainer
	pgErr  error

	minioOnce     sync.Once
	minioC        testcontainers.Container
	minioEndpoint string
	minioErr      error
)

func TestMain(m *testing.M) {
	code := m.Run()
	ctx := context.Background()
	if pgC != nil {
		_ = pgC.Terminate(ctx)
	}
	if minioC != nil {
		_ = minioC.Terminate(ctx)
	}
	os.Exit(code)
}

// pgEnv is one fresh database in the shared PostgreSQL container.
type pgEnv struct {
	container string
	db        string
	// url is how the test process connects (through the mapped port).
	url string
	// dump is how programs inside the container connect.
	dump   DumpConn
	runner dockerExecRunner
}

func startPostgres(t *testing.T) *tcpostgres.PostgresContainer {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	pgOnce.Do(func() {
		pgC, pgErr = tcpostgres.Run(context.Background(), postgresImage,
			tcpostgres.WithDatabase("elythia"),
			tcpostgres.WithUsername(pgUser),
			tcpostgres.WithPassword(pgPassword),
			// verify の使い捨てのサーバーを同じ container の中に立て、このプロセスから
			// 繋ぐための port (verify_env_test.go の sandboxPort)。
			testcontainers.WithExposedPorts(sandboxPort+"/tcp"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
	})
	require.NoError(t, pgErr)
	return pgC
}

// newDatabase creates an empty database for one test.
func newDatabase(t *testing.T) *pgEnv {
	t.Helper()
	c := startPostgres(t)
	ctx := context.Background()
	admin, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	name := "t_" + randomHex(t, 6)
	conn, err := pgx.Connect(ctx, admin)
	require.NoError(t, err)
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)

	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	return &pgEnv{
		container: c.GetContainerID(),
		db:        name,
		url:       fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", pgUser, pgPassword, host, port.Port(), name),
		dump:      DumpConn{URI: "postgresql://" + pgUser + "@localhost:5432/" + name + "?sslmode=disable", Password: pgPassword},
		runner:    dockerExecRunner{container: c.GetContainerID()},
	}
}

func (p *pgEnv) connect(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), p.url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func (p *pgEnv) exec(t *testing.T, sqls ...string) {
	t.Helper()
	conn := p.connect(t)
	for _, s := range sqls {
		_, err := conn.Exec(context.Background(), s)
		require.NoError(t, err, s)
	}
}

// seedSimple creates tables covering the cases CountRows has to handle: two
// schemas, a quoted name, a partitioned table, the core bookkeeping table
// (the local one is left missing) and per-database settings.
func (p *pgEnv) seedSimple(t *testing.T) {
	t.Helper()
	p.exec(t,
		`CREATE EXTENSION pg_trgm`,
		`CREATE TABLE note (id bigserial PRIMARY KEY, body text NOT NULL)`,
		`INSERT INTO note (body) SELECT 'note ' || g FROM generate_series(1, 1000) g`,
		`CREATE SCHEMA other`,
		`CREATE TABLE other."Weird Name" (x int)`,
		`INSERT INTO other."Weird Name" VALUES (1), (2), (3)`,
		`CREATE TABLE ev (id int, k int) PARTITION BY RANGE (k)`,
		`CREATE TABLE ev_p1 PARTITION OF ev FOR VALUES FROM (0) TO (10)`,
		`CREATE TABLE ev_p2 PARTITION OF ev FOR VALUES FROM (10) TO (20)`,
		`INSERT INTO ev SELECT g, g FROM generate_series(0, 14) g`,
		`CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`INSERT INTO schema_migrations VALUES (121, false)`,
		`ALTER DATABASE `+p.db+` SET work_mem = '8MB'`,
		`ALTER DATABASE `+p.db+` SET statement_timeout = '5min'`,
		// ロール単位の設定は DB 単位の設定ではないので、記録しない。
		`ALTER ROLE `+pgUser+` IN DATABASE `+p.db+` SET lock_timeout = '1s'`,
	)
}

// restore loads a plaintext custom-format dump into a new database inside the
// container and returns that database.
func (p *pgEnv) restore(t *testing.T, dump []byte) *pgEnv {
	t.Helper()
	target := newDatabase(t)
	cmd := p.runner.Command(context.Background(), []string{"PGPASSWORD=" + pgPassword},
		"pg_restore", "--no-owner", "--exit-on-error", "--dbname="+target.dump.URI)
	cmd.Stdin = bytes.NewReader(dump)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return target
}

// pgRestoreList runs pg_restore --list on a plaintext dump.
func (p *pgEnv) pgRestoreList(t *testing.T, dump []byte) string {
	t.Helper()
	cmd := p.runner.Command(context.Background(), nil, "pg_restore", "--list")
	cmd.Stdin = bytes.NewReader(dump)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	require.NoError(t, cmd.Run(), stderr.String())
	return stdout.String()
}

func (p *pgEnv) countRows(t *testing.T) map[string]int64 {
	t.Helper()
	counts, err := CountRows(context.Background(), p.connect(t))
	require.NoError(t, err)
	return counts
}

func (p *pgEnv) takeOptions(st Storage) TakeOptions {
	return TakeOptions{Storage: st, DatabaseURL: p.url, Dump: p.dump, Runner: p.runner}
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
	a = append(a, r.container, name)
	a = append(a, args...)
	return exec.CommandContext(ctx, "docker", a...)
}

func startMinIO(t *testing.T) string {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	minioOnce.Do(func() {
		ctx := context.Background()
		minioC, minioErr = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        minioImage,
				ExposedPorts: []string{"9000/tcp"},
				Env:          map[string]string{"MINIO_ROOT_USER": minioUser, "MINIO_ROOT_PASSWORD": minioSecret},
				Cmd:          []string{"server", "/data"},
				WaitingFor:   wait.ForHTTP("/minio/health/live").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
			},
			Started: true,
		})
		if minioErr != nil {
			return
		}
		var host string
		if host, minioErr = minioC.Host(ctx); minioErr != nil {
			return
		}
		port, err := minioC.MappedPort(ctx, "9000/tcp")
		if err != nil {
			minioErr = err
			return
		}
		minioEndpoint = "http://" + host + ":" + port.Port()
		st, err := NewS3Storage(minioOptions(""))
		if err != nil {
			minioErr = err
			return
		}
		_, minioErr = st.client.(*s3.Client).CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(minioBucket)})
	})
	require.NoError(t, minioErr)
	return minioEndpoint
}

func minioOptions(prefix string) config.BackupS3Options {
	return config.BackupS3Options{
		Endpoint:       minioEndpoint,
		Region:         "us-east-1",
		Bucket:         minioBucket,
		Prefix:         prefix,
		AccessKey:      minioUser,
		SecretKey:      minioSecret,
		ForcePathStyle: true,
	}
}

// newS3Storage returns an S3Storage on MinIO under a prefix unique to t.
func newS3Storage(t *testing.T) *S3Storage {
	t.Helper()
	startMinIO(t)
	prefix := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + randomHex(t, 4)
	st, err := NewS3Storage(minioOptions(prefix))
	require.NoError(t, err)
	return st
}

// rawS3 reads an object straight from the bucket, bypassing S3Storage.
func rawS3Get(t *testing.T, st *S3Storage, key string) []byte {
	t.Helper()
	out, err := st.client.(*s3.Client).GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(minioBucket),
		Key:    aws.String(st.prefix + key),
	})
	require.NoError(t, err)
	defer out.Body.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(out.Body)
	require.NoError(t, err)
	return buf.Bytes()
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return hex.EncodeToString(b)
}

// markedDir returns a new directory that a DirStorage accepts.
func markedDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, CreateDirMarker(dir))
	return dir
}
