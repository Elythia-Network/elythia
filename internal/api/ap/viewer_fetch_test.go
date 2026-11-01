package ap

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/api/notehide"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hiddenNoteURI = "https://remote.example/notes/hidden"

// keyedFetcher hides the note from the instance actor (instanceErr) and serves
// it to a GET signed with a user's key.
type keyedFetcher struct {
	instanceErr   error
	keyedErr      error
	keyIDs        []string
	instanceCalls int
}

func (f *keyedFetcher) FetchObject(string) ([]byte, error) {
	f.instanceCalls++
	if f.instanceErr != nil {
		return nil, f.instanceErr
	}
	return []byte(hiddenNoteDoc), nil
}

func (f *keyedFetcher) FetchObjectSignedBy(_ string, key *activitypub.PrivateKey) ([]byte, error) {
	f.keyIDs = append(f.keyIDs, key.KeyID)
	if f.keyedErr != nil {
		return nil, f.keyedErr
	}
	return []byte(hiddenNoteDoc), nil
}

const hiddenNoteDoc = `{"@context":"https://www.w3.org/ns/activitystreams","id":"` + hiddenNoteURI + `","type":"Note","content":"secret"}`

// keyedResolver records which resolve was used.
type keyedResolver struct {
	mockResolver
	keyedWith []string
	plain     int
}

func (r *keyedResolver) ResolveNoteAllowCrossHost(uri string) (*model.Note, error) {
	r.plain++
	return r.mockResolver.ResolveNoteAllowCrossHost(uri)
}

func (r *keyedResolver) ResolveNoteAllowCrossHostWithKey(_ string, key *activitypub.PrivateKey) (*model.Note, error) {
	r.keyedWith = append(r.keyedWith, key.KeyID)
	return r.note, r.noteErr
}

type viewerFetchFixture struct {
	h         *Handler
	fetcher   *keyedFetcher
	resolver  *keyedResolver
	following *testutil.MockFollowingRepository
	keypairs  *memoryKeypairRepo
	notes     *testutil.MockNoteRepository
	viewer    *model.User
	resolved  *model.Note
}

func newViewerFetchFixture(t *testing.T) *viewerFetchFixture {
	t.Helper()
	users := testutil.NewMockUserRepository()
	notes := testutil.NewMockNoteRepository()
	following := testutil.NewMockFollowingRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	userSvc := coreuser.NewService(users, notes, testutil.NewMockUserNotePiningRepository(), idGen, nil)
	keypairs := &memoryKeypairRepo{items: map[string]*model.UserKeypair{}}
	h := NewHandler(activitypub.NewRenderer(activitypub.NewURLBuilder("https://local.example")), userSvc,
		corenote.NewQueryService(notes, following), keypairs, idGen,
		notehide.NewHider(following, nil), nil, apShowHostBlocker{}, "local.example", nil, nil)

	privPEM, _, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	viewer := &model.User{ID: "viewer", Username: "viewer"}
	keypairs.items[viewer.ID] = &model.UserKeypair{UserID: viewer.ID, PrivateKey: privPEM}

	host, uri := "remote.example", "https://remote.example/users/alice"
	alice := &model.User{ID: "alice", Username: "alice", Host: &host, URI: &uri}
	noteURI := hiddenNoteURI
	text := "secret"
	resolved := &model.Note{ID: "rn", UserID: alice.ID, User: alice, UserHost: &host, URI: &noteURI,
		Text: &text, Visibility: model.NoteVisibilityFollowers}

	fetcher := &keyedFetcher{instanceErr: &activitypub.StatusError{StatusCode: http.StatusNotFound}}
	resolver := &keyedResolver{mockResolver: mockResolver{note: resolved, err: errors.New("not an actor")}}
	h.SetRemote(fetcher, resolver)
	h.SetFollowingRepo(following)
	h.SetNoteRepo(notes)
	return &viewerFetchFixture{h: h, fetcher: fetcher, resolver: resolver, following: following, keypairs: keypairs, notes: notes, viewer: viewer, resolved: resolved}
}

func (f *viewerFetchFixture) show(t *testing.T) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"uri":"`+hiddenNoteURI+`"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.Set(string(middleware.UserContextKey), f.viewer)
	require.NoError(t, f.h.APIShow(c))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

// follow makes the viewer follow alice, the author on remote.example.
func (f *viewerFetchFixture) follow(t *testing.T) {
	t.Helper()
	host := "remote.example"
	require.NoError(t, f.following.Create(&model.Following{ID: "f1", FollowerID: f.viewer.ID, FolloweeID: "alice", FolloweeHost: &host}))
}

const viewerKeyID = "https://local.example/users/viewer#main-key"

func TestAPIShow_RetriesWithViewerKey(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			f := newViewerFetchFixture(t)
			f.follow(t)
			f.fetcher.instanceErr = &activitypub.StatusError{StatusCode: status}
			code, body := f.show(t)
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, "Note", body["type"])
			assert.Equal(t, []string{viewerKeyID}, f.fetcher.keyIDs)
			assert.Equal(t, []string{viewerKeyID}, f.resolver.keyedWith, "the note is ingested with the same key")
			assert.Zero(t, f.resolver.plain)
			obj, _ := body["object"].(map[string]any)
			assert.Equal(t, "secret", obj["text"], "a follower sees the text")
		})
	}
}

