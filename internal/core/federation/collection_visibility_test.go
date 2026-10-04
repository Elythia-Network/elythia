package federation_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

const (
	cvActorURI     = "https://remote.example/users/carol"
	cvFollowersURI = "https://remote.example/users/carol/followers"
	cvFollowingURI = "https://remote.example/users/carol/following"
)

// collectionFetcher serves canned documents and errors per URI and records
// every request. Unknown URIs fail like a network error.
// followers と following は並行に取りに行くので、記録は mutex で守る。
type collectionFetcher struct {
	mu    sync.Mutex
	docs  map[string]string
	errs  map[string]error
	calls []string
	// onFetch, if set, runs on every request outside mu, so it may block to
	// observe concurrent requests.
	onFetch func(uri string)
}

func (f *collectionFetcher) FetchObject(uri string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, uri)
	onFetch := f.onFetch
	f.mu.Unlock()
	if onFetch != nil {
		onFetch(uri)
	}
	if err, ok := f.errs[uri]; ok {
		return nil, err
	}
	if body, ok := f.docs[uri]; ok {
		return []byte(body), nil
	}
	return nil, errors.New("no fixture for " + uri)
}

// cvActorDoc renders carol's actor document. followers / following are raw
// JSON values; "" omits the key.
func cvActorDoc(followers, following string) string {
	extra := ""
	if followers != "" {
		extra += `,"followers":` + followers
	}
	if following != "" {
		extra += `,"following":` + following
	}
	return `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "` + cvActorURI + `",
	"type": "Person",
	"preferredUsername": "carol",
	"inbox": "https://remote.example/users/carol/inbox",
	"publicKey": {
		"id": "` + cvActorURI + `#main-key",
		"owner": "` + cvActorURI + `",
		"publicKeyPem": "-----BEGIN PUBLIC KEY-----\nFAKE\n-----END PUBLIC KEY-----"
	}` + extra + `}`
}

// cvCollection renders a fetched collection document with the given id and
// extra members (e.g. `"first":"..."`).
func cvCollection(collectionID, typ, members string) string {
	doc := `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + collectionID + `","type":"` + typ + `"`
	if members != "" {
		doc += "," + members
	}
	return doc + "}"
}

func newCollectionResolver(t *testing.T, f *collectionFetcher) (*federation.Resolver, *testutil.MockUserRepository) {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(),
		activitypub.NewURLBuilder("https://example.com"), f, idGen)
	return r, repo
}

func cvProfile(t *testing.T, repo *testutil.MockUserRepository, userID string) *model.UserProfile {
	t.Helper()
	p, ok := repo.Profiles[userID]
	require.True(t, ok, "profile must exist")
	return p
}

func quoted(s string) string { return fmt.Sprintf("%q", s) }

