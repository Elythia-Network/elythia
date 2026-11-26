package federation_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 送り手から見えないローカルの返信先への返信は、返信として受けず、ノートごと
// 取り込まないこと (本家 ApNoteService の inReplyTo と同じ)。見える返信先と、
// リモートの返信先は従来どおり返信として受ける。
func TestIngestNote_ReplyToInvisibleLocalNoteIsRejected(t *testing.T) {
	const replyURI = "https://remote.example/notes/reply-visibility"
	body := func(target string) []byte {
		return []byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
			"id": "` + replyURI + `",
			"type": "Note",
			"attributedTo": "https://remote.example/users/alice",
			"content": "reply",
			"inReplyTo": "` + target + `",
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}`)
	}
	remoteHost := "remote.example"
	targetURI := "https://remote.example/notes/parent"
	cases := []struct {
		name    string
		target  func(aliceID string) *model.Note
		follows bool
		inReply string
		visible bool
	}{
		{"public", func(string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilityPublic}
		}, false, "https://example.com/notes/parent", true},
		{"home", func(string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilityHome}
		}, false, "https://example.com/notes/parent", true},
		{"followers, not a follower", func(string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilityFollowers}
		}, false, "https://example.com/notes/parent", false},
		{"followers, a follower", func(string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilityFollowers}
		}, true, "https://example.com/notes/parent", true},
		{"followers, mentioned", func(aliceID string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilityFollowers, Mentions: model.StringArray{aliceID}}
		}, false, "https://example.com/notes/parent", true},
		{"specified, not a recipient", func(string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{"carol"}}
		}, false, "https://example.com/notes/parent", false},
		{"specified, a recipient", func(aliceID string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: model.StringArray{aliceID}}
		}, false, "https://example.com/notes/parent", true},
		// inbox の連鎖ではない取得 (IngestNote) でも localOnly の返信先を拒否する。
		{"public but localOnly", func(string) *model.Note {
			return &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilityPublic, LocalOnly: true}
		}, false, "https://example.com/notes/parent", false},
		// リモートの返信先の公開範囲は配送元が決めるので見ない (本家と同じ)。
		{"remote followers note", func(string) *model.Note {
			return &model.Note{ID: "parent", UserID: "remote-bob", UserHost: &remoteHost, URI: &targetURI, Visibility: model.NoteVisibilityFollowers}
		}, false, targetURI, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			followingRepo := testutil.NewMockFollowingRepository()
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolverNilDeps(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"),
				&stubFetcher{body: []byte(sampleActor)}, idGen)
			r.SetFollowingRepo(followingRepo)
			alice, err := r.ResolveActor("https://remote.example/users/alice")
			require.NoError(t, err)
			if tc.follows {
				require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: alice.ID, FolloweeID: "bob"}))
			}
			noteRepo.Notes["parent"] = tc.target(alice.ID)

			got, err := r.IngestNote(body(tc.inReply))
			if !tc.visible {
				require.ErrorIs(t, err, federation.ErrReplyTargetNotVisible)
				require.ErrorIs(t, err, federation.ErrInboundLocalReferenceUnavailable,
					"the inbox worker must drop the activity without a retry")
				assert.Nil(t, got)
				_, findErr := noteRepo.FindByURI(replyURI)
				assert.Error(t, findErr, "the note must not be saved as a non-reply either")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			require.NotNil(t, got.ReplyID)
			assert.Equal(t, "parent", *got.ReplyID)
		})
	}
}

// 見えない返信先が投票を持っていても、投票として数えない (本家と同じく、返信先の
// 可視性を投票より先に判定する)。
func TestIngestNote_VoteToInvisibleLocalPollIsRejected(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["pollNote"] = &model.Note{ID: "pollNote", UserID: "author", HasPoll: true, Visibility: model.NoteVisibilityFollowers}
	pollRepo := testutil.NewMockPollRepository()
	require.NoError(t, pollRepo.Create(&model.Poll{NoteID: "pollNote", Choices: []string{"Apple", "Banana"}, Votes: []int64{0, 0}}))
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolverNilDeps(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"),
		&stubFetcher{body: []byte(sampleActor)}, idGen)
	r.SetFollowingRepo(testutil.NewMockFollowingRepository())
	r.SetPollRepo(pollRepo)
	voter := &stubPollVoter{}
	r.SetPollVoter(voter)

	got, err := r.IngestNote([]byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/users/alice#votes/pollNote",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"name": "Banana",
		"inReplyTo": "https://example.com/notes/pollNote",
		"to": ["https://example.com/users/author"]
	}`))
	require.ErrorIs(t, err, federation.ErrReplyTargetNotVisible)
	assert.Nil(t, got)
	assert.Empty(t, voter.calls, "must not vote on a poll the sender cannot see")
}