func TestAPIShow_DoesNotRetryWithViewerKey(t *testing.T) {
	cases := map[string]func(t *testing.T, f *viewerFetchFixture){
		"instance actor can see it": func(_ *testing.T, f *viewerFetchFixture) { f.fetcher.instanceErr = nil },
		"gone": func(_ *testing.T, f *viewerFetchFixture) {
			f.fetcher.instanceErr = &activitypub.StatusError{StatusCode: http.StatusGone}
		},
		"server error": func(_ *testing.T, f *viewerFetchFixture) {
			f.fetcher.instanceErr = &activitypub.StatusError{StatusCode: http.StatusInternalServerError}
		},
		"network error":    func(_ *testing.T, f *viewerFetchFixture) { f.fetcher.instanceErr = errors.New("dial") },
		"no keypair":       func(_ *testing.T, f *viewerFetchFixture) { delete(f.keypairs.items, "viewer") },
		"viewer suspended": func(_ *testing.T, f *viewerFetchFixture) { f.viewer.IsSuspended = true },
		"remote viewer": func(_ *testing.T, f *viewerFetchFixture) {
			h := "other.example"
			f.viewer.Host = &h
		},
		"viewer not federated": func(_ *testing.T, f *viewerFetchFixture) {
			f.h.SetLocalFederationObserver(&localFederationObserverStub{available: false})
		},
		// 照会先のホストの誰もフォローしていなければ、鍵を見せない。
		"follows no one on the host": func(t *testing.T, f *viewerFetchFixture) {
			require.NoError(t, f.following.Delete(&model.Following{ID: "f1"}))
			other := "other.example"
			require.NoError(t, f.following.Create(&model.Following{ID: "f2", FollowerID: f.viewer.ID, FolloweeID: "carol", FolloweeHost: &other}))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newViewerFetchFixture(t)
			f.follow(t)
			mutate(t, f)
			f.show(t)
			assert.Empty(t, f.fetcher.keyIDs)
			assert.Empty(t, f.resolver.keyedWith)
		})
	}
}

func TestAPIShow_ViewerKeyRetryFails(t *testing.T) {
	for _, tc := range []struct {
		instance, keyed int
		want            string
	}{
		// 鍵でも見せてもらえなければ、instance actor で取ったときのエラーで返す。
		{http.StatusNotFound, http.StatusNotFound, "NO_SUCH_OBJECT"},
		{http.StatusNotFound, http.StatusForbidden, "NO_SUCH_OBJECT"},
		{http.StatusForbidden, http.StatusNotFound, "REQUEST_FAILED"},
		{http.StatusNotFound, http.StatusBadGateway, "REQUEST_FAILED"},
	} {
		t.Run(fmt.Sprintf("%d/%d", tc.instance, tc.keyed), func(t *testing.T) {
			f := newViewerFetchFixture(t)
			f.follow(t)
			f.fetcher.instanceErr = &activitypub.StatusError{StatusCode: tc.instance}
			f.fetcher.keyedErr = &activitypub.StatusError{StatusCode: tc.keyed}
			_, body := f.show(t)
			require.Len(t, f.fetcher.keyIDs, 1, "retried")
			assert.Equal(t, tc.want, body["error"].(map[string]any)["code"])
		})
	}
}

// A followers-only note is not shown to a viewer who does not follow the author
// here, even when the remote server hands it out.
func TestAPIShow_HidesNoteFromNonFollower(t *testing.T) {
	f := newViewerFetchFixture(t)
	f.fetcher.instanceErr = nil
	code, body := f.show(t)
	require.Equal(t, http.StatusOK, code, body)
	obj, _ := body["object"].(map[string]any)
	assert.Nil(t, obj["text"], "the text is hidden")
	assert.Equal(t, true, obj["isHidden"])
}

// A remote note already in the database is returned without fetching, so the
// viewer's key is not shown to anyone, and hidden from a viewer who cannot see it.
func TestAPIShow_KnownRemoteNoteIsNotFetched(t *testing.T) {
	for _, follows := range []bool{true, false} {
		t.Run(fmt.Sprintf("follows=%v", follows), func(t *testing.T) {
			f := newViewerFetchFixture(t)
			if follows {
				f.follow(t)
			}
			f.notes.Notes[f.resolved.ID] = f.resolved
			code, body := f.show(t)
			require.Equal(t, http.StatusOK, code, body)
			assert.Zero(t, f.fetcher.instanceCalls)
			assert.Empty(t, f.fetcher.keyIDs)
			obj, _ := body["object"].(map[string]any)
			if follows {
				assert.Equal(t, "secret", obj["text"])
			} else {
				assert.Nil(t, obj["text"])
			}
		})
	}
}