// TestResolveActor_FollowCollectionVisibilityOnCreate covers upstream
// createPerson's isPublicCollection rule for newly imported actors.
func TestResolveActor_FollowCollectionVisibilityOnCreate(t *testing.T) {
	const (
		pub  = model.FollowingVisibilityPublic
		priv = model.FollowingVisibilityPrivate
	)
	cases := []struct {
		name        string
		followers   string // raw JSON in the actor; "" = absent
		followerDoc string // fetched followers document; "" = no fixture
		followerErr error
		want        model.FollowingVisibility
	}{
		{name: "collection with first is public", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection(cvFollowersURI, "OrderedCollection", `"totalItems":3,"first":"`+cvFollowersURI+`?page=1"`), want: pub},
		{name: "collection with items is public", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection(cvFollowersURI, "Collection", `"items":["https://remote.example/users/x"]`), want: pub},
		{name: "empty items array is still public (JS truthiness)", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection(cvFollowersURI, "Collection", `"items":[]`), want: pub},
		{name: "ordered collection with orderedItems is public", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection(cvFollowersURI, "OrderedCollection", `"orderedItems":[]`), want: pub},
		{name: "type given as array", followers: quoted(cvFollowersURI),
			followerDoc: `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + cvFollowersURI + `","type":["OrderedCollection"],"first":"x"}`, want: pub},
		{name: "only totalItems is private", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection(cvFollowersURI, "OrderedCollection", `"totalItems":12`), want: priv},
		{name: "null first is private", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection(cvFollowersURI, "OrderedCollection", `"first":null,"items":"","orderedItems":0`), want: priv},
		{name: "absent collection is private", want: priv},
		{name: "null collection is private", followers: `null`, want: priv},
		{name: "network error is private", followers: quoted(cvFollowersURI),
			followerErr: errors.New("connection reset"), want: priv},
		{name: "server error is private", followers: quoted(cvFollowersURI),
			followerErr: &activitypub.StatusError{StatusCode: 503, Status: "503 Service Unavailable"}, want: priv},
		{name: "forbidden is private", followers: quoted(cvFollowersURI),
			followerErr: &activitypub.StatusError{StatusCode: 403, Status: "403 Forbidden"}, want: priv},
		{name: "non-collection document is private", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection(cvFollowersURI, "Person", `"first":"x"`), want: priv},
		{name: "document without AS context is private", followers: quoted(cvFollowersURI),
			followerDoc: `{"id":"` + cvFollowersURI + `","type":"OrderedCollection","first":"x"}`, want: priv},
		{name: "document id on another host is private", followers: quoted(cvFollowersURI),
			followerDoc: cvCollection("https://other.example/followers", "OrderedCollection", `"first":"x"`), want: priv},
		{name: "embedded collection with first is public", followers: `{"id":"` + cvFollowersURI + `","type":"OrderedCollection","first":"x"}`, want: pub},
		{name: "embedded collection with only totalItems is private", followers: `{"id":"` + cvFollowersURI + `","type":"OrderedCollection","totalItems":5}`, want: priv},
		{name: "embedded non-collection is private", followers: `{"id":"` + cvFollowersURI + `","type":"Note","first":"x"}`, want: priv},
		{name: "embedded collection without id is private", followers: `{"type":"OrderedCollection","first":"x"}`, want: priv},
		// 別ホストの collection は actor ごと拒否する (TestResolveActor_CrossHostCollectionRejectsActor)。
		{name: "array value is private", followers: `["` + cvFollowersURI + `"]`, want: priv},
		{name: "fragment URL is private", followers: quoted(cvFollowersURI + "#x"),
			followerDoc: cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`), want: priv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// following は常に公開の collection にして、followers 側の判定だけを
			// 変える (列の取り違えも検出する)。
			f := &collectionFetcher{
				docs: map[string]string{
					cvActorURI:     cvActorDoc(tc.followers, quoted(cvFollowingURI)),
					cvFollowingURI: cvCollection(cvFollowingURI, "OrderedCollection", `"first":"x"`),
				},
				errs: map[string]error{},
			}
			if tc.followerDoc != "" {
				f.docs[cvFollowersURI] = tc.followerDoc
			}
			if tc.followerErr != nil {
				f.errs[cvFollowersURI] = tc.followerErr
			}
			r, repo := newCollectionResolver(t, f)
			user, err := r.ResolveActor(cvActorURI)
			require.NoError(t, err)
			p := cvProfile(t, repo, user.ID)
			assert.Equal(t, tc.want, p.FollowersVisibility, "followersVisibility")
			assert.Equal(t, pub, p.FollowingVisibility, "followingVisibility follows the following collection")
		})
	}
}

// TestResolveActor_FollowingCollectionReadSeparately checks that the
// following collection drives followingVisibility independently.
func TestResolveActor_FollowingCollectionReadSeparately(t *testing.T) {
	f := &collectionFetcher{
		docs: map[string]string{
			cvActorURI:     cvActorDoc(quoted(cvFollowersURI), quoted(cvFollowingURI)),
			cvFollowersURI: cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`),
			cvFollowingURI: cvCollection(cvFollowingURI, "OrderedCollection", `"totalItems":3`),
		},
	}
	r, repo := newCollectionResolver(t, f)
	user, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	p := cvProfile(t, repo, user.ID)
	assert.Equal(t, model.FollowingVisibilityPublic, p.FollowersVisibility)
	assert.Equal(t, model.FollowingVisibilityPrivate, p.FollowingVisibility)
	assert.Contains(t, f.calls, cvFollowersURI)
	assert.Contains(t, f.calls, cvFollowingURI)
}

