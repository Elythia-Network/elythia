package drive

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func TestNewStorageFromMeta_NilMeta(t *testing.T) {
	st := NewStorageFromMeta(nil, "/tmp/drive", "https://example.com/files")
	_, ok := st.(*LocalStorage)
	assert.True(t, ok, "nil meta should return LocalStorage")
}

func TestNewStorageFromMeta_UseObjectStorageFalse(t *testing.T) {
	meta := &model.Meta{UseObjectStorage: false}
	st := NewStorageFromMeta(meta, "/tmp/drive", "https://example.com/files")
	_, ok := st.(*LocalStorage)
	assert.True(t, ok)
}

func TestNewStorageFromMeta_NoBucket(t *testing.T) {
	meta := &model.Meta{UseObjectStorage: true}
	st := NewStorageFromMeta(meta, "/tmp/drive", "https://example.com/files")
	_, ok := st.(*LocalStorage)
	assert.True(t, ok, "empty bucket should fallback to LocalStorage")
}

func TestNewStorageFromMeta_EmptyBucket(t *testing.T) {
	meta := &model.Meta{
		UseObjectStorage:    true,
		ObjectStorageBucket: strPtr(""),
	}
	st := NewStorageFromMeta(meta, "/tmp/drive", "https://example.com/files")
	_, ok := st.(*LocalStorage)
	assert.True(t, ok)
}

func TestNewStorageFromMeta_S3(t *testing.T) {
	meta := &model.Meta{
		UseObjectStorage:              true,
		ObjectStorageBucket:           strPtr("my-bucket"),
		ObjectStoragePrefix:           strPtr("files/"),
		ObjectStorageBaseURL:          strPtr("https://cdn.example.com"),
		ObjectStorageEndpoint:         strPtr("s3.example.com"),
		ObjectStorageRegion:           strPtr("ap-northeast-1"),
		ObjectStorageAccessKey:        strPtr("AKID"),
		ObjectStorageSecretKey:        strPtr("SECRET"),
		ObjectStorageUseSSL:           true,
		ObjectStorageSetPublicRead:    true,
		ObjectStorageS3ForcePathStyle: true,
	}
	st := NewStorageFromMeta(meta, "/tmp/drive", "https://example.com/files")
	s3st, ok := st.(*S3Storage)
	assert.True(t, ok, "should return S3Storage")
	assert.Equal(t, "my-bucket", s3st.bucket)
	// NewS3Storage は末尾の "/" を正規化するため、"files/" は "files" として
	// 保持される (#525)。objectKey が separator を再挿入するので最終的な
	// S3 キーは "files/<accessKey>" のまま変わらない。
	assert.Equal(t, "files", s3st.prefix)
	assert.Equal(t, "https://cdn.example.com", s3st.baseURL)
	assert.True(t, s3st.setPublicRead)
}

func TestNewStorageFromMeta_S3_NilOptionalFields(t *testing.T) {
	meta := &model.Meta{
		UseObjectStorage:    true,
		ObjectStorageBucket: strPtr("bucket"),
	}
	st := NewStorageFromMeta(meta, "/tmp/drive", "https://example.com/files")
	_, ok := st.(*S3Storage)
	assert.True(t, ok)
}

// ---------------------------------------------------------------------------
// buildBaseURL tests
// ---------------------------------------------------------------------------

func TestBuildBaseURL_CustomBaseURL(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageBaseURL: strPtr("https://cdn.example.com/files"),
	}
	assert.Equal(t, "https://cdn.example.com/files", buildBaseURL(meta))
}

func TestBuildBaseURL_AutoGenerate_HTTPS(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageUseSSL:   true,
		ObjectStorageEndpoint: strPtr("s3.example.com"),
		ObjectStorageBucket:   strPtr("my-bucket"),
	}
	assert.Equal(t, "https://s3.example.com/my-bucket", buildBaseURL(meta))
}

func TestBuildBaseURL_AutoGenerate_HTTP(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageUseSSL:   false,
		ObjectStorageEndpoint: strPtr("minio.local"),
		ObjectStorageBucket:   strPtr("bucket"),
	}
	assert.Equal(t, "http://minio.local/bucket", buildBaseURL(meta))
}

func TestBuildBaseURL_AutoGenerate_WithPort(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageUseSSL:   false,
		ObjectStorageEndpoint: strPtr("minio.local"),
		ObjectStoragePort:     intPtr(9000),
		ObjectStorageBucket:   strPtr("bucket"),
	}
	assert.Equal(t, "http://minio.local:9000/bucket", buildBaseURL(meta))
}

