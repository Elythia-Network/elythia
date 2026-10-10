package backupadmin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// minioImage is pinned so that the test does not change under us.
//
// Docker Hub の minio/minio は repository が無くなったので、#3458 と同じ
// コミュニティ版を digest で固定する。
const minioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"

const (
	minioUser   = "minioadmin"
	minioSecret = "minioadmin-secret"
	minioBucket = "backups"
	minioPrefix = "elythia"
)

// startMinio starts MinIO with an empty bucket and returns its endpoint and a
// raw client that bypasses backup.S3Storage.
func startMinio(t *testing.T) (string, *s3.Client) {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": minioUser, "MINIO_ROOT_PASSWORD": minioSecret},
			ExposedPorts: []string{"9000/tcp"},
			WaitingFor:   wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "9000/tcp")
	require.NoError(t, err)
	endpoint := fmt.Sprintf("http://%s:%s", host, port.Port())
	client := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(minioUser, minioSecret, ""),
	})
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(minioBucket)})
	require.NoError(t, err)
	return endpoint, client
}

// pgSource is a PostgreSQL 18 database that backup.Take can dump.
type pgSource struct {
	url    string
	dump   backup.DumpConn
	runner dockerExecRunner
}

// startPostgres starts postgres:18-alpine with a small table.
//
// ホストの pg_dump は 18 のサーバーから取れないので、#3458 のテストと同じく
// pg_dump をコンテナの中で動かす。
func startPostgres(t *testing.T) *pgSource {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	ctx := context.Background()
	const user, pass, db = "test", "test", "elythia"
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine",
		tcpostgres.WithDatabase(db), tcpostgres.WithUsername(user), tcpostgres.WithPassword(pass),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })
	url, err := c.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	conn, err := pgx.Connect(ctx, url)
	require.NoError(t, err)
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE TABLE note (id bigserial PRIMARY KEY, body text NOT NULL)`,
		`INSERT INTO note (body) SELECT 'note ' || g FROM generate_series(1, 200) g`,
		`CREATE TABLE schema_migrations (version bigint NOT NULL PRIMARY KEY, dirty boolean NOT NULL)`,
		`INSERT INTO schema_migrations VALUES (121, false)`,
	} {
		_, err := conn.Exec(ctx, q)
		require.NoError(t, err, q)
	}
	return &pgSource{
		url:    url,
		dump:   backup.DumpConn{URI: "postgresql://" + user + "@localhost:5432/" + db + "?sslmode=disable", Password: pass},
		runner: dockerExecRunner{container: c.GetContainerID()},
	}
}

func (p *pgSource) take(t *testing.T, st backup.Storage, at time.Time) *backup.Meta {
	t.Helper()
	m, err := backup.Take(context.Background(), backup.TakeOptions{
		Storage: st, DatabaseURL: p.url, Dump: p.dump, Runner: p.runner,
		Now: func() time.Time { return at },
	})
	require.NoError(t, err)
	return m
}

// dockerExecRunner runs programs inside a container with docker exec.
type dockerExecRunner struct{ container string }

func (r dockerExecRunner) Command(ctx context.Context, env []string, name string, args ...string) *exec.Cmd {
	a := []string{"exec", "-i"}
	for _, e := range env {
		a = append(a, "-e", e)
	}
	a = append(append(a, r.container, name), args...)
	return exec.CommandContext(ctx, "docker", a...)
}

// TestMinio_TakenGenerationListDownloadDelete takes a real backup with
// backup.Take into MinIO and reads it back through the storage the main
// server opens (backup.OpenStorage).
func TestMinio_TakenGenerationListDownloadDelete(t *testing.T) {
	endpoint, client := startMinio(t)
	src := startPostgres(t)
	ctx := context.Background()

	st, err := backup.OpenStorage(config.BackupStorageOptions{Type: backup.StorageTypeS3, S3: config.BackupS3Options{
		Endpoint: endpoint, Region: "us-east-1", Bucket: minioBucket, Prefix: minioPrefix,
		AccessKey: minioUser, SecretKey: minioSecret, ForcePathStyle: true,
	}})
	require.NoError(t, err)

	taken := src.take(t, st, time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC))
	require.Equal(t, gen2, taken.ID)
	putGeneration(t, st, gen1, bytes.Repeat([]byte("a"), 1500), true)
	putJSON(t, st, backup.Key(gen2, backup.VerifyFile), backup.VerifyResult{ID: gen2, OK: true})
	require.NoError(t, st.Put(ctx, backup.Key(gen3, backup.DumpFile), strings.NewReader("partial")))
	// prefix の外のもの。この保存先の使用量には入らない。
	_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(minioBucket), Key: aws.String("other/x"), Body: strings.NewReader("zzz")})
	require.NoError(t, err)

	svc := NewService(Options{StorageType: "s3", Storage: st, PricePerGBMonth: 0.015})
	ov, err := svc.List(ctx)
	require.NoError(t, err)

	// **保存先の実際の中身と突き合わせる。** MinIO に直接 ListObjectsV2 を
	// 投げて、prefix の下の合計と世代ごとの合計を数える。
	raw, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(minioBucket), Prefix: aws.String(minioPrefix + "/")})
	require.NoError(t, err)
	var total int64
	perGen := map[string]int64{}
	for _, o := range raw.Contents {
		total += aws.ToInt64(o.Size)
		perGen[backup.GenerationIDFromKey(strings.TrimPrefix(aws.ToString(o.Key), minioPrefix+"/"))] += aws.ToInt64(o.Size)
	}
	assert.Equal(t, total, ov.Usage.TotalBytes)
	assert.Equal(t, len(raw.Contents), ov.Usage.ObjectCount)
	assert.Equal(t, 3, ov.Usage.GenerationCount)
	require.Len(t, ov.Generations, 3)
	for _, g := range ov.Generations {
		assert.Equal(t, perGen[g.ID], g.Size, g.ID)
	}
	assert.False(t, ov.Generations[0].Complete, "gen3 has no meta.json")

	// backup.Take が書いたメタ情報がそのまま一覧に出る。
	g := ov.Generations[1]
	require.Equal(t, gen2, g.ID)
	assert.True(t, g.Complete)
	assert.False(t, g.Encrypted)
	assert.Equal(t, taken.DumpSize, g.DumpSize)
	assert.Equal(t, config.MkGoVersion, g.ElythiaVersion)
	assert.Regexp(t, `^18\.`, g.PostgresVersion)
	assert.Equal(t, "elythia", g.Database)
	require.NotEmpty(t, g.Migrations)
	assert.Equal(t, int64(121), g.Migrations[0].Version)
	require.NotNil(t, g.Verify)
	assert.True(t, g.Verify.OK)
	assert.True(t, ov.Generations[2].Encrypted)

	// 署名付き URL: 期限内は取れて、保存先が attachment の名前を付ける。期限を
	// 過ぎると拒否される。
	short := NewService(Options{Storage: st, DownloadTTL: 2 * time.Second})
	d, err := short.Download(ctx, gen2, "admin1")
	require.NoError(t, err)
	assert.Equal(t, "storage", d.Via)
	res, err := http.Get(d.URL)
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, `attachment; filename="`+gen2+`-dump.pgc"`, res.Header.Get("Content-Disposition"))
	assert.Equal(t, d.FileName, gen2+"-dump.pgc", "the server-side download uses the same name")
	assert.Equal(t, taken.DumpSize, int64(len(body)))
	assert.Equal(t, "PGDMP", string(body[:5]))

	time.Sleep(3500 * time.Millisecond)
	res, err = http.Get(d.URL)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	assert.Equal(t, http.StatusForbidden, res.StatusCode, "an expired presigned URL must be refused")

	// 消すと、保存先からも消える。
	freed, err := svc.Delete(ctx, gen2)
	require.NoError(t, err)
	assert.Equal(t, perGen[gen2], freed)
	left, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(minioBucket), Prefix: aws.String(minioPrefix + "/" + backup.Key(gen2, ""))})
	require.NoError(t, err)
	assert.Empty(t, left.Contents)
	ov, err = svc.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, total-perGen[gen2], ov.Usage.TotalBytes)
	assert.Equal(t, 2, ov.Usage.GenerationCount)
}

// TestDirStorage_TakenGenerationServerDownload takes a real backup into a
// directory storage (with its marker) and hands it out through the main
// server's download token.
func TestDirStorage_TakenGenerationServerDownload(t *testing.T) {
	src := startPostgres(t)
	ctx := context.Background()
	root := t.TempDir()
	require.NoError(t, backup.CreateDirMarker(root))
	st, err := backup.OpenStorage(config.BackupStorageOptions{Type: backup.StorageTypeDir, Dir: config.BackupDirectoryOptions{Path: root}})
	require.NoError(t, err)
	_, ok := st.(backup.Presigner)
	require.False(t, ok, "a directory storage is served by the main server")

	taken := src.take(t, st, time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC))
	require.Equal(t, gen1, taken.ID)

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	svc := NewService(Options{StorageType: "dir", Storage: st, Tokens: NewRedisTokens(rdb), DownloadURLBase: "https://example.com/backup-download?token="})

	ov, err := svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, ov.Generations, 1)
	assert.True(t, ov.Generations[0].Complete)
	// 目印のファイルは保存先の中身ではないので、使用量に数えない。
	var onDisk int64
	require.NoError(t, filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && fi.Name() != backup.DirMarkerFile {
			onDisk += fi.Size()
		}
		return err
	}))
	assert.Equal(t, onDisk, ov.Usage.TotalBytes)

	d, err := svc.Download(ctx, gen1, "admin1")
	require.NoError(t, err)
	assert.Equal(t, "server", d.Via)
	assert.Equal(t, gen1+"-dump.pgc", d.FileName)
	token := strings.TrimPrefix(d.URL, "https://example.com/backup-download?token=")
	grant, rc, info, err := svc.Open(ctx, token)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	_ = rc.Close()
	want, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(backup.Key(gen1, backup.DumpFile))))
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, taken.DumpSize, info.Size)
	assert.Equal(t, "admin1", grant.UserID)

	freed, err := svc.Delete(ctx, gen1)
	require.NoError(t, err)
	assert.Equal(t, onDisk, freed)
	_, err = os.Stat(filepath.Join(root, "generations", gen1))
	assert.ErrorIs(t, err, os.ErrNotExist, "the generation directory is gone")
	_, err = os.Stat(filepath.Join(root, backup.DirMarkerFile))
	assert.NoError(t, err, "the marker stays")
}
