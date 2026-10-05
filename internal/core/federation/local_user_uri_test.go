package federation_test

import (
	"fmt"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	corefollowing "github.com/elythia-network/elythia/internal/core/following"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// plantLocalUsers registers local users with the given IDs, so that local
// actor URIs naming them resolve (upstream fetchPerson finds them by ID).
func plantLocalUsers(repo *testutil.MockUserRepository, ids ...string) {
	for _, id := range ids {
		repo.Users[id] = &model.User{ID: id, Username: id, UsernameLower: id}
	}
}

// plantNumberedLocalUsers registers the local users local0..local<n-1>.
func plantNumberedLocalUsers(repo *testutil.MockUserRepository, n int) {
	for i := 0; i < n; i++ {
		plantLocalUsers(repo, fmt.Sprintf("local%d", i))
	}
}

// #3330: ExtractLocalUserID reads the last path segment like upstream
// ApPersonService.fetchPerson / ApInboxService.flag (`uri.split('/').pop()`),
// so a local user's collection URI does not name that user.
func TestExtractLocalUserID_LastSegment(t *testing.T) {
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(), urls, &stubFetcher{body: []byte(aliceActor)}, idGen)

	cases := []struct {
		uri  string
		want string
	}{
		{"https://example.com/users/bob", "bob"},
		{"https://example.com/users/bob/followers", "followers"},
		{"https://example.com/users/bob/following", "following"},
		{"https://example.com/users/bob/", ""},
		{"https://example.com/notes/bob", "bob"},
		{"https://remote.example/users/bob", ""},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			assert.Equal(t, tc.want, r.ExtractLocalUserID(tc.uri))
		})
	}
}

// #3330: a Flag whose object is a local user's followers collection reports
// nobody, as upstream flag() looks up the last segment (`followers`).
func TestProcess_FlagLocalFollowersCollectionReportsNobody(t *testing.T) {
	p, repo, _ := newProcessorWithBlocking(t)
	abuseRepo := testutil.NewMockAbuseReportRepository()
	idGenFlag, _ := id.NewGenerator("aidx")
	p.SetAbuseReportRepo(abuseRepo, idGenFlag)
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}

	body := []byte(`{
		"type": "Flag",
		"actor": "https://remote.example/users/alice",
		"object": ["https://example.com/users/bob/followers"],
		"content": "spam"
	}`)
	_ = p.Process(body)
	assert.Empty(t, abuseRepo.Reports, "followers collection を bob の通報にしない")
}

