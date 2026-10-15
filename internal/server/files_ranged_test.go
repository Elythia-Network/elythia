package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/model"
)

// fakeRangeS3 is an in-memory S3API whose GetObject honours a single Range the
// way S3 does and counts every byte the caller actually reads from the bodies.
type fakeRangeS3 struct {
	objects map[string][]byte
	etag    string
	mod     time.Time
	// getErr, when set, fails the GetObject whose Range equals errOnRange
	// ("" = the whole-object request).
	getErr     error
	errOnRange *string

	read   atomic.Int64
	ranges []string
}

type countingBody struct {
	r    io.Reader
	read *atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *countingBody) Close() error { return nil }

func (f *fakeRangeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	rng := aws.ToString(in.Range)
	f.ranges = append(f.ranges, rng)
	if f.getErr != nil && f.errOnRange != nil && *f.errOnRange == rng {
		return nil, f.getErr
	}
	data, ok := f.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	out := &s3.GetObjectOutput{
		ETag:         aws.String(f.etag),
		LastModified: aws.Time(f.mod),
	}
	size := int64(len(data))
	if rng == "" {
		out.Body = &countingBody{r: bytes.NewReader(data), read: &f.read}
		out.ContentLength = aws.Int64(size)
		return out, nil
	}
	spec := strings.TrimPrefix(rng, "bytes=")
	first, last, _ := strings.Cut(spec, "-")
	var start, end int64
	switch {
	case first == "":
		n, _ := strconv.ParseInt(last, 10, 64)
		if n == 0 || size == 0 {
			return nil, &smithy.GenericAPIError{Code: "InvalidRange"}
		}
		start, end = max(size-n, 0), size-1
	default:
		start, _ = strconv.ParseInt(first, 10, 64)
		end = size - 1
		if last != "" {
			e, _ := strconv.ParseInt(last, 10, 64)
			end = min(e, size-1)
		}
	}
	if start >= size {
		return nil, &smithy.GenericAPIError{Code: "InvalidRange"}
	}
	out.Body = &countingBody{r: bytes.NewReader(data[start : end+1]), read: &f.read}
	out.ContentLength = aws.Int64(end - start + 1)
	out.ContentRange = aws.String(fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	return out, nil
}

func (f *fakeRangeS3) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{}, nil
}

func (f *fakeRangeS3) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeRangeS3) CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	return &s3.CreateMultipartUploadOutput{}, nil
}

func (f *fakeRangeS3) UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	return &s3.UploadPartOutput{}, nil
}

func (f *fakeRangeS3) CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeRangeS3) AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	return &s3.AbortMultipartUploadOutput{}, nil
}

// pngObject is a PNG signature followed by size-8 filler bytes, so that only
// the object's first bytes sniff as image/png.
func pngObject(size int) []byte {
	data := bytes.Repeat([]byte{'<'}, size)
	copy(data, "\x89PNG\r\n\x1a\n")
	return data
}

type rangedFixture struct {
	s3      *fakeRangeS3
	handler echo.HandlerFunc
}

func newRangedFixture(t *testing.T, key string, data []byte) *rangedFixture {
	t.Helper()
	fake := &fakeRangeS3{
		objects: map[string][]byte{"files/" + key: data},
		etag:    `"etag-1"`,
		mod:     time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}
	primary := coredrive.NewS3Storage(coredrive.S3StorageConfig{Client: fake, Bucket: "b", Prefix: "files"})
	lookup := &stubFilesLookup{byKey: map[string]*model.DriveFile{key: {ID: "f1"}}}
	return &rangedFixture{s3: fake, handler: filesHandler(lookup, primary, &memStorage{byKey: map[string]string{}})}
}

func (f *rangedFixture) do(t *testing.T, method, key string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, "/files/"+key, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/files/:accessKey")
	c.SetParamNames("accessKey")
	c.SetParamValues(key)
	require.NoError(t, f.handler(c))
	return rec
}