func TestBuildBaseURL_AutoGenerate_NoEndpoint(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageUseSSL: true,
		ObjectStorageBucket: strPtr("bucket"),
	}
	assert.Equal(t, "https://s3.amazonaws.com/bucket", buildBaseURL(meta))
}

func TestBuildBaseURL_EmptyBaseURL(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageBaseURL: strPtr(""),
		ObjectStorageUseSSL:  true,
		ObjectStorageBucket:  strPtr("bucket"),
	}
	assert.Equal(t, "https://s3.amazonaws.com/bucket", buildBaseURL(meta))
}

func TestBuildBaseURL_ZeroPort(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageEndpoint: strPtr("s3.example.com"),
		ObjectStoragePort:     intPtr(0),
		ObjectStorageBucket:   strPtr("bucket"),
		ObjectStorageUseSSL:   true,
	}
	// port=0 はポートなし扱い
	assert.Equal(t, "https://s3.example.com/bucket", buildBaseURL(meta))
}

// ---------------------------------------------------------------------------
// buildS3Client tests
// ---------------------------------------------------------------------------

func TestBuildS3Client_MinimalConfig(t *testing.T) {
	meta := &model.Meta{ObjectStorageUseSSL: true}
	client := buildS3Client(meta)
	assert.NotNil(t, client)
}

func TestBuildS3Client_FullConfig(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageEndpoint:         strPtr("s3.example.com"),
		ObjectStoragePort:             intPtr(9000),
		ObjectStorageRegion:           strPtr("eu-west-1"),
		ObjectStorageAccessKey:        strPtr("AKID"),
		ObjectStorageSecretKey:        strPtr("SECRET"),
		ObjectStorageUseSSL:           false,
		ObjectStorageS3ForcePathStyle: true,
	}
	client := buildS3Client(meta)
	assert.NotNil(t, client)
}

func TestBuildS3Client_NoCredentials(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageUseSSL: true,
		ObjectStorageRegion: strPtr("us-west-2"),
	}
	client := buildS3Client(meta)
	assert.NotNil(t, client)
}

func TestBuildS3Client_EmptyEndpoint(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageEndpoint: strPtr(""),
		ObjectStorageUseSSL:   true,
	}
	client := buildS3Client(meta)
	assert.NotNil(t, client)
}

func TestBuildS3Client_EmptyRegion(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageRegion: strPtr(""),
		ObjectStorageUseSSL: true,
	}
	client := buildS3Client(meta)
	assert.NotNil(t, client)
}

func TestBuildS3Client_PartialCredentials(t *testing.T) {
	meta := &model.Meta{
		ObjectStorageAccessKey: strPtr("AKID"),
		// SecretKey は nil
		ObjectStorageUseSSL: true,
	}
	client := buildS3Client(meta)
	assert.NotNil(t, client)
}

// #2315: pack 層が「この URL は自分がホストしている」と判定するのに使う。
// NewStorageFromMeta と同じ条件で "" / base URL を返すこと。
func TestPublicBaseURL(t *testing.T) {
	assert.Equal(t, "", PublicBaseURL(nil))
	assert.Equal(t, "", PublicBaseURL(&model.Meta{}), "object storage 無効なら空")

	bucket := "b"
	assert.Equal(t, "", PublicBaseURL(&model.Meta{UseObjectStorage: true}), "bucket 未設定は local fallback なので空")
	empty := ""
	assert.Equal(t, "", PublicBaseURL(&model.Meta{UseObjectStorage: true, ObjectStorageBucket: &empty}))

	base := "https://cdn.example/b"
	assert.Equal(t, base, PublicBaseURL(&model.Meta{
		UseObjectStorage: true, ObjectStorageBucket: &bucket, ObjectStorageBaseURL: &base,
	}))

	// baseUrl 未設定ならエンドポイントから自動生成した値 (= 実際に
	// drive_file.url へ書かれる base) と一致すること。
	endpoint := "s3.example.test"
	m := &model.Meta{
		UseObjectStorage:      true,
		ObjectStorageBucket:   &bucket,
		ObjectStorageEndpoint: &endpoint,
		ObjectStorageUseSSL:   true,
	}
	assert.Equal(t, "https://s3.example.test/b", PublicBaseURL(m))

	// 実際に drive_file.url へ書かれる値と同じ base であること (ネットワークは
	// 使わず URL 生成だけを見る)。
	st, ok := NewStorageFromMeta(m, t.TempDir(), "https://example.com/files").(*S3Storage)
	require.True(t, ok)
	assert.True(t, strings.HasPrefix(st.publicURL("k"), PublicBaseURL(m)+"/"),
		"生成される URL の base が PublicBaseURL と一致すること: %s", st.publicURL("k"))
}
