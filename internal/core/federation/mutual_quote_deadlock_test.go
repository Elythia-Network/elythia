package federation

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
)

// barrierFetcher holds every gated URI until all of them have been requested,
// so two goroutines are guaranteed to be inside their own singleflight entry
// at the same time.
type barrierFetcher struct {
	docs  map[string]string
	gated map[string]bool

	mu      sync.Mutex
	arrived map[string]bool
	ready   chan struct{}
	once    sync.Once
}

func (f *barrierFetcher) FetchObject(uri string) ([]byte, error) {
	if f.gated[uri] {
		f.mu.Lock()
		f.arrived[uri] = true
		all := len(f.arrived) == len(f.gated)
		f.mu.Unlock()
		if all {
			f.once.Do(func() { close(f.ready) })
		}
		select {
		case <-f.ready:
		case <-time.After(10 * time.Second):
		}
	}
	if b, ok := f.docs[uri]; ok {
		return []byte(b), nil
	}
	return nil, errors.New("no fixture for " + uri)
}

// REVIEW: two goroutines resolving notes that quote each other now block on
// each other's singleflight entry forever (ABBA). The process-wide ledger on
// develop made the second one give up instead of waiting.
func TestResolveNote_MutualQuoteAcrossGoroutinesDoesNotDeadlock(t *testing.T) {
	const (
		actorURI = "https://remote.example/users/pat"
		noteA    = "https://remote.example/notes/a1"
		noteB    = "https://remote.example/notes/b1"
	)
	f := &barrierFetcher{
		docs: map[string]string{
			actorURI: featuredActorDoc("remote.example", "pat", ""),
			noteA:    quoteNoteDoc(noteA, actorURI, noteB),
			noteB:    quoteNoteDoc(noteB, actorURI, noteA),
		},
		gated:   map[string]bool{noteA: true, noteB: true},
		arrived: map[string]bool{},
		ready:   make(chan struct{}),
	}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(), urls, f, idGen)

	done := make(chan error, 2)
	for _, u := range []string{noteA, noteB} {
		go func(uri string) {
			_, e := r.ResolveNote(uri)
			done <- e
		}(u)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(25 * time.Second):
			t.Fatalf("DEADLOCK: %d/2 returned; mutual quote across goroutines wedges both", i)
		}
	}
}
