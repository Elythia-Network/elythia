package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// ErrRangeNotSatisfiable is returned by RangeGetter.GetRange when the requested
// byte range does not overlap the object.
var ErrRangeNotSatisfiable = errors.New("range not satisfiable")

// RangedObject is an object body fetched by RangeGetter.GetRange, together with
// the metadata needed to answer an HTTP request without buffering the body.
type RangedObject struct {
	// Body streams the returned bytes. The caller must close it.
	Body io.ReadCloser
	// ContentLength is the number of bytes in Body, or -1 when unknown.
	ContentLength int64
	// ContentRange is the backend's Content-Range ("bytes a-b/size") when only
	// part of the object was returned, and empty for the whole object.
	ContentRange string
	// ETag is the backend's entity tag (quoted), or empty.
	ETag string
	// LastModified is the backend's modification time, or the zero time.
	LastModified time.Time
}

// RangeGetter is implemented by storages that can return a byte range of an
// object without reading the rest of it.
type RangeGetter interface {
	// GetRange returns the bytes selected by rangeSpec, a single HTTP range
	// ("bytes=0-99", "bytes=100-", "bytes=-100"), or the whole object when
	// rangeSpec is empty. It returns ErrObjectNotFound when the object does not
	// exist and ErrRangeNotSatisfiable when the range lies outside the object.
	GetRange(accessKey, rangeSpec string) (*RangedObject, error)
}

// S3Storage implements RangeGetter.
var _ RangeGetter = (*S3Storage)(nil)

// GetRange retrieves an object (or one byte range of it) from S3 as a stream.
//
// `/files/<accessKey>` が `Get` の Body (seek できない) を全量メモリに読んで
// いたので、Range を S3 にそのまま渡し、返ってきた分だけを流せるようにする。
func (s *S3Storage) GetRange(accessKey, rangeSpec string) (*RangedObject, error) {
	key := s.objectKey(accessKey)
	input := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if rangeSpec != "" {
		input.Range = aws.String(rangeSpec)
	}
	output, err := s.client.GetObject(context.Background(), input)
	if err != nil {
		if isS3NotFound(err) {
			return nil, ErrObjectNotFound
		}
		if isS3InvalidRange(err) {
			return nil, ErrRangeNotSatisfiable
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	obj := &RangedObject{Body: output.Body, ContentLength: -1}
	if output.ContentLength != nil {
		obj.ContentLength = *output.ContentLength
	}
	if output.ContentRange != nil {
		obj.ContentRange = *output.ContentRange
	}
	if output.ETag != nil {
		obj.ETag = *output.ETag
	}
	if output.LastModified != nil {
		obj.LastModified = *output.LastModified
	}
	return obj, nil
}

// isS3InvalidRange reports whether err is S3's answer to a range that does not
// overlap the object (416 InvalidRange).
func isS3InvalidRange(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidRange" {
		return true
	}
	var respErr *awshttp.ResponseError
	return errors.As(err, &respErr) && respErr.HTTPStatusCode() == http.StatusRequestedRangeNotSatisfiable
}

// sizedBody is an object body whose total size the backend reported.
type sizedBody struct {
	io.ReadCloser
	size int64
}

// Size returns the object's size in bytes.
func (b *sizedBody) Size() int64 { return b.size }

// ObjectSize reports the size of a body returned by Storage.Get when it is
// known without reading the body: a local file, or an S3 object whose
// Content-Length was returned. ok is false otherwise.
func ObjectSize(body io.Reader) (size int64, ok bool) {
	switch b := body.(type) {
	case interface{ Size() int64 }:
		return b.Size(), true
	case interface{ Stat() (fs.FileInfo, error) }:
		info, err := b.Stat()
		if err != nil || !info.Mode().IsRegular() {
			return 0, false
		}
		return info.Size(), true
	}
	return 0, false
}