// TestRefreshActor_FollowCollectionVisibilityOnUpdate covers upstream
// updatePerson: transient failures keep the stored value, non-retryable
// statuses make the list private, and successful lookups overwrite it.
func TestRefreshActor_FollowCollectionVisibilityOnUpdate(t *testing.T) {
	const (
		pub  = model.FollowingVisibilityPublic
		priv = model.FollowingVisibilityPrivate
	)
	publicDoc := cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`)
	privateDoc := cvCollection(cvFollowersURI, "OrderedCollection", `"totalItems":1`)
	cases := []struct {
		name    string
		initial string // followers document at create time
		doc     string // followers document at refresh time ("" = none)
		err     error  // followers fetch error at refresh time
		want    model.FollowingVisibility
	}{
		{name: "network error keeps public", initial: publicDoc, err: errors.New("timeout"), want: pub},
		{name: "network error keeps private", initial: privateDoc, err: errors.New("timeout"), want: priv},
		{name: "5xx keeps the stored value", initial: publicDoc,
			err: &activitypub.StatusError{StatusCode: 502, Status: "502 Bad Gateway"}, want: pub},
		{name: "429 keeps the stored value", initial: publicDoc,
			err: &activitypub.StatusError{StatusCode: 429, Status: "429 Too Many Requests"}, want: pub},
		{name: "malformed document keeps the stored value", initial: publicDoc,
			doc: cvCollection(cvFollowersURI, "Person", ""), want: pub},
		{name: "404 makes it private", initial: publicDoc,
			err: &activitypub.StatusError{StatusCode: 404, Status: "404 Not Found"}, want: priv},
		{name: "403 makes it private", initial: publicDoc,
			err: &activitypub.StatusError{StatusCode: 403, Status: "403 Forbidden"}, want: priv},
		{name: "collection turned private", initial: publicDoc, doc: privateDoc, want: priv},
		{name: "collection turned public", initial: privateDoc, doc: publicDoc, want: pub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &collectionFetcher{
				docs: map[string]string{
					cvActorURI:     cvActorDoc(quoted(cvFollowersURI), ""),
					cvFollowersURI: tc.initial,
				},
				errs: map[string]error{},
			}
			r, repo := newCollectionResolver(t, f)
			user, err := r.ResolveActor(cvActorURI)
			require.NoError(t, err)
			initial := cvProfile(t, repo, user.ID).FollowersVisibility

			delete(f.docs, cvFollowersURI)
			if tc.doc != "" {
				f.docs[cvFollowersURI] = tc.doc
			}
			if tc.err != nil {
				f.errs[cvFollowersURI] = tc.err
			}
			_, err = r.ForceResolveActor(cvActorURI)
			require.NoError(t, err)
			assert.Equal(t, tc.want, cvProfile(t, repo, user.ID).FollowersVisibility,
				"initial=%s", initial)
			// following は actor から消えているので非公開 (absent → private)。
			assert.Equal(t, priv, cvProfile(t, repo, user.ID).FollowingVisibility)
		})
	}
}

// TestRefreshActor_BackfilledProfileTransientErrorIsPrivate checks that a
// profile created during refresh (no stored value yet) falls back to private
// on a transient failure instead of the column default.
func TestRefreshActor_BackfilledProfileTransientErrorIsPrivate(t *testing.T) {
	f := &collectionFetcher{
		docs: map[string]string{
			cvActorURI:     cvActorDoc(quoted(cvFollowersURI), quoted(cvFollowingURI)),
			cvFollowersURI: cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`),
			cvFollowingURI: cvCollection(cvFollowingURI, "OrderedCollection", `"first":"x"`),
		},
		errs: map[string]error{},
	}
	r, repo := newCollectionResolver(t, f)
	user, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	delete(repo.Profiles, user.ID)

	f.errs[cvFollowersURI] = errors.New("timeout")
	_, err = r.ForceResolveActor(cvActorURI)
	require.NoError(t, err)
	p := cvProfile(t, repo, user.ID)
	assert.Equal(t, model.FollowingVisibilityPrivate, p.FollowersVisibility)
	assert.Equal(t, model.FollowingVisibilityPublic, p.FollowingVisibility)
}

