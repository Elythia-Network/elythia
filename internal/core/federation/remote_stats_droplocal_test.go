package federation

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

// seqUsersShow answers /api/users/show with values carrying the request
// number n (notesCount n, createdAt on day n of 2020). When block is set, the
// first request waits until release is closed.
type seqUsersShow struct {
	hits    atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func newSeqUsersShowFetcher(t *testing.T, block bool) (*RemoteStatsFetcher, *seqUsersShow) {
	t.Helper()
	s := &seqUsersShow{entered: make(chan struct{}), release: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := s.hits.Add(1)
		if block && n == 1 {
			close(s.entered)
			<-s.release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"username":"alice","notesCount":%d,"createdAt":%q}`,
			n, time.Date(2020, 1, int(n), 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
	}))
	t.Cleanup(srv.Close)
	return newRemoteStatsFetcherWithTransport(redirectTransport{target: srv.URL}), s
}

// remoteStatsReads are the two cached reads of RemoteStatsFetcher, each
// reduced to the request number its value came from.
var remoteStatsReads = map[string]func(f *RemoteStatsFetcher) int{
	"stats": func(f *RemoteStatsFetcher) int {
		s := f.Fetch(context.Background(), "remote.example", "alice")
		if s == nil {
			return 0
		}
		return s.NotesCount
	},
	"createdAt": func(f *RemoteStatsFetcher) int {
		c := f.FetchAccountCreatedAt(context.Background(), "remote.example", "alice")
		if c == nil {
			return 0
		}
		return c.Day()
	},
}

func TestRemoteStatsFetcher_DropLocal(t *testing.T) {
	f, srv := newSeqUsersShowFetcher(t, false)
	require.Equal(t, 1, remoteStatsReads["stats"](f))
	require.Equal(t, 2, remoteStatsReads["createdAt"](f))
	require.Equal(t, 1, remoteStatsReads["stats"](f), "前提: 2 回目はキャッシュから返る")
	require.Equal(t, 2, remoteStatsReads["createdAt"](f), "前提: 2 回目はキャッシュから返る")
	// 応答しなかったホストの印は、取得の間引きなので捨てない。
	f.createdHostDown.Add("down.example", time.Now())

	f.DropLocal()

	assert.Equal(t, 3, remoteStatsReads["stats"](f), "the stats were not dropped")
	assert.Equal(t, 4, remoteStatsReads["createdAt"](f), "the account creation times were not dropped")
	assert.True(t, f.createdHostDown.Contains("down.example"), "DropLocal dropped the hosts that did not answer")
	assert.Nil(t, f.FetchAccountCreatedAt(context.Background(), "down.example", "alice"),
		"a host that did not answer was asked again after DropLocal")
	assert.Equal(t, int64(4), srv.hits.Load())
}

// A drop while a value is fetched keeps it out of the cache, and a call after
// the drop does not share the fetch started before it.
func TestRemoteStatsFetcher_DropDuringFetch(t *testing.T) {
	for name, read := range remoteStatsReads {
		t.Run(name, func(t *testing.T) {
			f, srv := newSeqUsersShowFetcher(t, true)

			var wg sync.WaitGroup
			var before int
			wg.Add(1)
			go func() {
				defer wg.Done()
				before = read(f)
			}()
			waitEntered(t, srv.entered)
			f.DropLocal()

			after := make(chan int, 1)
			go func() { after <- read(f) }()
			select {
			case got := <-after:
				assert.Equal(t, 2, got)
			case <-time.After(5 * time.Second):
				close(srv.release)
				t.Fatal("a call after the drop waited for the fetch started before it")
			}

			close(srv.release)
			wg.Wait()
			require.Equal(t, 1, before)
			assert.Equal(t, 2, read(f), "the value fetched before the drop was stored over the newer one")
		})
	}
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