// #3330: a remote note addressed (specified) to a local user's followers
// collection and mentioning it does not make that user a recipient or a
// mention, as upstream resolvePerson does not resolve the collection URI.
func TestIngestNote_LocalFollowersCollectionIsNotTheUser(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	repo.Users["bob"] = &model.User{ID: "bob", Username: "bob", UsernameLower: "bob"}
	noteRepo := testutil.NewMockNoteRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, noteRepo, urls, &stubFetcher{body: []byte(sampleActor)}, idGen)

	body := []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/to-bob-followers",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "hi",
		"to": ["https://example.com/users/bob/followers"],
		"cc": [],
		"tag": [{"type": "Mention", "href": "https://example.com/users/bob/followers"}]
	}`)
	got, err := r.IngestNote(body)
	require.NoError(t, err)
	assert.Equal(t, model.NoteVisibilitySpecified, got.Visibility, "他人の followers collection は followers ではない")
	assert.NotContains(t, []string(got.VisibleUserIDs), "bob")
	assert.NotContains(t, []string(got.Mentions), "bob")
	// 最後の段の `followers` は実在しない ID なので、宛先にもメンションにも入れない。
	assert.Empty(t, []string(got.VisibleUserIDs))
	assert.Empty(t, []string(got.Mentions))
}

// countingUserRepo counts the user lookups resolveMentionedUserIDs issues.
type countingUserRepo struct {
	*testutil.MockUserRepository
	manyByIDs int
	byID      int
	lastIDs   []string
}

func (c *countingUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	c.manyByIDs++
	c.lastIDs = append([]string(nil), ids...)
	return c.MockUserRepository.FindManyByIDs(ids)
}

// #3330 review: chat comes from cherrypick and has no upstream counterpart, so
// its recipient keeps the reading from before #3330 (the segment after
// `users`): `/users/{id}/` and `/users/{id}/...` still name the local user.
func TestProcess_ChatRecipientKeepsFirstSegmentReading(t *testing.T) {
	for _, to := range []string{
		"https://example.com/users/bob/",
		"https://example.com/users/bob/inbox",
	} {
		t.Run("Create(Note) to "+to, func(t *testing.T) {
			p, repo, _, _ := newProcessor(t, aliceActor)
			repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
			chatSvc := &stubChatReceiver{}
			p.SetChatService(chatSvc)
			body := []byte(`{
				"id": "https://remote.example/activities/create-chat-seg",
				"type": "Create",
				"actor": "https://remote.example/users/alice",
				"object": {
					"id": "https://remote.example/chat-messages/cm-seg",
					"type": "Note",
					"attributedTo": "https://remote.example/users/alice",
					"content": "<p>hi</p>",
					"to": ["` + to + `"],
					"_misskey_talk": true
				}
			}`)
			require.NoError(t, p.Process(body))
			require.Equal(t, 1, chatSvc.called)
			assert.Equal(t, "bob", chatSvc.lastTo)
		})
		t.Run("ChatMessage to "+to, func(t *testing.T) {
			p, repo, _, _ := newProcessor(t, aliceActor)
			repo.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
			chatSvc := &stubChatReceiver{}
			p.SetChatService(chatSvc)
			body := []byte(`{
				"id": "https://remote.example/chat-messages/direct-seg",
				"type": "Misskey:ChatMessage",
				"actor": "https://remote.example/users/alice",
				"attributedTo": "https://remote.example/users/alice",
				"to": "` + to + `",
				"content": "<p>hi</p>"
			}`)
			require.NoError(t, p.Process(body))
			require.Equal(t, 1, chatSvc.called)
			assert.Equal(t, "bob", chatSvc.lastTo)
		})
	}
}

// #3330 review: repeated Mentions of the same local user are looked up with
// the ID once, so the IN list does not grow with the repetition.
func TestResolveMentionedUserIDs_LocalLookupDedupsIDs(t *testing.T) {
	mock := testutil.NewMockUserRepository()
	plantLocalUsers(mock, "bob")
	repo := &countingUserRepo{MockUserRepository: mock}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls, &stubFetcher{}, idGen)

	ids, err := r.ResolveMentionedUserIDs([]string{
		"https://example.com/users/bob",
		"https://example.com/users/bob",
		"https://example.com/users/bob",
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"bob"}, ids)
	assert.Equal(t, 1, repo.manyByIDs)
	assert.Equal(t, []string{"bob"}, repo.lastIDs, "重複を除いた ID だけを引く")
}

func (c *countingUserRepo) FindByID(id string) (*model.User, error) {
	c.byID++
	return c.MockUserRepository.FindByID(id)
}

// #3330: local mention targets are checked for existence in a single query,
// not one per Mention.
func TestResolveMentionedUserIDs_LocalExistenceIsOneQuery(t *testing.T) {
	mock := testutil.NewMockUserRepository()
	plantNumberedLocalUsers(mock, 5)
	repo := &countingUserRepo{MockUserRepository: mock}
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(repo, testutil.NewMockNoteRepository(), urls, &stubFetcher{}, idGen)

	hrefs := []string{"https://example.com/users/missing"}
	for i := 0; i < 5; i++ {
		hrefs = append(hrefs, fmt.Sprintf("https://example.com/users/local%d", i))
	}
	ids, err := r.ResolveMentionedUserIDs(hrefs)
	require.NoError(t, err)
	assert.Equal(t, []string{"local0", "local1", "local2", "local3", "local4"}, ids)
	assert.Equal(t, 1, repo.manyByIDs, "まとめて 1 回で引く")
	assert.Zero(t, repo.byID, "1 件ずつ引かない")

	repo.manyByIDs = 0
	_, err = r.ResolveMentionedUserIDs([]string{"https://remote.example/users/x"})
	require.NoError(t, err)
	assert.Zero(t, repo.manyByIDs, "ローカルの URI が無ければ引かない")
}

// #3330: Accept reads its inner Follow actor like upstream getUserFromApId
// (parseUri): the segment after `users`, ignoring what follows it. A trailing
// slash therefore still names the local follower.
func TestProcess_AcceptFollowActorReadLikeGetUserFromApID(t *testing.T) {
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
	reqRepo.Requests["r1"] = &model.FollowRequest{ID: "r1", FollowerID: "bob", FolloweeID: "alice1"}

	acceptBody := []byte(`{
		"type": "Accept",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Follow",
			"actor": "https://example.com/users/bob/",
			"object": "https://remote.example/users/alice"
		}
	}`)
	require.NoError(t, p.Process(acceptBody))
	assert.Len(t, followingRepo.Followings, 1, "users の次の段 (bob) を follower として承認する")
	assert.Empty(t, reqRepo.Requests)
}