// TestResolveActor_CrossHostCollectionRejectsActor checks that an actor whose
// outbox / followers / following is on another host is rejected as a whole and
// the collection is never fetched, as upstream ApPersonService.validateActor
// throws `invalid Actor: ${collection} has different host` (#3330).
func TestResolveActor_CrossHostCollectionRejectsActor(t *testing.T) {
	const other = "https://other.example/followers"
	cases := []struct {
		name  string
		extra string
	}{
		{"followers IRI", `,"followers":"` + other + `"`},
		{"embedded followers", `,"followers":{"id":"` + other + `","type":"OrderedCollection","first":"x"}`},
		{"followers on this instance", `,"followers":"https://example.com/users/abc/followers"`},
		{"following IRI", `,"following":"` + other + `"`},
		{"outbox IRI", `,"outbox":"https://other.example/outbox"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := cvActorDoc("", "")
			doc = doc[:len(doc)-1] + tc.extra + "}"
			f := &collectionFetcher{
				docs: map[string]string{
					cvActorURI: doc,
					other:      cvCollection(other, "OrderedCollection", `"first":"x"`),
				},
			}
			r, repo := newCollectionResolver(t, f)
			_, err := r.ResolveActor(cvActorURI)
			require.ErrorIs(t, err, federation.ErrInvalidActor)
			assert.Empty(t, repo.Users, "actor を作らない")
			assert.NotContains(t, f.calls, other)
		})
	}

	t.Run("same host collections are accepted", func(t *testing.T) {
		doc := cvActorDoc(quoted(cvFollowersURI), quoted(cvFollowingURI))
		doc = doc[:len(doc)-1] + `,"outbox":"https://remote.example/users/carol/outbox"}`
		f := &collectionFetcher{docs: map[string]string{cvActorURI: doc}, errs: map[string]error{}}
		r, _ := newCollectionResolver(t, f)
		_, err := r.ResolveActor(cvActorURI)
		require.NoError(t, err)
	})
}

// #3330: a refresh whose actor now declares a cross-host collection is
// rejected like creation (upstream updatePerson also runs validateActor), so
// the stored followersUri is not replaced with the other host's value.
func TestRefreshActor_CrossHostCollectionKeepsStoredActor(t *testing.T) {
	f := &collectionFetcher{
		docs: map[string]string{
			cvActorURI:     cvActorDoc(quoted(cvFollowersURI), ""),
			cvFollowersURI: cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`),
		},
		errs: map[string]error{},
	}
	r, repo := newCollectionResolver(t, f)
	user, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	require.NotNil(t, repo.Users[user.ID].FollowersURI)

	f.docs[cvActorURI] = cvActorDoc(quoted("https://other.example/followers"), "")
	_, _ = r.ForceResolveActor(cvActorURI)
	require.NotNil(t, repo.Users[user.ID].FollowersURI)
	assert.Equal(t, cvFollowersURI, *repo.Users[user.ID].FollowersURI, "別ホストの followers で上書きしない")
}

