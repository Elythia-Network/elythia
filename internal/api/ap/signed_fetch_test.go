package ap

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	corefederation "github.com/elythia-network/elythia/internal/core/federation"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubDeliveryGate refuses the listed hosts.
type stubDeliveryGate map[string]bool

func (g stubDeliveryGate) CanFetchOptionalRemoteData(host string) bool { return !g[host] }

// stubFetchSigner returns a fixed signer for any request.
type stubFetchSigner struct {
	user  *model.User
	err   error
	calls int
}

func (s *stubFetchSigner) AuthenticateFetch(*http.Request) (*model.User, error) {
	s.calls++
	return s.user, s.err
}

type signedFetchFixture struct {
	h         *Handler
	users     *testutil.MockUserRepository
	notes     *testutil.MockNoteRepository
	following *testutil.MockFollowingRepository
	blocking  *testutil.MockBlockingRepository
	signer    *stubFetchSigner
	bob       *model.User
}

// newSignedFetchFixture wires a handler with a local author alice, a remote
// user bob who signs the requests, and notes of each visibility by alice.
func newSignedFetchFixture(t *testing.T) *signedFetchFixture {
	t.Helper()
	users := testutil.NewMockUserRepository()
	notes := testutil.NewMockNoteRepository()
	following := testutil.NewMockFollowingRepository()
	blocking := testutil.NewMockBlockingRepository()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	userSvc := coreuser.NewService(users, notes, testutil.NewMockUserNotePiningRepository(), idGen, nil)
	querySvc := corenote.NewQueryService(notes, following)
	renderer := activitypub.NewRenderer(activitypub.NewURLBuilder("https://local.example"))
	h := NewHandler(renderer, userSvc, querySvc, &memoryKeypairRepo{items: map[string]*model.UserKeypair{}}, idGen,
		nil, nil, apShowHostBlocker{}, "local.example", nil, nil)
	h.SetRelationRepos(userrelation.Repos{Blocking: blocking})
	h.SetNoteRepo(notes)

	alice := &model.User{ID: "alice", Username: "alice"}
	host, uri := "remote.example", "https://remote.example/users/bob"
	bob := &model.User{ID: "bob", Username: "bob", Host: &host, URI: &uri}
	users.Users[alice.ID], users.Users[bob.ID] = alice, bob
	for _, n := range []*model.Note{
		{ID: "n-public", UserID: alice.ID, User: alice, Visibility: model.NoteVisibilityPublic},
		{ID: "n-followers", UserID: alice.ID, User: alice, Visibility: model.NoteVisibilityFollowers},
		{ID: "n-specified", UserID: alice.ID, User: alice, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{bob.ID}},
		{ID: "n-specified-other", UserID: alice.ID, User: alice, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"carol"}},
	} {
		notes.Notes[n.ID] = n
	}
	signer := &stubFetchSigner{user: bob}
	h.SetFetchSigner(signer, stubDeliveryGate{}, apShowSoftwareBlocker{})
	return &signedFetchFixture{h: h, users: users, notes: notes, following: following, blocking: blocking, signer: signer, bob: bob}
}

func (f *signedFetchFixture) follow(t *testing.T) {
	t.Helper()
	require.NoError(t, f.following.Create(&model.Following{ID: "f1", FollowerID: f.bob.ID, FolloweeID: "alice"}))
}

// signedFetchEndpoints are the two routes that serve a note over ActivityPub.
func signedFetchEndpoints(h *Handler) map[string]echo.HandlerFunc {
	return map[string]echo.HandlerFunc{"note": h.Note, "activity": h.NoteActivity}
}

// fetch runs a GET with a Signature header (its value is not read by the stub).
func (f *signedFetchFixture) fetch(t *testing.T, handler echo.HandlerFunc, noteID string, signed bool) int {
	t.Helper()
	c, rec := newReq(t, "id", noteID)
	if signed {
		c.Request().Header.Set("Signature", `keyId="https://remote.example/users/bob#main-key"`)
	}
	require.NoError(t, handler(c))
	return rec.Code
}

func TestSignedFetch_FollowerSeesFollowersNote(t *testing.T) {
	for name := range signedFetchEndpoints(newSignedFetchFixture(t).h) {
		t.Run(name, func(t *testing.T) {
			f := newSignedFetchFixture(t)
			handler := signedFetchEndpoints(f.h)[name]
			assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-followers", true), "not following yet")
			f.follow(t)
			assert.Equal(t, http.StatusOK, f.fetch(t, handler, "n-followers", true))
			assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-followers", false), "unsigned GET")
		})
	}
}