// **Range 1 バイトの要求で、オブジェクトの全体を読まない。** 読む量が
// 要求された範囲に比例し、ファイルの大きさに比例しないこと。
func TestFilesHandler_ObjectStorageRangeDoesNotReadWholeObject(t *testing.T) {
	const size = 4 << 20
	f := newRangedFixture(t, "big", pngObject(size))

	rec := f.do(t, http.MethodGet, "big", map[string]string{"Range": "bytes=0-0"})

	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "\x89", rec.Body.String())
	assert.Equal(t, "1", rec.Header().Get(echo.HeaderContentLength))
	assert.Equal(t, fmt.Sprintf("bytes 0-0/%d", size), rec.Header().Get("Content-Range"))
	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType), "MIME must be sniffed from the object's first bytes")
	assert.LessOrEqual(t, f.s3.read.Load(), int64(1+coredrive.MIMESniffLen), "read far more of the object than the response needs")
	assert.Equal(t, "bytes=0-0", f.s3.ranges[0], "the client's range must be passed to S3")
}

func TestFilesHandler_ObjectStorageServesWholeObjectStreamed(t *testing.T) {
	data := pngObject(10000)
	f := newRangedFixture(t, "k", data)

	rec := f.do(t, http.MethodGet, "k", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, data, rec.Body.Bytes())
	assert.Equal(t, "10000", rec.Header().Get(echo.HeaderContentLength))
	assert.Empty(t, rec.Header().Get("Content-Range"))
	assert.Equal(t, `"etag-1"`, rec.Header().Get("ETag"))
	assert.Equal(t, "bytes", rec.Header().Get("Accept-Ranges"))
	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType))
	assert.Equal(t, "max-age=31536000, immutable, no-transform", rec.Header().Get("Cache-Control"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, []string{""}, f.s3.ranges, "a whole object needs no separate sniff request")
}

// 先頭から始まらない範囲 (動画のシーク) でも、MIME はオブジェクトの先頭で判定する。
// 範囲の中身 ("<<<<") で判定すると text/html になり、許可リストの意味が変わる。
func TestFilesHandler_ObjectStorageMidRangeSniffsObjectHead(t *testing.T) {
	data := pngObject(1 << 20)
	f := newRangedFixture(t, "k", data)

	rec := f.do(t, http.MethodGet, "k", map[string]string{"Range": "bytes=1000-1999"})

	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, data[1000:2000], rec.Body.Bytes())
	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType))
	assert.Equal(t, "bytes 1000-1999/1048576", rec.Header().Get("Content-Range"))
	assert.LessOrEqual(t, f.s3.read.Load(), int64(1000+coredrive.MIMESniffLen))
}

func TestFilesHandler_ObjectStorageSuffixRange(t *testing.T) {
	data := pngObject(5000)
	f := newRangedFixture(t, "k", data)

	rec := f.do(t, http.MethodGet, "k", map[string]string{"Range": "bytes=-10"})

	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, data[4990:], rec.Body.Bytes())
	assert.Equal(t, "bytes 4990-4999/5000", rec.Header().Get("Content-Range"))
}

// 非 browser-safe な中身は、ストリームの経路でも octet-stream に矯正する (#2106 H3)。
func TestFilesHandler_ObjectStorageNonBrowserSafeIsOctetStream(t *testing.T) {
	f := newRangedFixture(t, "k", []byte("<html><script>alert(1)</script></html>"))

	rec := f.do(t, http.MethodGet, "k", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "application/octet-stream", rec.Header().Get(echo.HeaderContentType))
	assert.Equal(t, "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'", rec.Header().Get("Content-Security-Policy"))
}