// TestResolveActor_FragmentCollectionNotFetched checks that a collection IRI
// with a fragment is not fetched (upstream Resolver refuses such URLs).
func TestResolveActor_FragmentCollectionNotFetched(t *testing.T) {
	withFragment := cvFollowersURI + "#x"
	f := &collectionFetcher{
		docs: map[string]string{
			cvActorURI:   cvActorDoc(quoted(withFragment), ""),
			withFragment: cvCollection(withFragment, "OrderedCollection", `"first":"x"`),
		},
	}
	r, repo := newCollectionResolver(t, f)
	user, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	assert.Equal(t, model.FollowingVisibilityPrivate, cvProfile(t, repo, user.ID).FollowersVisibility)
	assert.NotContains(t, f.calls, withFragment)
}

// 一覧の公開範囲は user 行を作る前に取る。後にすると、取得の間 profile の無い
// user 行が残り、その間は公開範囲が未設定 (= public 扱い) になる。
func TestResolveActor_CollectionsFetchedBeforeUserRowIsCreated(t *testing.T) {
	f := &collectionFetcher{
		docs: map[string]string{
			cvActorURI:     cvActorDoc(quoted(cvFollowersURI), quoted(cvFollowingURI)),
			cvFollowersURI: cvCollection(cvFollowersURI, "OrderedCollection", `"totalItems":3`),
			cvFollowingURI: cvCollection(cvFollowingURI, "OrderedCollection", `"totalItems":3`),
		},
	}
	r, repo := newCollectionResolver(t, f)
	var mu sync.Mutex
	var rowsAtFetch []int
	f.onFetch = func(uri string) {
		if uri == cvFollowersURI || uri == cvFollowingURI {
			mu.Lock()
			defer mu.Unlock()
			rowsAtFetch = append(rowsAtFetch, len(repo.Users))
		}
	}
	_, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	assert.Equal(t, []int{0, 0}, rowsAtFetch, "collection fetched after the user row was created")
}

