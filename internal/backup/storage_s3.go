package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/elythia-network/elythia/internal/config"
)

// DefaultS3PartSize is the multipart part size S3Storage.Put uses. With the
// S3 limit of 10000 parts it allows objects up to about 625 GiB.
const DefaultS3PartSize = 64 << 20

// s3Client is the part of *s3.Client S3Storage uses.
type s3Client interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
}

// S3Storage keeps generations in an S3-compatible bucket under a prefix.
type S3Storage struct {
	client  s3Client
	presign *s3.PresignClient
	bucket  string
	prefix  string
	// PartSize is the multipart part size. Objects smaller than it are sent
	// with a single PutObject.
	PartSize int
}

var (
	_ Storage   = (*S3Storage)(nil)
	_ Presigner = (*S3Storage)(nil)
	_ Storage   = (*DirStorage)(nil)
)

// NewS3Storage builds an S3Storage from the config.
func NewS3Storage(o config.BackupS3Options) (*S3Storage, error) {
	if o.Bucket == "" {
		return nil, errors.New("backup: storage.s3.bucket is empty")
	}
	if o.AccessKey == "" || o.SecretKey == "" {
		return nil, errors.New("backup: storage.s3.accessKey and storage.s3.secretKey are required")
	}
	region := o.Region
	if region == "" {
		region = "us-east-1"
	}
	opts := s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(o.AccessKey, o.SecretKey, ""),
		UsePathStyle: o.ForcePathStyle,
		// SDK の既定は、送るたびに CRC32 などの checksum を付ける。S3 互換の実装には
		// これを受け付けないものがあり、転送の正しさは送った後に sha256 を読み直して
		// 確かめているので、必要な操作に限る。
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	}
	if o.Endpoint != "" {
		opts.BaseEndpoint = aws.String(o.Endpoint)
	}
	client := s3.New(opts)
	return &S3Storage{
		client:   client,
		presign:  s3.NewPresignClient(client),
		bucket:   o.Bucket,
		prefix:   normalizePrefix(o.Prefix),
		PartSize: DefaultS3PartSize,
	}, nil
}

// normalizePrefix trims slashes and adds one trailing slash to a non-empty
// prefix, so "backups", "/backups/" and "backups/" are the same.
func normalizePrefix(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

func (s *S3Storage) objectKey(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("backup: invalid key %q", key)
	}
	return s.prefix + key, nil
}

// Put uploads r. Objects smaller than PartSize go in one PutObject; larger
// ones use a multipart upload, which is aborted when anything fails. Neither
// makes an object visible under key until the upload completes.
func (s *S3Storage) Put(ctx context.Context, key string, r io.Reader) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	partSize := s.PartSize
	if partSize <= 0 {
		partSize = DefaultS3PartSize
	}
	buf := make([]byte, partSize)
	n, err := io.ReadFull(r, buf)
	switch {
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(k),
			Body:          bytes.NewReader(buf[:n]),
			ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			return fmt.Errorf("backup: put %s: %w", key, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("backup: read %s: %w", key, err)
	}
	return s.putMultipart(ctx, key, k, buf, r)
}

func (s *S3Storage) putMultipart(ctx context.Context, key, k string, first []byte, r io.Reader) (err error) {
	created, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(k),
	})
	if err != nil {
		return fmt.Errorf("backup: start multipart upload %s: %w", key, err)
	}
	uploadID := created.UploadId
	defer func() {
		if err == nil {
			return
		}
		// 中断した multipart upload は object として見えないが、parts は残って容量の
		// 料金がかかる。呼び出し元の ctx が切れていても後始末は送る。
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = s.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(s.bucket),
			Key:      aws.String(k),
			UploadId: uploadID,
		})
	}()

	var parts []types.CompletedPart
	buf := first
	n := len(first)
	for num := int32(1); ; num++ {
		out, err := s.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(s.bucket),
			Key:           aws.String(k),
			UploadId:      uploadID,
			PartNumber:    aws.Int32(num),
			Body:          bytes.NewReader(buf[:n]),
			ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			return fmt.Errorf("backup: upload part %d of %s: %w", num, key, err)
		}
		parts = append(parts, types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(num)})
		if n < len(buf) {
			// 足りない分を読んだ part が最後。
			break
		}
		n, err = io.ReadFull(r, buf)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("backup: read %s: %w", key, err)
		}
	}
	_, err = s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(k),
		UploadId:        uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		return fmt.Errorf("backup: complete multipart upload %s: %w", key, err)
	}
	return nil
}

// Get opens key for reading.
func (s *S3Storage) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(k)})
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("backup: get %s: %w", key, err)
	}
	return out.Body, nil
}

// Stat describes key.
func (s *S3Storage) Stat(ctx context.Context, key string) (ObjectInfo, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(k)})
	if err != nil {
		if isS3NotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("backup: stat %s: %w", key, err)
	}
	return ObjectInfo{Key: key, Size: aws.ToInt64(out.ContentLength), ModTime: aws.ToTime(out.LastModified)}, nil
}

// List returns every object under prefix, in key order.
func (s *S3Storage) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	var token *string
	for {
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(s.prefix + prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("backup: list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(o.Key), s.prefix)
			out = append(out, ObjectInfo{Key: key, Size: aws.ToInt64(o.Size), ModTime: aws.ToTime(o.LastModified)})
		}
		if !aws.ToBool(page.IsTruncated) || page.NextContinuationToken == nil {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// Delete removes key. S3 does not report missing keys on delete.
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	k, err := s.objectKey(key)
	if err != nil {
		return err
	}
	if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(k)}); err != nil {
		return fmt.Errorf("backup: delete %s: %w", key, err)
	}
	return nil
}

// PresignGet returns a GET URL for key that expires after ttl.
func (s *S3Storage) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	k, err := s.objectKey(key)
	if err != nil {
		return "", err
	}
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(k)}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("backup: presign %s: %w", key, err)
	}
	return req.URL, nil
}

func isS3NotFound(err error) bool {
	var nsk *types.NoSuchKey
	var nf *types.NotFound
	if errors.As(err, &nsk) || errors.As(err, &nf) {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	return false
}
