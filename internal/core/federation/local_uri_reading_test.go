package federation_test

import (
	"errors"
	"testing"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	corefollowing "github.com/shiroha-a/mk/internal/core/following"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3330: ExtractLocalUserID reads a URI the way upstream fetchPerson does — a
// plain `{url}/` string prefix, not a host comparison — so a URI on this host
// that does not spell out `{url}/` (other scheme, explicit default port,
// uppercase host, userinfo) names nobody, as upstream createPerson then throws
// `cannot resolve local user` for it.
func TestExtractLocalUserID_FetchPersonPrefix(t *testing.T) {
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(aliceActor)}, idGen)

	cases := []struct {
		uri  string
		want string
	}{
		{"https://example.com/users/bob", "bob"},
		{"https://example.com/foo/bob", "bob"},
		{"https://example.com/bob", "bob"},
		{"https://example.com/", ""},
		{"https://example.com", ""},
		{"http://example.com/users/bob", ""},
		{"https://example.com:443/users/bob", ""},
		{"https://EXAMPLE.com/users/bob", ""},
		{"https://example.com@remote.example/users/bob", ""},
		{"https://user@example.com/users/bob", ""},
		{"https://example.com.remote.example/users/bob", ""},
		{"https://remote.example/https://example.com/users/bob", ""},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			assert.Equal(t, tc.want, r.ExtractLocalUserID(tc.uri))
		})
	}
}

// #3330: a mention / specified recipient under `{url}/` resolves to the user
// its last segment names even outside `/users/` (upstream fetchPerson), while
// one on this host under another scheme resolves to nobody.
func TestIngestNote_LocalMentionReadsFetchPersonPrefix(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	plantLocalUsers(repo, "bob", "carol")
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/prefix-mention",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hi",
		"to": ["https://example.com/foo/bob", "http://example.com/users/carol"],
		"cc": [],
		"tag": [
			{"type": "Mention", "href": "https://example.com/foo/bob"},
			{"type": "Mention", "href": "http://example.com/users/carol"}
		]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, []string{"bob"}, []string(got.Mentions))
	assert.Equal(t, []string{"bob"}, []string(got.VisibleUserIDs))
}