func TestFilesHandler_ObjectStorageRangeHeaderVariants(t *testing.T) {
	data := pngObject(5000)
	for _, tc := range []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantBody   []byte
	}{
		{"malformed", map[string]string{"Range": "bytes=abc"}, http.StatusRequestedRangeNotSatisfiable, nil},
		{"reversed", map[string]string{"Range": "bytes=5-1"}, http.StatusRequestedRangeNotSatisfiable, nil},
		{"unknown unit", map[string]string{"Range": "items=0-1"}, http.StatusRequestedRangeNotSatisfiable, nil},
		{"no dash", map[string]string{"Range": "bytes=5"}, http.StatusRequestedRangeNotSatisfiable, nil},
		{"bad suffix", map[string]string{"Range": "bytes=-x"}, http.StatusRequestedRangeNotSatisfiable, nil},
		{"bad end", map[string]string{"Range": "bytes=1-x"}, http.StatusRequestedRangeNotSatisfiable, nil},
		{"beyond the end", map[string]string{"Range": "bytes=999999-"}, http.StatusRequestedRangeNotSatisfiable, nil},
		{"multiple ranges serve the whole object", map[string]string{"Range": "bytes=0-1, 5-6"}, http.StatusOK, data},
		{"empty range set serves the whole object", map[string]string{"Range": "bytes= ,"}, http.StatusOK, data},
		{"If-Range ignores the range", map[string]string{"Range": "bytes=0-0", "If-Range": `"etag-1"`}, http.StatusOK, data},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRangedFixture(t, "k", data)
			rec := f.do(t, http.MethodGet, "k", tc.headers)
			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantBody != nil {
				assert.Equal(t, tc.wantBody, rec.Body.Bytes())
			}
		})
	}
}

func TestFilesHandler_ObjectStorageConditionalRequests(t *testing.T) {
	data := pngObject(5000)
	for _, tc := range []struct {
		name       string
		headers    map[string]string
		wantStatus int
	}{
		{"matching If-None-Match", map[string]string{"If-None-Match": `"other", W/"etag-1"`}, http.StatusNotModified},
		{"wildcard If-None-Match", map[string]string{"If-None-Match": "*"}, http.StatusNotModified},
		{"stale If-None-Match", map[string]string{"If-None-Match": `"other"`}, http.StatusOK},
		{"If-None-Match wins over If-Modified-Since", map[string]string{"If-None-Match": `"other"`, "If-Modified-Since": "Wed, 01 Oct 2026 00:00:00 GMT"}, http.StatusOK},
		{"fresh If-Modified-Since", map[string]string{"If-Modified-Since": "Wed, 01 Oct 2026 00:00:00 GMT"}, http.StatusNotModified},
		{"old If-Modified-Since", map[string]string{"If-Modified-Since": "Tue, 30 Sep 2026 00:00:00 GMT"}, http.StatusOK},
		{"malformed If-Modified-Since", map[string]string{"If-Modified-Since": "yesterday"}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRangedFixture(t, "k", data)
			rec := f.do(t, http.MethodGet, "k", tc.headers)
			assert.Equal(t, tc.wantStatus, rec.Code)
			if tc.wantStatus == http.StatusNotModified {
				assert.Empty(t, rec.Body.Bytes())
				assert.Equal(t, `"etag-1"`, rec.Header().Get("ETag"))
				assert.Equal(t, "max-age=31536000, immutable, no-transform", rec.Header().Get("Cache-Control"))
			}
		})
	}
}

func TestFilesHandler_ObjectStorageNoValidatorsNeverNotModified(t *testing.T) {
	f := newRangedFixture(t, "k", pngObject(100))
	f.s3.etag = ""
	f.s3.mod = time.Time{}
	// LastModified がゼロ時刻でも aws.Time は非 nil なので、ここではゼロ時刻を
	// 「無い」として扱えることを見る。
	rec := f.do(t, http.MethodGet, "k", map[string]string{"If-None-Match": "*", "If-Modified-Since": "Wed, 01 Oct 2026 00:00:00 GMT"})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("ETag"))
	assert.Empty(t, rec.Header().Get("Last-Modified"))
}

func TestFilesHandler_ObjectStorageHead(t *testing.T) {
	const size = 1 << 20
	f := newRangedFixture(t, "k", pngObject(size))

	rec := f.do(t, http.MethodHead, "k", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.Bytes())
	assert.Equal(t, strconv.Itoa(size), rec.Header().Get(echo.HeaderContentLength))
	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType))
	assert.LessOrEqual(t, f.s3.read.Load(), int64(coredrive.MIMESniffLen))
}