// キーは大文字小文字まで完全一致で引く (upstream の `person.followers` と同じ)。
// `"Followers"` は followers ではないので、一覧は無いもの (非公開) として扱う。
func TestResolveActor_FollowCollectionKeysAreCaseSensitive(t *testing.T) {
	doc := strings.Replace(cvActorDoc(quoted(cvFollowersURI), quoted(cvFollowingURI)), `"followers":`, `"Followers":`, 1)
	f := &collectionFetcher{
		docs: map[string]string{
			cvActorURI:     doc,
			cvFollowersURI: cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`),
			cvFollowingURI: cvCollection(cvFollowingURI, "OrderedCollection", `"first":"x"`),
		},
	}
	r, repo := newCollectionResolver(t, f)
	user, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	p := cvProfile(t, repo, user.ID)
	assert.Equal(t, model.FollowingVisibilityPrivate, p.FollowersVisibility)
	assert.Equal(t, model.FollowingVisibilityPublic, p.FollowingVisibility)
	assert.NotContains(t, f.calls, cvFollowersURI)
}

// collectionBarrier makes each followers / following request wait until the
// other one has started too. Sequential fetching never satisfies it, so the
// request times out and the barrier records the failure.
type collectionBarrier struct {
	arrived chan struct{}
	timeout time.Duration
	mu      sync.Mutex
	stalled bool
}

func newCollectionBarrier() *collectionBarrier {
	return &collectionBarrier{arrived: make(chan struct{}, 2), timeout: 5 * time.Second}
}

func (b *collectionBarrier) onFetch(uri string) {
	if uri != cvFollowersURI && uri != cvFollowingURI {
		return
	}
	// 3 回目以降の取得 (将来のリトライなど) で詰まらないよう、満杯なら捨てる。
	select {
	case b.arrived <- struct{}{}:
	default:
	}
	deadline := time.After(b.timeout)
	for {
		if len(b.arrived) == 2 {
			return
		}
		select {
		case <-deadline:
			b.mu.Lock()
			b.stalled = true
			b.mu.Unlock()
			return
		case <-time.After(time.Millisecond):
		}
	}
}

func (b *collectionBarrier) wasStalled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stalled
}

// upstream は 2 本を Promise.all で並べる。直列だと遅い相手で取り込みと更新が
// 2 回分の timeout だけ延びるので、両方が同時に走っていることを確かめる。
func TestResolveActor_FollowCollectionsFetchedConcurrently(t *testing.T) {
	newFetcher := func() *collectionFetcher {
		return &collectionFetcher{
			docs: map[string]string{
				cvActorURI:     cvActorDoc(quoted(cvFollowersURI), quoted(cvFollowingURI)),
				cvFollowersURI: cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`),
				cvFollowingURI: cvCollection(cvFollowingURI, "OrderedCollection", `"first":"x"`),
			},
		}
	}
	t.Run("create", func(t *testing.T) {
		f := newFetcher()
		b := newCollectionBarrier()
		f.onFetch = b.onFetch
		r, _ := newCollectionResolver(t, f)
		_, err := r.ResolveActor(cvActorURI)
		require.NoError(t, err)
		assert.False(t, b.wasStalled(), "followers / following were fetched sequentially")
	})
	t.Run("update", func(t *testing.T) {
		f := newFetcher()
		r, _ := newCollectionResolver(t, f)
		_, err := r.ResolveActor(cvActorURI)
		require.NoError(t, err)
		b := newCollectionBarrier()
		f.mu.Lock()
		f.onFetch = b.onFetch
		f.mu.Unlock()
		_, err = r.ForceResolveActor(cvActorURI)
		require.NoError(t, err)
		assert.False(t, b.wasStalled(), "followers / following were fetched sequentially")
	})
}

// 並行に取る側の goroutine で panic しても、プロセスを落とさずその一覧を
// 非公開 (作成時) / 保存値の維持 (更新時) に倒す。
func TestResolveActor_FollowCollectionPanicIsContained(t *testing.T) {
	f := &collectionFetcher{
		docs: map[string]string{
			cvActorURI:     cvActorDoc(quoted(cvFollowersURI), quoted(cvFollowingURI)),
			cvFollowersURI: cvCollection(cvFollowersURI, "OrderedCollection", `"first":"x"`),
			cvFollowingURI: cvCollection(cvFollowingURI, "OrderedCollection", `"first":"x"`),
		},
	}
	r, repo := newCollectionResolver(t, f)
	user, err := r.ResolveActor(cvActorURI)
	require.NoError(t, err)
	require.Equal(t, model.FollowingVisibilityPublic, cvProfile(t, repo, user.ID).FollowersVisibility)

	f.mu.Lock()
	f.onFetch = func(uri string) {
		if uri == cvFollowersURI {
			panic("boom")
		}
	}
	f.mu.Unlock()
	_, err = r.ForceResolveActor(cvActorURI)
	require.NoError(t, err)
	p := cvProfile(t, repo, user.ID)
	assert.Equal(t, model.FollowingVisibilityPublic, p.FollowersVisibility, "update keeps the stored value")
	assert.Equal(t, model.FollowingVisibilityPublic, p.FollowingVisibility)

	f2 := &collectionFetcher{
		docs:    f.docs,
		onFetch: f.onFetch,
	}
	r2, repo2 := newCollectionResolver(t, f2)
	user2, err := r2.ResolveActor(cvActorURI)
	require.NoError(t, err)
	p2 := cvProfile(t, repo2, user2.ID)
	assert.Equal(t, model.FollowingVisibilityPrivate, p2.FollowersVisibility, "create falls back to private")
	assert.Equal(t, model.FollowingVisibilityPublic, p2.FollowingVisibility)
}
