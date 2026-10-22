package urlpreview

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingServer answers each request with a title carrying the request
// number. When block is set, the first request waits until release is closed.
type countingServer struct {
	*httptest.Server
	hits    atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func newCountingServer(t *testing.T, block bool) *countingServer {
	t.Helper()
	s := &countingServer{entered: make(chan struct{}), release: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := s.hits.Add(1)
		if block && n == 1 {
			close(s.entered)
			<-s.release
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<html><head><title>T%d</title></head></html>`, n)
	}))
	t.Cleanup(s.Close)
	return s
}

// newDropTestFetcher reads its settings on every request, so each Fetch runs
// on a copy (forRequest) like in the server.
func newDropTestFetcher() *Fetcher {
	cfg := Config{Enabled: true, TimeoutMs: 5000, MaxContentLength: 1 << 20}
	f := NewFetcher(cfg, nil)
	f.SetHTTPClient(&http.Client{Timeout: f.client.Timeout})
	f.SetConfigProvider(func() (Config, error) { return cfg, nil })
	return f
}

func fetchTitle(t *testing.T, f *Fetcher, url string) string {
	t.Helper()
	r, err := f.Fetch(context.Background(), url)
	require.NoError(t, err)
	require.NotNil(t, r.Title)
	return *r.Title
}

func TestFetcher_DropLocal(t *testing.T) {
	srv := newCountingServer(t, false)
	f := newDropTestFetcher()

	require.Equal(t, "T1", fetchTitle(t, f, srv.URL))
	require.Equal(t, "T1", fetchTitle(t, f, srv.URL), "前提: 2 回目はキャッシュから返る")

	f.DropLocal()
	assert.Equal(t, "T2", fetchTitle(t, f, srv.URL), "the previews were not dropped")
	assert.Equal(t, int64(2), srv.hits.Load())
}

// A drop while a preview is fetched keeps the fetched preview out of the
// cache.
func TestFetcher_DropLocalDuringFetchIsNotStored(t *testing.T) {
	srv := newCountingServer(t, true)
	f := newDropTestFetcher()

	var wg sync.WaitGroup
	var before string
	wg.Add(1)
	go func() {
		defer wg.Done()
		before = fetchTitle(t, f, srv.URL)
	}()
	waitEntered(t, srv.entered)
	f.DropLocal()
	close(srv.release)
	wg.Wait()
	require.Equal(t, "T1", before)

	assert.Equal(t, "T2", fetchTitle(t, f, srv.URL), "the preview fetched before the drop was stored")
}

// waitEntered fails the test when the server never receives the first
// request, instead of blocking until the package's timeout.
func waitEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the first request never reached the server")
	}
}