// existsFailingFollowingRepo fails every follow lookup.
type existsFailingFollowingRepo struct {
	*testutil.MockFollowingRepository
	err error
}

func (f existsFailingFollowingRepo) Exists(string, string) (bool, error) { return false, f.err }

// フォロー関係を引けない障害は「見えない」に倒さず、再試行できるエラーにする。
// 恒久の拒否にすると、正当な返信が一時的な障害で失われる。
func TestIngestNote_ReplyVisibilityLookupFailureIsRetryable(t *testing.T) {
	repo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	noteRepo.Notes["parent"] = &model.Note{ID: "parent", UserID: "bob", Visibility: model.NoteVisibilityFollowers}
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolverNilDeps(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"),
		&stubFetcher{body: []byte(sampleActor)}, idGen)
	boom := errors.New("following lookup failed")
	r.SetFollowingRepo(existsFailingFollowingRepo{MockFollowingRepository: testutil.NewMockFollowingRepository(), err: boom})

	got, err := r.IngestNote([]byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
		"id": "https://remote.example/notes/reply-lookup-failure",
		"type": "Note",
		"attributedTo": "https://remote.example/users/alice",
		"content": "reply",
		"inReplyTo": "https://example.com/notes/parent",
		"to": ["https://www.w3.org/ns/activitystreams#Public"]
	}`))
	require.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, federation.ErrInboundLocalReferenceUnavailable, "a lookup failure must not be a permanent rejection")
	assert.Nil(t, got)
	_, findErr := noteRepo.FindByURI("https://remote.example/notes/reply-lookup-failure")
	assert.Error(t, findErr, "the note must not be saved")
}

// ローカルの localOnly のノートは、inbox 以外の取得の経路でも引用先にしない
// (本家と同じ)。外側のノートは引用なしで取り込む。
func TestIngestNote_LocalOnlyQuoteIsDropped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		localOnly bool
	}{{"localOnly", true}, {"not localOnly", false}} {
		t.Run(tc.name, func(t *testing.T) {
			repo := testutil.NewMockUserRepository()
			noteRepo := testutil.NewMockNoteRepository()
			noteRepo.Notes["quoted"] = &model.Note{ID: "quoted", UserID: "bob", Visibility: model.NoteVisibilityPublic, LocalOnly: tc.localOnly}
			idGen, _ := id.NewGenerator("aidx")
			r := federation.NewResolverNilDeps(repo, noteRepo, activitypub.NewURLBuilder("https://example.com"),
				&stubFetcher{body: []byte(sampleActor)}, idGen)
			got, err := r.IngestNote([]byte(`{ "@context": "https://www.w3.org/ns/activitystreams",
				"id": "https://remote.example/notes/quote-local",
				"type": "Note",
				"attributedTo": "https://remote.example/users/alice",
				"content": "quote",
				"quoteUrl": "https://example.com/notes/quoted",
				"to": ["https://www.w3.org/ns/activitystreams#Public"]
			}`))
			require.NoError(t, err)
			require.NotNil(t, got)
			if tc.localOnly {
				assert.Nil(t, got.RenoteID, "a localOnly note must not become a quote target")
				return
			}
			require.NotNil(t, got.RenoteID)
			assert.Equal(t, "quoted", *got.RenoteID)
		})
	}
}