// flagBody returns a Flag from remote alice whose object is objects (a JSON
// array literal).
func flagBody(objects string) []byte {
	return []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": ` + objects + `,
		"content": "spam"
	}`)
}

// #3330: upstream flag() reports users[0] of `findBy({ id: In(ids) })`, which
// a primary-key index scan returns in ID order. mk-go fixes that order: the
// smallest existing ID wins, not the first URI.
func TestProcess_FlagReportsSmallestExistingID(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	plantLocalUsers(repo, "b2", "a1")

	require.NoError(t, p.Process(flagBody(`[
		"https://example.com/users/zz-missing",
		"https://example.com/users/b2",
		"https://example.com/users/a1",
		"https://example.com/users/b2"
	]`)))
	require.Len(t, abuseRepo.Reports, 1)
	for _, r := range abuseRepo.Reports {
		assert.Equal(t, "a1", r.TargetUserID)
	}
}

// #3330: unlike mentions (fetchPerson, `{url}/`), upstream flag() keeps only
// URIs under `{url}/users/`, so a note-shaped local URI reports nobody even
// when its last segment is a user ID.
func TestProcess_FlagIgnoresLocalURIOutsideUsers(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	plantLocalUsers(repo, "bob")

	require.NoError(t, p.Process(flagBody(`["https://example.com/notes/bob", "http://example.com/users/bob"]`)))
	assert.Empty(t, abuseRepo.Reports)
}

// #3330 (#3121): a failed target lookup is retried, not acked as "nobody".
func TestProcess_FlagLookupFailurePropagates(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	plantLocalUsers(repo, "bob")
	boom := errors.New("connection refused")
	repo.FindManyByIDsErr = boom

	require.ErrorIs(t, p.Process(flagBody(`["https://example.com/users/bob"]`)), boom)
	assert.Empty(t, abuseRepo.Reports)
}

// #3330: a Flag naming a remote user (upstream flag() does not filter by host)
// stores that user's host as targetUserHost, taken from the row the target was
// picked from. Re-reading it by ID and swallowing a failure stored nil, which
// reads as a local target.
func TestProcess_FlagRemoteTargetKeepsHost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failByID bool
	}{
		{"plain", false},
		{"FindByID failing", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, repo, _ := newProcessorWithBlocking(t)
			abuseRepo := testutil.NewMockAbuseReportRepository()
			idGenFlag, _ := id.NewGenerator("aidx")
			p.SetAbuseReportRepo(abuseRepo, idGenFlag)
			// reporter (alice) を先に取り込んでおく (FindByID の失敗を立てる前に)。
			require.NoError(t, p.Process(flagBody(`["https://example.com/users/nobody"]`)))
			host := "other.example"
			uri := "https://other.example/users/carol"
			repo.Users["carol"] = &model.User{ID: "carol", Username: "carol", Host: &host, URI: &uri}
			if tc.failByID {
				repo.FindErr = errors.New("connection refused")
			}

			require.NoError(t, p.Process(flagBody(`["https://example.com/users/carol"]`)))
			require.Len(t, abuseRepo.Reports, 1)
			for _, r := range abuseRepo.Reports {
				assert.Equal(t, "carol", r.TargetUserID)
				require.NotNil(t, r.TargetUserHost, "リモートの対象をローカル扱いで保存している")
				assert.Equal(t, host, *r.TargetUserHost)
			}
		})
	}
}

// acceptFamilyFixture wires a processor with a real following service over
// mock repositories, the remote followee alice1 and the local user bob.
type acceptFamilyFixture struct {
	p             *federation.Processor
	repo          *testutil.MockUserRepository
	followingRepo *testutil.MockFollowingRepository
	reqRepo       *testutil.MockFollowRequestRepository
}

func newAcceptFamilyFixture(t *testing.T) *acceptFamilyFixture {
	t.Helper()
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	followingRepo := testutil.NewMockFollowingRepository()
	reqRepo := testutil.NewMockFollowRequestRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	followingSvc := corefollowing.NewService(repo, followingRepo, reqRepo, idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, repo, noteRepo)

	aliceURI := "https://remote.example/users/alice"
	host := "remote.example"
	repo.Users["alice1"] = &model.User{ID: "alice1", Username: "alice", UsernameLower: "alice", URI: &aliceURI, Host: &host}
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	return &acceptFamilyFixture{p: p, repo: repo, followingRepo: followingRepo, reqRepo: reqRepo}
}

func acceptBody(typ, followerURI string) []byte {
	return []byte(`{
		"type": "` + typ + `",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "` + followerURI + `",
			"object": "https://remote.example/users/alice"
		}
	}`)
}

func undoAcceptBody(followerURI string) []byte {
	const actor = "https://remote.example/users/alice"
	return []byte(`{"type":"Undo","actor":"` + actor + `","object":{"type":"Accept","actor":"` + actor + `",` +
		`"object":{"type":"Follow","actor":"` + followerURI + `","object":"` + actor + `"}}}`)
}

// #3330: the follower of an Accept / Reject / Undo(Accept) is read like
// upstream getUserFromApId: the path only (query and fragment dropped), any
// scheme on this host, and a deleted user is not found.
func TestProcess_AcceptFamilyFollowerLikeGetUserFromApID(t *testing.T) {
	t.Run("Accept reads the path without query and fragment", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		f.reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}
		require.NoError(t, f.p.Process(acceptBody("Accept", "http://example.com/users/bob?x=1#y")))
		assert.Len(t, f.followingRepo.Followings, 1)
		assert.Empty(t, f.reqRepo.Requests)
	})

	t.Run("Accept from a deleted follower is skipped", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		f.repo.Users["bob"].IsDeleted = true
		f.reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}
		require.NoError(t, f.p.Process(acceptBody("Accept", "https://example.com/users/bob")))
		assert.Empty(t, f.followingRepo.Followings)
		assert.Len(t, f.reqRepo.Requests, 1)
	})

	t.Run("Accept naming a local non-users path is skipped", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		f.reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}
		// 本家は parsed.type !== 'users' で null。
		bobURI := "https://example.com/notes/bob"
		f.repo.Users["bob"].URI = &bobURI
		require.NoError(t, f.p.Process(acceptBody("Accept", bobURI)))
		assert.Empty(t, f.followingRepo.Followings)
	})

	t.Run("Reject from a deleted follower is skipped", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		f.repo.Users["bob"].IsDeleted = true
		f.reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}
		require.NoError(t, f.p.Process(acceptBody("Reject", "https://example.com/users/bob")))
		assert.Len(t, f.reqRepo.Requests, 1)
	})

	t.Run("Reject reads the path without query", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		f.reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}
		require.NoError(t, f.p.Process(acceptBody("Reject", "https://example.com/users/bob?x=1")))
		assert.Empty(t, f.reqRepo.Requests)
	})

	t.Run("Undo(Accept) of a deleted follower is skipped", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		f.repo.Users["bob"].IsDeleted = true
		f.followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice1"}
		require.NoError(t, f.p.Process(undoAcceptBody("https://example.com/users/bob")))
		assert.Len(t, f.followingRepo.Followings, 1)
	})

	t.Run("Undo(Accept) reads the path without fragment", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		f.followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "bob", FolloweeID: "alice1"}
		require.NoError(t, f.p.Process(undoAcceptBody("https://example.com/users/bob#main-key")))
		assert.Empty(t, f.followingRepo.Followings)
	})

	t.Run("a deleted remote follower is not found either", func(t *testing.T) {
		f := newAcceptFamilyFixture(t)
		carolURI := "https://other.example/users/carol"
		host := "other.example"
		f.repo.Users["carol"] = &model.User{ID: "carol", Username: "carol", URI: &carolURI, Host: &host, IsDeleted: true}
		f.followingRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "carol", FolloweeID: "alice1"}
		require.NoError(t, f.p.Process(undoAcceptBody(carolURI)))
		assert.Len(t, f.followingRepo.Followings, 1)
	})
}
