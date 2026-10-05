package drive

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/elythia-network/elythia/internal/model"
)

// NewStorageFromMeta creates a Storage based on the Meta table settings.
// UseObjectStorage=true の場合は S3Storage を、false の場合は LocalStorage を返す。
func NewStorageFromMeta(meta *model.Meta, localDir, localBaseURL string) Storage {
	if meta == nil || !meta.UseObjectStorage {
		return NewLocalStorage(localDir, localBaseURL)
	}

	bucket := ""
	if meta.ObjectStorageBucket != nil {
		bucket = *meta.ObjectStorageBucket
	}
	if bucket == "" {
		// バケット未設定の場合は LocalStorage にフォールバック
		return NewLocalStorage(localDir, localBaseURL)
	}

	prefix := ""
	if meta.ObjectStoragePrefix != nil {
		prefix = *meta.ObjectStoragePrefix
	}

	baseURL := buildBaseURL(meta)

	client := buildS3Client(meta)

	return NewS3Storage(S3StorageConfig{
		Client:        client,
		Bucket:        bucket,
		Prefix:        prefix,
		BaseURL:       baseURL,
		SetPublicRead: meta.ObjectStorageSetPublicRead,
	})
}

// PublicBaseURL returns the URL base that objects in the configured object
// storage are served from, or "" when object storage is not in use.
//
// pack 層が「この URL は自分がホストしている」と判定するのに使う (#2315)。
// NewStorageFromMeta と同じ条件・同じ buildBaseURL を通すので、実際に
// drive_file.url へ書かれる値と必ず同じ base になる。
func PublicBaseURL(meta *model.Meta) string {
	if meta == nil || !meta.UseObjectStorage {
		return ""
	}
	if meta.ObjectStorageBucket == nil || *meta.ObjectStorageBucket == "" {
		// bucket 未設定は NewStorageFromMeta が local に fallback する条件。
		return ""
	}
	return buildBaseURL(meta)
}

// buildBaseURL constructs the public URL base from Meta settings.
// ObjectStorageBaseURL が設定されていればそれを使い、
// 未設定の場合はエンドポイントから自動生成する。
func buildBaseURL(meta *model.Meta) string {
	if meta.ObjectStorageBaseURL != nil && *meta.ObjectStorageBaseURL != "" {
		return *meta.ObjectStorageBaseURL
	}

	// エンドポイントから自動生成
	scheme := "https"
	if !meta.ObjectStorageUseSSL {
		scheme = "http"
	}

	endpoint := "s3.amazonaws.com"
	if meta.ObjectStorageEndpoint != nil && *meta.ObjectStorageEndpoint != "" {
		endpoint = *meta.ObjectStorageEndpoint
	}

	bucket := ""
	if meta.ObjectStorageBucket != nil {
		bucket = *meta.ObjectStorageBucket
	}

	if meta.ObjectStoragePort != nil && *meta.ObjectStoragePort > 0 {
		return fmt.Sprintf("%s://%s:%d/%s", scheme, endpoint, *meta.ObjectStoragePort, bucket)
	}
	return fmt.Sprintf("%s://%s/%s", scheme, endpoint, bucket)
}

// buildS3Client creates an S3 client from Meta settings.
func buildS3Client(meta *model.Meta) *s3.Client {
	scheme := "https"
	if !meta.ObjectStorageUseSSL {
		scheme = "http"
	}

	opts := []func(*s3.Options){
		func(o *s3.Options) {
			o.UsePathStyle = meta.ObjectStorageS3ForcePathStyle
		},
	}

	// エンドポイント設定
	if meta.ObjectStorageEndpoint != nil && *meta.ObjectStorageEndpoint != "" {
		endpoint := *meta.ObjectStorageEndpoint
		port := ""
		if meta.ObjectStoragePort != nil && *meta.ObjectStoragePort > 0 {
			port = fmt.Sprintf(":%d", *meta.ObjectStoragePort)
		}
		endpointURL := fmt.Sprintf("%s://%s%s", scheme, endpoint, port)
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(endpointURL)
		})
	}

	// リージョン設定
	region := "us-east-1"
	if meta.ObjectStorageRegion != nil && *meta.ObjectStorageRegion != "" {
		region = *meta.ObjectStorageRegion
	}
	opts = append(opts, func(o *s3.Options) {
		o.Region = region
	})

	// 認証情報
	accessKey := ""
	secretKey := ""
	if meta.ObjectStorageAccessKey != nil {
		accessKey = *meta.ObjectStorageAccessKey
	}
	if meta.ObjectStorageSecretKey != nil {
		secretKey = *meta.ObjectStorageSecretKey
	}
	if accessKey != "" && secretKey != "" {
		creds := credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")
		opts = append(opts, func(o *s3.Options) {
			o.Credentials = creds
		})
	}

	return s3.New(s3.Options{}, opts...)
}