func TestFilesHandler_ObjectStorageHeadWithRange(t *testing.T) {
	f := newRangedFixture(t, "k", pngObject(5000))

	rec := f.do(t, http.MethodHead, "k", map[string]string{"Range": "bytes=10-19"})

	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Empty(t, rec.Body.Bytes())
	assert.Equal(t, "10", rec.Header().Get(echo.HeaderContentLength))
	assert.Equal(t, "bytes 10-19/5000", rec.Header().Get("Content-Range"))
}

func TestFilesHandler_ObjectStorageEmptyObject(t *testing.T) {
	f := newRangedFixture(t, "k", []byte{})

	head := f.do(t, http.MethodHead, "k", nil)
	assert.Equal(t, http.StatusOK, head.Code)
	assert.Equal(t, "0", head.Header().Get(echo.HeaderContentLength))
	assert.Equal(t, "max-age=31536000, immutable, no-transform", head.Header().Get("Cache-Control"))

	get := f.do(t, http.MethodGet, "k", nil)
	assert.Equal(t, http.StatusOK, get.Code)
	assert.Empty(t, get.Body.Bytes())
	assert.Equal(t, "0", get.Header().Get(echo.HeaderContentLength))
}

// 短い範囲が先頭から始まっても、判定に足りなければ先頭を別に取る。
func TestFilesHandler_ObjectStorageShortHeadRangeStillSniffsEnough(t *testing.T) {
	f := newRangedFixture(t, "k", pngObject(5000))

	rec := f.do(t, http.MethodGet, "k", map[string]string{"Range": "bytes=0-3"})

	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "image/png", rec.Header().Get(echo.HeaderContentType))
	assert.Equal(t, []string{"bytes=0-3", "bytes=0-3071"}, f.s3.ranges)
}

func TestFilesHandler_ObjectStorageFailures(t *testing.T) {
	boom := errors.New("s3 down")
	whole, probe := "", "bytes=0-3071"
	for _, tc := range []struct {
		name       string
		errOn      *string
		headers    map[string]string
		key        string
		wantStatus int
	}{
		{"missing object", nil, nil, "missing", http.StatusNotFound},
		{"main fetch fails", &whole, nil, "k", http.StatusInternalServerError},
		{"sniff fetch fails", &probe, map[string]string{"Range": "bytes=10-19"}, "k", http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRangedFixture(t, "k", pngObject(5000))
			// 行はある (無いと #3037 の 404 で手前で止まる)。
			f.handler = filesHandler(&stubFilesLookup{byKey: map[string]*model.DriveFile{"k": {ID: "f1"}, "missing": {ID: "f2"}}},
				coredrive.NewS3Storage(coredrive.S3StorageConfig{Client: f.s3, Bucket: "b", Prefix: "files"}), nil)
			f.s3.getErr = boom
			f.s3.errOnRange = tc.errOn
			rec := f.do(t, http.MethodGet, tc.key, tc.headers)
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

// 送信中に切れても handler はエラーを返さない (echo の error handler に二重に
// 書かせない)。
func TestFilesHandler_ObjectStorageClientDisconnect(t *testing.T) {
	f := newRangedFixture(t, "k", pngObject(100000))
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/files/k", nil)
	w := &failingWriter{ResponseRecorder: httptest.NewRecorder()}
	c := e.NewContext(req, w)
	c.SetPath("/files/:accessKey")
	c.SetParamNames("accessKey")
	c.SetParamValues("k")
	require.NoError(t, f.handler(c))
	assert.Equal(t, http.StatusOK, w.Code)
}

type failingWriter struct {
	*httptest.ResponseRecorder
}

func (w *failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		in          string
		start, size int64
	}{
		{"bytes 5-9/100", 5, 100},
		{"bytes 5-9/*", 5, -1},
		{"bytes */100", -1, 100},
		{"items 1-2/3", -1, -1},
		{"", -1, -1},
	} {
		start, size := parseContentRange(tc.in)
		assert.Equal(t, tc.start, start, tc.in)
		assert.Equal(t, tc.size, size, tc.in)
	}
}
