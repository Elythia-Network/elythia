package drive

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rangeRecordingS3 records the GetObject input and returns a canned output.
type rangeRecordingS3 struct {
	mockS3API
	input  *s3.GetObjectInput
	output *s3.GetObjectOutput
}

func (m *rangeRecordingS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	m.input = input
	if m.getErr != nil {
		return nil, m.getErr
	}
	return m.output, nil
}

func TestS3Storage_GetRange_PassesRangeAndMetadata(t *testing.T) {
	mod := time.Date(2026, 10, 15, 1, 2, 3, 0, time.UTC)
	mock := &rangeRecordingS3{output: &s3.GetObjectOutput{
		Body:          io.NopCloser(strings.NewReader("b")),
		ContentLength: aws.Int64(1),
		ContentRange:  aws.String("bytes 0-0/100"),
		ETag:          aws.String(`"abc"`),
		LastModified:  &mod,
	}}
	st := NewS3Storage(S3StorageConfig{Client: mock, Bucket: "bucket", Prefix: "files/"})

	obj, err := st.GetRange("k", "bytes=0-0")
	require.NoError(t, err)
	defer obj.Body.Close()

	assert.Equal(t, "files/k", aws.ToString(mock.input.Key))
	assert.Equal(t, "bucket", aws.ToString(mock.input.Bucket))
	assert.Equal(t, "bytes=0-0", aws.ToString(mock.input.Range), "the range must reach S3 instead of being applied after a full read")
	assert.Equal(t, int64(1), obj.ContentLength)
	assert.Equal(t, "bytes 0-0/100", obj.ContentRange)
	assert.Equal(t, `"abc"`, obj.ETag)
	assert.Equal(t, mod, obj.LastModified)
}

func TestS3Storage_GetRange_WholeObjectSendsNoRange(t *testing.T) {
	mock := &rangeRecordingS3{output: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(""))}}
	st := NewS3Storage(S3StorageConfig{Client: mock, Bucket: "bucket"})

	obj, err := st.GetRange("k", "")
	require.NoError(t, err)
	defer obj.Body.Close()

	assert.Nil(t, mock.input.Range)
	assert.Equal(t, int64(-1), obj.ContentLength, "unknown length must not read as 0")
	assert.Empty(t, obj.ContentRange)
	assert.Empty(t, obj.ETag)
	assert.True(t, obj.LastModified.IsZero())
}

func TestS3Storage_GetRange_ErrorClassification(t *testing.T) {
	withStatus := func(code string, status int) error {
		return &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
				Err:      &smithy.GenericAPIError{Code: code},
			},
		}
	}
	bareStatus := func(status int) error {
		return &awshttp.ResponseError{
			ResponseError: &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: status}},
				Err:      errors.New("could not parse response body"),
			},
		}
	}
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"NoSuchKey", withStatus("NoSuchKey", http.StatusNotFound), ErrObjectNotFound},
		{"InvalidRange code", &smithy.GenericAPIError{Code: "InvalidRange"}, ErrRangeNotSatisfiable},
		{"bare HTTP 416", bareStatus(http.StatusRequestedRangeNotSatisfiable), ErrRangeNotSatisfiable},
		{"provider 5xx", withStatus("InternalError", http.StatusInternalServerError), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := NewS3Storage(S3StorageConfig{Client: &rangeRecordingS3{mockS3API: mockS3API{getErr: tc.err}}, Bucket: "bucket"})
			_, err := st.GetRange("k", "bytes=5-")
			require.Error(t, err)
			if tc.want != nil {
				assert.ErrorIs(t, err, tc.want)
				return
			}
			assert.NotErrorIs(t, err, ErrObjectNotFound)
			assert.NotErrorIs(t, err, ErrRangeNotSatisfiable)
			assert.ErrorIs(t, err, tc.err)
		})
	}
}

// Get の Body は、S3 が返した Content-Length を読む前に ObjectSize で教える。
// ローカルの Get (os.File) も同じく大きさが分かる。
func TestObjectSize(t *testing.T) {
	t.Run("s3 with content length", func(t *testing.T) {
		mock := &rangeRecordingS3{output: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("abc")),
			ContentLength: aws.Int64(3),
		}}
		st := NewS3Storage(S3StorageConfig{Client: mock, Bucket: "bucket"})
		rc, err := st.Get("k")
		require.NoError(t, err)
		defer rc.Close()
		size, ok := ObjectSize(rc)
		assert.True(t, ok)
		assert.Equal(t, int64(3), size)
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, "abc", string(data))
	})
	t.Run("s3 without content length", func(t *testing.T) {
		mock := &rangeRecordingS3{output: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("abc"))}}
		st := NewS3Storage(S3StorageConfig{Client: mock, Bucket: "bucket"})
		rc, err := st.Get("k")
		require.NoError(t, err)
		defer rc.Close()
		_, ok := ObjectSize(rc)
		assert.False(t, ok)
	})
	t.Run("local file", func(t *testing.T) {
		st := NewLocalStorage(t.TempDir(), "")
		_, err := st.Put("k", strings.NewReader("hello"))
		require.NoError(t, err)
		rc, err := st.Get("k")
		require.NoError(t, err)
		defer rc.Close()
		size, ok := ObjectSize(rc)
		assert.True(t, ok)
		assert.Equal(t, int64(5), size)
	})
	t.Run("closed file", func(t *testing.T) {
		st := NewLocalStorage(t.TempDir(), "")
		_, err := st.Put("k", strings.NewReader("hello"))
		require.NoError(t, err)
		rc, err := st.Get("k")
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		_, ok := ObjectSize(rc)
		assert.False(t, ok)
	})
	t.Run("unknown reader", func(t *testing.T) {
		_, ok := ObjectSize(io.NopCloser(strings.NewReader("abc")))
		assert.False(t, ok)
	})
}
