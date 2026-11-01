package federation_test

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hiddenNoteFetcher serves some notes to anyone (the instance actor) and others
// only to a GET signed with a local user's key, the way a server that decides by
// the signer answers followers-only notes (#3915).
type hiddenNoteFetcher struct {
	mu          sync.Mutex
	public      map[string]string
	hidden      map[string]string
	instanceErr *activitypub.StatusError // overrides the 404 for hidden notes when set
	actor       string                   // alice's actor document; sampleActor when empty
	keyedErr    error                    // the keyed fetch fails with this when set
	actorErr    error                    // alice's actor document fails with this when set
	delay       time.Duration            // every note fetch waits this long first
	plain       []string
	keyed       []string
	keyIDs      []string
}

func (f *hiddenNoteFetcher) FetchObject(uri string) ([]byte, error) {
	b, _, err := f.FetchObjectWithFinalURL(uri)
	return b, err
}

func (f *hiddenNoteFetcher) FetchObjectWithFinalURL(uri string) ([]byte, string, error) {
	if f.delay > 0 && uri != "https://remote.example/users/alice" {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plain = append(f.plain, uri)
	if uri == "https://remote.example/users/alice" {
		if f.actorErr != nil {
			return nil, "", f.actorErr
		}
		if f.actor != "" {
			return []byte(f.actor), uri, nil
		}
		return []byte(sampleActor), uri, nil
	}
	if b, ok := f.public[uri]; ok {
		return []byte(b), uri, nil
	}
	if _, ok := f.hidden[uri]; ok && f.instanceErr != nil {
		return nil, "", f.instanceErr
	}
	return nil, "", &activitypub.StatusError{StatusCode: http.StatusNotFound, URL: uri}
}

func (f *hiddenNoteFetcher) FetchObjectWithFinalURLSignedBy(uri string, key *activitypub.PrivateKey) ([]byte, string, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keyed = append(f.keyed, uri)
	f.keyIDs = append(f.keyIDs, key.KeyID)
	if f.keyedErr != nil {
		return nil, "", f.keyedErr
	}
	if b, ok := f.hidden[uri]; ok {
		return []byte(b), uri, nil
	}
	if b, ok := f.public[uri]; ok {
		return []byte(b), uri, nil
	}
	return nil, "", &activitypub.StatusError{StatusCode: http.StatusNotFound, URL: uri}
}

const (
	hiddenFollowersNote = `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/hidden",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "for followers",
		"to": ["https://remote.example/users/alice/followers"]
	}`
	// 宛先を 1 人も解決できない指名の投稿 (宛先は DB に無いリモートの actor)。
	hiddenSpecifiedNote = `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/direct",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "direct",
		"to": ["https://unknown.example/users/nobody"]
	}`
	publicQuotingHidden = `{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/quoting",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "quote",
		"to": ["https://www.w3.org/ns/activitystreams#Public"],
		"_misskey_quote": "https://remote.example/notes/hidden"
	}`
)

func newViewerKey(t *testing.T) *activitypub.PrivateKey {
	t.Helper()
	privPEM, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://example.com/users/viewer#main-key", privPEM)
	require.NoError(t, err)
	return key
}

func newHiddenNoteResolver(t *testing.T, f *hiddenNoteFetcher) (*federation.Resolver, *testutil.MockNoteRepository) {
	t.Helper()
	noteRepo := testutil.NewMockNoteRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return federation.NewResolverNilDeps(testutil.NewMockUserRepository(), noteRepo, activitypub.NewURLBuilder("https://example.com"), f, idGen), noteRepo
}

func TestResolveNoteWithKey_RetriesHiddenNoteWithViewerKey(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := &hiddenNoteFetcher{
				hidden:      map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote},
				instanceErr: &activitypub.StatusError{StatusCode: status},
			}
			r, _ := newHiddenNoteResolver(t, f)
			key := newViewerKey(t)

			n, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", key)
			require.NoError(t, err)
			assert.Equal(t, model.NoteVisibilityFollowers, n.Visibility, "visibility comes from the addressing")
			assert.Equal(t, []string{"https://remote.example/notes/hidden"}, f.keyed)
			assert.Equal(t, []string{key.KeyID}, f.keyIDs)
			assert.Contains(t, f.plain, "https://remote.example/notes/hidden", "the instance actor is tried first")
		})
	}
}

