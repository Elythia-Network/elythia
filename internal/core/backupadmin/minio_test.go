package backupadmin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// minioImage is pinned so that the test does not change under us.
//
// Docker Hub の minio/minio は repository が無くなったので、#3458 と同じ
// コミュニティ版を digest で固定する。
const minioImage = "pgsty/minio:RELEASE.2026-08-04T00-00-00Z@sha256:b6bfe7239bfc83fb90d31612d9704d86039dd714f7904b3f1ad68f211e602372"

// s3TestStorage is a minimal S3 backup.Storage + backup.Presigner for this
// test only.
//
// 保存先の実装は #3458 が足す (internal/backup の S3Storage)。それまで、一覧・
// 使用量・署名付き URL が実際の S3 互換の保存先 (MinIO) で正しく動くことを、
// aws-sdk を直に使うこの最小の実装で確かめる。#3458 がマージされたら、
// backup.S3Storage に差し替える。
type s3TestStorage struct {
	client *s3.Client
	bucket string
	prefix string
}

func (s *s3TestStorage) Put(ctx context.Context, key string, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: &s.bucket, Key: aws.String(s.prefix + key), Body: bytes.NewReader(b)})
	return err
}

func (s *s3TestStorage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(s.prefix + key)})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, backup.ErrNotFound
		}
		return nil, err
	}
	return out.Body, nil
}

func (s *s3TestStorage) Stat(ctx context.Context, key string) (backup.ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: aws.String(s.prefix + key)})
	if err != nil {
		var nf *s3types.NotFound
		if errors.As(err, &nf) {
			return backup.ObjectInfo{}, backup.ErrNotFound
		}
		return backup.ObjectInfo{}, err
	}
	return backup.ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), ModTime: aws.ToTime(out.LastModified)}, nil
}

func (s *s3TestStorage) List(ctx context.Context, prefix string) ([]backup.ObjectInfo, error) {
	var out []backup.ObjectInfo
	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: aws.String(s.prefix + prefix)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			out = append(out, backup.ObjectInfo{Key: strings.TrimPrefix(aws.ToString(o.Key), s.prefix), Size: aws.ToInt64(o.Size), ModTime: aws.ToTime(o.LastModified)})
		}
	}
	return out, nil
}

func (s *s3TestStorage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: aws.String(s.prefix + key)})
	return err
}

func (s *s3TestStorage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: aws.String(s.prefix + key)}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func startMinio(t *testing.T) *s3.Client {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	ctx := context.Background()
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			Cmd:          []string{"server", "/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": "minioadmin", "MINIO_ROOT_PASSWORD": "minioadmin"},
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
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       "us-east-1",
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("minioadmin", "minioadmin", ""),
	})
}

func TestMinio_ListUsageDeleteAndPresign(t *testing.T) {
	client := startMinio(t)
	ctx := context.Background()
	const bucket = "backups"
	_, err := client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	st := &s3TestStorage{client: client, bucket: bucket, prefix: "elythia/"}

	putGeneration(t, st, gen1, bytes.Repeat([]byte("a"), 1500), false)
	putGeneration(t, st, gen2, bytes.Repeat([]byte("b"), 2500), true)
	putJSON(t, st, backup.Key(gen2, backup.VerifyFile), backup.VerifyResult{ID: gen2, OK: true})
	require.NoError(t, st.Put(ctx, backup.Key(gen3, backup.DumpFile), strings.NewReader("partial")))
	// prefix の外のもの。この保存先の使用量には入らない。
	_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("other/x"), Body: strings.NewReader("zzz")})
	require.NoError(t, err)

	svc := NewService(Options{StorageType: "s3", Storage: st, PricePerGBMonth: 0.015})
	ov, err := svc.List(ctx)
	require.NoError(t, err)

	// **保存先の実際の中身と突き合わせる。** MinIO に直接 ListObjectsV2 を
	// 投げて、prefix の下の合計と世代ごとの合計を数える。
	raw, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("elythia/")})
	require.NoError(t, err)
	var total int64
	perGen := map[string]int64{}
	for _, o := range raw.Contents {
		total += aws.ToInt64(o.Size)
		id := backup.GenerationIDFromKey(strings.TrimPrefix(aws.ToString(o.Key), "elythia/"))
		perGen[id] += aws.ToInt64(o.Size)
	}
	assert.Equal(t, total, ov.Usage.TotalBytes)
	assert.Equal(t, len(raw.Contents), ov.Usage.ObjectCount)
	assert.Equal(t, 3, ov.Usage.GenerationCount)
	require.Len(t, ov.Generations, 3)
	for _, g := range ov.Generations {
		assert.Equal(t, perGen[g.ID], g.Size, g.ID)
	}
	assert.True(t, ov.Generations[1].Encrypted)
	require.NotNil(t, ov.Generations[1].Verify)
	assert.True(t, ov.Generations[1].Verify.OK)
	assert.False(t, ov.Generations[0].Complete)

	// 署名付き URL: 期限内は取れて、期限を過ぎると拒否される。
	short := NewService(Options{Storage: st, DownloadTTL: 2 * time.Second})
	d, err := short.Download(ctx, gen2, "admin1")
	require.NoError(t, err)
	assert.Equal(t, "storage", d.Via)
	res, err := http.Get(d.URL)
	require.NoError(t, err)
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	require.Equal(t, http.StatusOK, res.StatusCode)
	assert.Equal(t, bytes.Repeat([]byte("b"), 2500), body, "the encrypted dump is handed out as stored")

	time.Sleep(3500 * time.Millisecond)
	res, err = http.Get(d.URL)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	assert.Equal(t, http.StatusForbidden, res.StatusCode, "an expired presigned URL must be refused")

	// 消すと、保存先からも消える。
	freed, err := svc.Delete(ctx, gen1)
	require.NoError(t, err)
	assert.Equal(t, perGen[gen1], freed)
	left, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("elythia/" + backup.Key(gen1, ""))})
	require.NoError(t, err)
	assert.Empty(t, left.Contents)
	ov, err = svc.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, total-perGen[gen1], ov.Usage.TotalBytes)
	assert.Equal(t, 2, ov.Usage.GenerationCount)
}
