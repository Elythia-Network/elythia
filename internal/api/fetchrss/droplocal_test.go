package fetchrss

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seqFeedServer answers each request with a feed titled after the request
// number ("Feed 1", "Feed 2", ...). When block is set, the first request waits
// until release is closed.
type seqFeedServer struct {
	*httptest.Server
	hits    atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func newSeqFeedServer(t *testing.T, block bool) *seqFeedServer {
	t.Helper()
	s := &seqFeedServer{entered: make(chan struct{}), release: make(chan struct{})}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := s.hits.Add(1)
		if block && n == 1 {
			close(s.entered)
			<-s.release
		}
		fmt.Fprint(w, strings.Replace(sampleRSS2, "Example Feed", fmt.Sprintf("Feed %d", n), 1))
	}))
	t.Cleanup(s.Close)
	return s
}

// fetchFeedTitle requests the feed and returns the status and the title.
func fetchFeedTitle(t *testing.T, h *Handler, feedURL string) (int, string) {
	t.Helper()
	c, rec := newRequestCtx(http.MethodGet, "/api/fetch-rss?url="+url.QueryEscape(feedURL))
	require.NoError(t, h.Fetch(c))
	if rec.Code != http.StatusOK {
		return rec.Code, ""
	}
	var got struct {
		Title string `json:"title"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return rec.Code, got.Title
}

func TestFetchRSS_DropLocal(t *testing.T) {
	srv := newSeqFeedServer(t, false)
	h := newTestHandler(t, srv.Server)

	_, title := fetchFeedTitle(t, h, srv.URL)
	require.Equal(t, "Feed 1", title)
	_, title = fetchFeedTitle(t, h, srv.URL)
	require.Equal(t, "Feed 1", title, "前提: 2 回目はキャッシュから返る")

	h.DropLocal()
	_, title = fetchFeedTitle(t, h, srv.URL)
	assert.Equal(t, "Feed 2", title, "the feeds were not dropped")
}

// A drop while a feed is fetched keeps it out of the cache, and a request
// after the drop does not share the fetch started before it.
func TestFetchRSS_DropDuringFetch(t *testing.T) {
	srv := newSeqFeedServer(t, true)
	h := newTestHandler(t, srv.Server)

	var wg sync.WaitGroup
	var before string
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, before = fetchFeedTitle(t, h, srv.URL)
	}()
	waitEntered(t, srv.entered)
	h.DropLocal()

	after := make(chan string, 1)
	go func() {
		_, title := fetchFeedTitle(t, h, srv.URL)
		after <- title
	}()
	select {
	case title := <-after:
		assert.Equal(t, "Feed 2", title)
	case <-time.After(5 * time.Second):
		close(srv.release)
		t.Fatal("a request after the drop waited for the fetch started before it")
	}

	close(srv.release)
	wg.Wait()
	require.Equal(t, "Feed 1", before)
	_, title := fetchFeedTitle(t, h, srv.URL)
	assert.Equal(t, "Feed 2", title, "the feed fetched before the drop was stored over the newer one")
}

// A fetch started after a drop does not join the one started before it, so it
// counts toward the concurrency cap like a fetch of another feed.
func TestFetchRSS_DropDuringFetchCountsTowardTheCap(t *testing.T) {
	srv := newSeqFeedServer(t, true)
	h := newTestHandler(t, srv.Server)
	h.SetMaxConcurrentFetches(1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		fetchFeedTitle(t, h, srv.URL)
	}()
	waitEntered(t, srv.entered)
	h.DropLocal()

	code, _ := fetchFeedTitle(t, h, srv.URL)
	close(srv.release)
	wg.Wait()
	assert.Equal(t, http.StatusServiceUnavailable, code,
		"a fetch after the drop was counted as joining the fetch started before it")
	assert.Equal(t, int64(1), srv.hits.Load())
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
