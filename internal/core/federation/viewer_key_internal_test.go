package federation

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A GET signed with a user's key carries that key and never falls back to an
// unsigned request (#3915).
func TestAPFetcher_FetchObjectSignedBy(t *testing.T) {
	var keyIDs []string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parsed, err := activitypub.ParseSignatureHeader(r.Header.Get("Signature"))
		if err != nil {
			keyIDs = append(keyIDs, "")
		} else {
			keyIDs = append(keyIDs, parsed.KeyID)
		}
		w.Header().Set("Content-Type", "application/activity+json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	instanceKey := genTestRSAKey(t)
	f := NewAPFetcher(activitypub.NewClient(nil, "test"))
	f.SetSigner(&stubSigner{key: instanceKey})
	userKey := genTestRSAKey(t)
	userKey.KeyID = "https://local.example/users/viewer#main-key"

	body, err := f.FetchObjectSignedBy(srv.URL+"/notes/1", userKey)
	require.NoError(t, err)
	assert.Contains(t, string(body), "x")
	assert.Equal(t, []string{userKey.KeyID}, keyIDs, "signed by the user, not the instance actor")

	keyIDs, status = nil, http.StatusUnauthorized
	_, err = f.FetchObjectSignedBy(srv.URL+"/notes/1", userKey)
	require.Error(t, err)
	assert.Equal(t, []string{userKey.KeyID}, keyIDs, "no unsigned fallback")

	_, _, err = f.FetchObjectWithFinalURLSignedBy(srv.URL+"/notes/1", nil)
	assert.ErrorIs(t, err, ErrNoSigner)

	// 別のホストへの redirect は追わない (照会した人の keyId を飛び先へ渡さない)。
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/notes/1", http.StatusFound)
	}))
	defer away.Close()
	keyIDs, status = nil, http.StatusOK
	_, err = f.FetchObjectSignedBy(away.URL+"/notes/1", userKey)
	require.Error(t, err)
	assert.Empty(t, keyIDs, "the other host is never asked")
}

// The user's key is carried to every branch of a note resolve chain but not
// into actor resolution, and is used only for its host. How the two stages
// share singleflight calls is pinned by TestResolveNoteWithKey_StagesAreSharedPerKey.
func TestResolveChain_CarriesFetchKey(t *testing.T) {
	const uri = "https://remote.example/notes/1"
	key := genTestRSAKey(t)
	root := &resolveChain{fetchKey: newFetchKeySource(key, uri)}
	assert.Same(t, key, root.fetchKeyFor(uri))
	assert.Same(t, key, root.with("k", "").fetchKeyFor(uri))
	assert.Same(t, key, root.asBestEffort().fetchKeyFor(uri))
	assert.Same(t, key, root.ensureTree().fetchKeyFor(uri))
	assert.Same(t, key, root.with("k", "").asBestEffort().with("k2", "d").fetchKeyFor(uri))
	assert.Same(t, key, root.fetchKeyFor("https://REMOTE.example:443/notes/2"), "same host in another spelling")
	assert.Nil(t, root.fetchKeyFor("https://third.example/notes/1"), "another host")
	assert.Nil(t, root.fetchKeyFor("https://sub.remote.example/notes/1"), "a subdomain is another host")
	assert.Nil(t, root.withoutFetchKey().fetchKeyFor(uri))
	assert.Same(t, key, root.fetchKeyFor(uri), "withoutFetchKey does not change the original")
	assert.Nil(t, (*resolveChain)(nil).fetchKeyFor(uri))
	assert.Nil(t, newFetchKeySource(nil, uri))
	assert.Nil(t, newFetchKeySource(key, "not a uri"))

	// 1 段目 (instance actor) は鍵の有無に関わらず同じ呼び出しにまとまる。
	assert.Equal(t, noteGroupKeyForChain(uri, true, false, nil), noteGroupKeyForChain(uri, true, false, root))
}
