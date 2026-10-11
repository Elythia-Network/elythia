package backup

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
)

// flakyS3 is a minimal S3 (path-style, single bucket) whose PUTs store the
// object and then answer 500 the first time for each key, like a response
// lost after the write. The SDK resends the request; a resent conditional
// PUT then meets the object its first copy stored and gets 412.
//
// 実物の SDK の自動の送り直しを通すため、s3Client の偽物ではなく HTTP の層で作る
// (#3458 で回帰したのは、この送り直しが 412 になる形)。
type flakyS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	failed  map[string]bool
	// puts counts the PUT requests per key, resent ones included.
	puts map[string]int
}

func newFlakyS3(t *testing.T) (*flakyS3, *S3Storage) {
	t.Helper()
	f := &flakyS3{objects: map[string][]byte{}, failed: map[string]bool{}, puts: map[string]int{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, err := NewS3Storage(config.BackupS3Options{
		Endpoint: srv.URL, Bucket: "b", AccessKey: "a", SecretKey: "s", ForcePathStyle: true, Prefix: "p",
	})
	require.NoError(t, err)
	return f, st
}

func (f *flakyS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := strings.CutPrefix(r.URL.Path, "/b/")
	if !ok && r.URL.Path != "/b" {
		http.Error(w, "no such bucket", http.StatusNotFound)
		return
	}
	switch {
	case r.Method == http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.puts[key]++
		if r.Header.Get("If-None-Match") == "*" {
			if _, exists := f.objects[key]; exists {
				writeS3Error(w, http.StatusPreconditionFailed, "PreconditionFailed")
				return
			}
		}
		f.objects[key] = body
		w.Header().Set("ETag", etag(body))
		if !f.failed[key] {
			f.failed[key] = true
			writeS3Error(w, http.StatusInternalServerError, "InternalError")
			return
		}
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		f.list(w, r.URL.Query().Get("prefix"))
	case r.Method == http.MethodGet || r.Method == http.MethodHead:
		body, exists := f.objects[key]
		if !exists {
			writeS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.Header().Set("ETag", etag(body))
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	case r.Method == http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeS3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func (f *flakyS3) list(w http.ResponseWriter, prefix string) {
	type content struct {
		Key          string
		Size         int
		LastModified string
		ETag         string
	}
	type result struct {
		XMLName     xml.Name `xml:"ListBucketResult"`
		Xmlns       string   `xml:"xmlns,attr"`
		Name        string
		Prefix      string
		KeyCount    int
		IsTruncated bool
		Contents    []content
	}
	res := result{Xmlns: "http://s3.amazonaws.com/doc/2006-03-01/", Name: "b", Prefix: prefix}
	for k, b := range f.objects {
		if strings.HasPrefix(k, prefix) {
			res.Contents = append(res.Contents, content{Key: k, Size: len(b), LastModified: time.Now().UTC().Format(time.RFC3339), ETag: etag(b)})
		}
	}
	sort.Slice(res.Contents, func(i, j int) bool { return res.Contents[i].Key < res.Contents[j].Key })
	res.KeyCount = len(res.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func writeS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func etag(b []byte) string {
	s := md5.Sum(b)
	return `"` + hex.EncodeToString(s[:]) + `"`
}

// TestS3Storage_PutNewSurvivesSDKRetry: the first PUT is stored and answered
// with 500, the SDK resends it and gets 412. The claim must succeed, because
// the object is the one this call sent.
func TestS3Storage_PutNewSurvivesSDKRetry(t *testing.T) {
	f, st := newFlakyS3(t)
	ctx := t.Context()
	key := Key("20261010T040000Z", LockFile)
	require.NoError(t, claimKey(ctx, st, key, []byte("mine")))
	require.Equal(t, 2, f.puts["p/"+key], "the SDK resent the request once")
	require.Equal(t, []byte("mine"), f.objects["p/"+key])

	// 別の中身で取ろうとすると、送り直しの有無に関係なく ErrExists。
	require.ErrorIs(t, claimKey(ctx, st, key, []byte("other")), ErrExists)
	require.Equal(t, []byte("mine"), f.objects["p/"+key])
}

// noConditionClient is an S3 service that does not implement conditional
// writes: it answers If-None-Match with 501.
type noConditionClient struct {
	s3Client
	unconditional int
}

func (c *noConditionClient) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if in.IfNoneMatch != nil {
		return nil, &smithy.GenericAPIError{Code: "NotImplemented"}
	}
	c.unconditional++
	return &s3.PutObjectOutput{}, nil
}

func TestS3Storage_PutNewFallsBackWithoutConditionalWrites(t *testing.T) {
	c := &noConditionClient{}
	st := &S3Storage{client: c, bucket: "b"}
	require.NoError(t, st.PutNew(t.Context(), "k", []byte("ab")))
	require.Equal(t, 1, c.unconditional, "the write is resent once without the condition")
}