func TestResolveNoteWithKey_DoesNotRetryOtherwise(t *testing.T) {
	t.Run("no key", func(t *testing.T) {
		f := &hiddenNoteFetcher{hidden: map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote}}
		r, _ := newHiddenNoteResolver(t, f)
		_, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", nil)
		require.Error(t, err)
		_, err = r.ResolveNoteAllowCrossHost("https://remote.example/notes/hidden")
		require.Error(t, err)
		assert.Empty(t, f.keyed)
	})
	t.Run("visible to the instance actor", func(t *testing.T) {
		f := &hiddenNoteFetcher{public: map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote}}
		r, _ := newHiddenNoteResolver(t, f)
		_, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", newViewerKey(t))
		require.NoError(t, err)
		assert.Empty(t, f.keyed, "the viewer's key is not shown when it is not needed")
	})
	for _, status := range []int{http.StatusGone, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := &hiddenNoteFetcher{
				hidden:      map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote},
				instanceErr: &activitypub.StatusError{StatusCode: status},
			}
			r, _ := newHiddenNoteResolver(t, f)
			_, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", newViewerKey(t))
			require.Error(t, err)
			assert.Empty(t, f.keyed)
		})
	}
}

// A quote target is not retried with the key, even on the same host: ingest
// does not link a followers-only or specified quote target, so the retry would
// only show the key's owner to the remote server.
func TestResolveNoteWithKey_QuoteTargetIsNotRetried(t *testing.T) {
	f := &hiddenNoteFetcher{
		public: map[string]string{"https://remote.example/notes/quoting": publicQuotingHidden},
		hidden: map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote},
	}
	r, noteRepo := newHiddenNoteResolver(t, f)
	n, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/quoting", newViewerKey(t))
	require.NoError(t, err)
	assert.Nil(t, n.RenoteID)
	assert.Contains(t, f.plain, "https://remote.example/notes/hidden", "the quote target is tried with the instance actor (the test's premise)")
	assert.Empty(t, f.keyed)
	_, err = noteRepo.FindByURI("https://remote.example/notes/hidden")
	assert.Error(t, err)
}

// A specified note none of whose addressees could be resolved stays specified
// when fetched with the viewer's key. Elythia never turns it into public
// (upstream does so only for a note it fetched anonymously).
func TestResolveNoteWithKey_SpecifiedStaysSpecified(t *testing.T) {
	f := &hiddenNoteFetcher{hidden: map[string]string{"https://remote.example/notes/direct": hiddenSpecifiedNote}}
	r, _ := newHiddenNoteResolver(t, f)
	n, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/direct", newViewerKey(t))
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, n.Visibility)
	assert.Empty(t, n.VisibleUserIDs)
}

func TestHiddenFromInstanceActor(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusUnauthorized: true, http.StatusForbidden: true, http.StatusNotFound: true,
		http.StatusGone: false, http.StatusInternalServerError: false, http.StatusTooManyRequests: false,
	} {
		assert.Equal(t, want, federation.HiddenFromInstanceActor(&activitypub.StatusError{StatusCode: status}), status)
	}
	assert.False(t, federation.HiddenFromInstanceActor(nil))
	assert.False(t, federation.HiddenFromInstanceActor(assert.AnError))
}

// The key is not taken into actor resolution: the author's featured notes are
// fetched with the instance actor only, even when they are hidden too.
func TestResolveNoteWithKey_FeaturedIsNotFetchedWithTheKey(t *testing.T) {
	const featured = "https://remote.example/users/alice/collections/featured"
	const pinned = "https://remote.example/notes/pinned"
	f := &hiddenNoteFetcher{
		actor:  featuredActor("remote.example", "alice", featured),
		public: map[string]string{featured: featuredCollection(featured, "OrderedCollection", pinned)},
		hidden: map[string]string{
			"https://remote.example/notes/hidden": hiddenFollowersNote,
			pinned:                                strings.Replace(hiddenFollowersNote, "https://remote.example/notes/hidden", pinned, 1),
		},
	}
	r, _ := newHiddenNoteResolver(t, f)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r.SetPinningRepo(testutil.NewMockUserNotePiningRepository(), idGen)

	_, err = r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", newViewerKey(t))
	require.NoError(t, err)
	assert.Contains(t, f.plain, featured, "the featured collection is read (the test's premise)")
	assert.Contains(t, f.plain, pinned, "the pinned note is tried with the instance actor (the test's premise)")
	assert.Equal(t, []string{"https://remote.example/notes/hidden"}, f.keyed, "only the requested note uses the key")
}

// A quote target on another host is not retried with the key: the author of a
// note chooses its quote URLs, not the viewer.
func TestResolveNoteWithKey_QuoteTargetOnAnotherHostIsNotRetried(t *testing.T) {
	const third = "https://third.example/notes/q"
	f := &hiddenNoteFetcher{
		public: map[string]string{"https://remote.example/notes/quoting": strings.Replace(publicQuotingHidden, "https://remote.example/notes/hidden", third, 1)},
		hidden: map[string]string{third: hiddenFollowersNote},
	}
	r, _ := newHiddenNoteResolver(t, f)
	_, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/quoting", newViewerKey(t))
	require.NoError(t, err)
	assert.Contains(t, f.plain, third, "the quote target is tried with the instance actor (the test's premise)")
	assert.Empty(t, f.keyed)
}