func TestSignedFetch_AddresseeSeesSpecifiedNote(t *testing.T) {
	for name := range signedFetchEndpoints(newSignedFetchFixture(t).h) {
		t.Run(name, func(t *testing.T) {
			f := newSignedFetchFixture(t)
			handler := signedFetchEndpoints(f.h)[name]
			assert.Equal(t, http.StatusOK, f.fetch(t, handler, "n-specified", true))
			assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-specified", false), "unsigned GET")
			assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-specified-other", true), "not an addressee")
		})
	}
}

// The response for a followers note must carry the followers addressing, so
// the requester does not take it as public.
func TestSignedFetch_FollowersNoteKeepsAddressing(t *testing.T) {
	f := newSignedFetchFixture(t)
	f.follow(t)
	c, rec := newReq(t, "id", "n-followers")
	c.Request().Header.Set("Signature", "x")
	require.NoError(t, f.h.Note(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "https://local.example/users/alice/followers")
	assert.NotContains(t, rec.Body.String(), "https://www.w3.org/ns/activitystreams#Public")
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestSignedFetch_Refused(t *testing.T) {
	cases := map[string]func(t *testing.T, f *signedFetchFixture){
		"signature not authenticated": func(_ *testing.T, f *signedFetchFixture) { f.signer.err = errors.New("bad signature") },
		"authenticator not wired":     func(_ *testing.T, f *signedFetchFixture) { f.h.fetchSigner = nil },
		"signer suspended":            func(_ *testing.T, f *signedFetchFixture) { f.bob.IsSuspended = true },
		"signer deleted":              func(_ *testing.T, f *signedFetchFixture) { f.bob.IsDeleted = true },
		"signer without host":         func(_ *testing.T, f *signedFetchFixture) { f.bob.Host = nil },
		// ブロック・許可リストの外・管理者による配送停止・meta を読めないときは、どれも CanFetchOptionalRemoteData が false を返す。
		"not delivered to the signer's host": func(_ *testing.T, f *signedFetchFixture) {
			f.h.fetchDelivery = stubDeliveryGate{"remote.example": true}
		},
		"delivery gate not wired": func(_ *testing.T, f *signedFetchFixture) { f.h.fetchDelivery = nil },
		"signer software not delivered to": func(_ *testing.T, f *signedFetchFixture) {
			f.h.fetchDeliverSoftware = apShowSoftwareBlocker{suspended: map[string]bool{"remote.example": true}}
		},
		"deliver software check not wired": func(_ *testing.T, f *signedFetchFixture) { f.h.fetchDeliverSoftware = nil },
		"signer software refused inbound": func(_ *testing.T, f *signedFetchFixture) {
			f.h.softwareBlocker = apShowSoftwareBlocker{suspended: map[string]bool{"remote.example": true}}
		},
		"author blocks signer": func(t *testing.T, f *signedFetchFixture) {
			require.NoError(t, f.blocking.Create(&model.Blocking{ID: "b1", BlockerID: "alice", BlockeeID: f.bob.ID}))
		},
		"signer blocks author": func(t *testing.T, f *signedFetchFixture) {
			require.NoError(t, f.blocking.Create(&model.Blocking{ID: "b1", BlockerID: f.bob.ID, BlockeeID: "alice"}))
		},
		"block check fails": func(_ *testing.T, f *signedFetchFixture) { f.blocking.ExistsErr = errors.New("db down") },
		"block check not wired": func(_ *testing.T, f *signedFetchFixture) {
			f.h.SetRelationRepos(userrelation.Repos{})
		},
		"local only": func(_ *testing.T, f *signedFetchFixture) { f.notes.Notes["n-followers"].LocalOnly = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			for endpoint := range signedFetchEndpoints(newSignedFetchFixture(t).h) {
				f := newSignedFetchFixture(t)
				f.follow(t)
				handler := signedFetchEndpoints(f.h)[endpoint]
				require.Equal(t, http.StatusOK, f.fetch(t, handler, "n-followers", true), "%s: baseline", endpoint)
				mutate(t, f)
				assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-followers", true), endpoint)
			}
		})
	}
}

// A remote note is redirected to its origin only when it is public; a
// non-public remote note is not served on the strength of our own follow record.
func TestSignedFetch_RemoteNoteIsNotServed(t *testing.T) {
	f := newSignedFetchFixture(t)
	host, uri := "third.example", "https://third.example/notes/x"
	carol := &model.User{ID: "carol", Username: "carol", Host: &host}
	f.users.Users[carol.ID] = carol
	f.notes.Notes["n-remote"] = &model.Note{ID: "n-remote", UserID: carol.ID, User: carol, UserHost: &host, URI: &uri,
		Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{f.bob.ID}}
	assert.Equal(t, http.StatusNotFound, f.fetch(t, f.h.Note, "n-remote", true))
}

// Public notes do not need the signature at all, so they are served without
// running the authenticator.
func TestSignedFetch_PublicNoteSkipsAuthentication(t *testing.T) {
	f := newSignedFetchFixture(t)
	assert.Equal(t, http.StatusOK, f.fetch(t, f.h.Note, "n-public", true))
	assert.Zero(t, f.signer.calls)
	assert.Equal(t, http.StatusNotFound, f.fetch(t, f.h.Note, "missing", false))
	assert.Zero(t, f.signer.calls, "unsigned requests never reach the authenticator")
}

// fetchKeys hands the signer's public key to the real authenticator.
type fetchKeys map[string]string

func (k fetchKeys) PublicKeyForKeyID(actorID, _ string) (string, error) {
	if pem, ok := k[actorID]; ok {
		return pem, nil
	}
	return "", errors.New("no key")
}

// End to end with a real signature: the request target and Host the handler
// sees are the ones the signer signed.
func TestSignedFetch_RealSignature(t *testing.T) {
	f := newSignedFetchFixture(t)
	f.follow(t)
	privPEM, pubPEM, err := activitypub.GenerateRSAKeypair()
	require.NoError(t, err)
	key, err := activitypub.NewPrivateKey("https://remote.example/users/bob#main-key", privPEM)
	require.NoError(t, err)
	f.h.fetchSigner = corefederation.NewSignedFetchAuthenticator(f.users, fetchKeys{f.bob.ID: pubPEM}, "local.example")

	get := func(target string) int {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set(echo.HeaderAccept, "application/activity+json")
		require.NoError(t, activitypub.SignRequest(req, key, "", []string{"(request-target)", "host", "date", "accept"}))
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		c.SetParamNames("id")
		c.SetParamValues("n-followers")
		require.NoError(t, f.h.Note(c))
		return rec.Code
	}
	assert.Equal(t, http.StatusOK, get("https://local.example/notes/n-followers"))
	assert.Equal(t, http.StatusNotFound, get("https://elsewhere.example/notes/n-followers"), "signed for another host")
}

// A mentioned non-follower may read a followers-only note: the delivery already
// sends it to mentioned users, and CanSeeNote allows it.
func TestSignedFetch_MentionedUserSeesFollowersNote(t *testing.T) {
	f := newSignedFetchFixture(t)
	assert.Equal(t, http.StatusNotFound, f.fetch(t, f.h.Note, "n-followers", true))
	f.notes.Notes["n-followers"].Mentions = []string{f.bob.ID}
	assert.Equal(t, http.StatusOK, f.fetch(t, f.h.Note, "n-followers", true))
}

// With the local federation observer wired, the author's availability is still
// checked after the signer is allowed.
func TestSignedFetch_LocalFederationObserver(t *testing.T) {
	f := newSignedFetchFixture(t)
	f.follow(t)
	observer := &localFederationObserverStub{available: true}
	f.h.SetLocalFederationObserver(observer)
	for name, handler := range signedFetchEndpoints(f.h) {
		assert.Equal(t, http.StatusOK, f.fetch(t, handler, "n-followers", true), name)
	}
	observer.available = false
	for name, handler := range signedFetchEndpoints(f.h) {
		assert.Equal(t, http.StatusNotFound, f.fetch(t, handler, "n-followers", true), name)
	}
}

// A followers-only pure renote is served as an Announce of its target.
func TestSignedFetch_FollowersPureRenoteIsAnnounce(t *testing.T) {
	f := newSignedFetchFixture(t)
	f.follow(t)
	target := "n-public"
	f.notes.Notes["n-renote"] = &model.Note{ID: "n-renote", UserID: "alice", User: f.users.Users["alice"],
		Visibility: model.NoteVisibilityFollowers, RenoteID: &target}
	c, rec := newReq(t, "id", "n-renote")
	c.Request().Header.Set("Signature", "x")
	require.NoError(t, f.h.NoteActivity(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"type":"Announce"`)
	assert.Contains(t, rec.Body.String(), "https://local.example/notes/n-public")
}