// When the retry with the key fails too, the instance actor's error is
// returned, so a 404 stays permanent (an inbound job drops it instead of
// retrying) whatever the keyed fetch failed with.
func TestResolveNoteWithKey_FailedRetryKeepsTheInstanceActorError(t *testing.T) {
	for name, keyedErr := range map[string]error{
		"server error":     &activitypub.StatusError{StatusCode: http.StatusInternalServerError},
		"refused redirect": &activitypub.StatusError{StatusCode: http.StatusFound},
		"network":          assert.AnError,
	} {
		t.Run(name, func(t *testing.T) {
			f := &hiddenNoteFetcher{hidden: map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote}, keyedErr: keyedErr}
			r, _ := newHiddenNoteResolver(t, f)
			_, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", newViewerKey(t))
			require.Error(t, err)
			assert.Len(t, f.keyed, 1, "retried")
			var se *activitypub.StatusError
			require.ErrorAs(t, err, &se)
			assert.Equal(t, http.StatusNotFound, se.StatusCode)
		})
	}
}

// Only the note's own fetch being refused triggers the retry: a note the
// instance actor can read is not fetched with the key even when ingesting it
// fails with a 401 / 403 / 404 elsewhere (its author's actor here).
func TestResolveNoteWithKey_VisibleNoteIsNotRetriedOnIngestFailure(t *testing.T) {
	f := &hiddenNoteFetcher{
		public:   map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote},
		actorErr: &activitypub.StatusError{StatusCode: http.StatusUnauthorized},
	}
	r, _ := newHiddenNoteResolver(t, f)
	_, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", newViewerKey(t))
	require.Error(t, err)
	assert.Empty(t, f.keyed)
}

// A failure after the keyed fetch succeeded (its author's actor answering 503)
// is returned as is, so an inbound job retries it instead of dropping the
// activity as a permanent 404.
func TestResolveNoteWithKey_IngestFailureAfterRetryIsReturned(t *testing.T) {
	f := &hiddenNoteFetcher{
		hidden:   map[string]string{"https://remote.example/notes/hidden": hiddenFollowersNote},
		actorErr: &activitypub.StatusError{StatusCode: http.StatusServiceUnavailable},
	}
	r, _ := newHiddenNoteResolver(t, f)
	_, err := r.ResolveNoteAllowCrossHostWithKey("https://remote.example/notes/hidden", newViewerKey(t))
	require.Error(t, err)
	assert.Len(t, f.keyed, 1)
	var se *activitypub.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusServiceUnavailable, se.StatusCode)
}

// The first stage (the instance actor's fetch) is one call for resolves with
// and without a key, and the second stage is one call per key.
func TestResolveNoteWithKey_StagesAreSharedPerKey(t *testing.T) {
	const uri = "https://remote.example/notes/hidden"
	// keyedErr を返すと 2 段目は取り込みまで進まない。違う鍵の 2 段目は並行して
	// 走るので、取り込みまで進ませると mock の note repo (素の map) を 2 つの
	// goroutine が同時に書く。
	run := func(t *testing.T, keyedErr error, keys ...*activitypub.PrivateKey) *hiddenNoteFetcher {
		t.Helper()
		f := &hiddenNoteFetcher{hidden: map[string]string{uri: hiddenFollowersNote}, delay: 300 * time.Millisecond, keyedErr: keyedErr}
		r, _ := newHiddenNoteResolver(t, f)
		var wg sync.WaitGroup
		for _, key := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, _ = r.ResolveNoteAllowCrossHostWithKey(uri, key)
			}()
		}
		wg.Wait()
		return f
	}
	countPlain := func(f *hiddenNoteFetcher) int {
		n := 0
		for _, u := range f.plain {
			if u == uri {
				n++
			}
		}
		return n
	}

	t.Run("with and without a key", func(t *testing.T) {
		f := run(t, nil, newViewerKey(t), nil)
		assert.Equal(t, 1, countPlain(f), "one instance actor fetch for both")
	})
	t.Run("two different keys", func(t *testing.T) {
		other := newViewerKey(t)
		other.KeyID = "https://example.com/users/other#main-key"
		f := run(t, &activitypub.StatusError{StatusCode: http.StatusNotFound}, newViewerKey(t), other)
		assert.Equal(t, 1, countPlain(f), "one instance actor fetch for both")
		assert.ElementsMatch(t, []string{"https://example.com/users/viewer#main-key", other.KeyID}, f.keyIDs, "each key retries on its own")
	})
	t.Run("the same key", func(t *testing.T) {
		key := newViewerKey(t)
		f := run(t, nil, key, key)
		assert.Len(t, f.keyIDs, 1, "one retry for the same key")
	})
}
