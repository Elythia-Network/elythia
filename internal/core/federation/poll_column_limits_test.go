package federation_test

import (
	"strings"
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

// newQuestionProcessor wires the poll repo into a processor so inbound
// Create(Question) reaches createPollFromQuestion.
func newQuestionProcessor(t *testing.T) (*federation.Processor, *testutil.MockNoteRepository, *testutil.MockPollRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	noteRepo := testutil.NewMockNoteRepository()
	pollRepo := testutil.NewMockPollRepository()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	resolver := federation.NewResolver(userRepo, noteRepo, urls, &stubFetcher{body: []byte(aliceActor)}, idGen)
	resolver.SetPollRepo(pollRepo)
	followingSvc := corefollowing.NewService(
		userRepo, testutil.NewMockFollowingRepository(), testutil.NewMockFollowRequestRepository(), idGen)
	p := federation.NewProcessor(resolver, followingSvc, nil, nil, userRepo, noteRepo)
	return p, noteRepo, pollRepo
}

// `poll.choices` は varchar(256)[]。1 つでも溢れると poll の INSERT ごと落ちて、
// note は hasPoll=true のまま選択肢が無い状態になる (#2726)。
func TestProcess_CreateQuestion_TruncatesChoices(t *testing.T) {
	p, noteRepo, pollRepo := newQuestionProcessor(t)

	body := []byte(`{
		"type": "Create",
		"actor": "https://remote.example/users/alice",
		"object": {
			"type": "Question",
			"id": "https://remote.example/notes/qlimit",
			"attributedTo": "https://remote.example/users/alice",
			"content": "long?",
			"oneOf": [
				{"type": "Note", "name": "` + strings.Repeat("あ", 300) + `"},
				{"type": "Note", "name": "a\u0000b"}
			],
			"to": ["https://www.w3.org/ns/activitystreams#Public"]
		}
	}`)
	require.NoError(t, p.Process(body))
	assert.NotEmpty(t, noteRepo.Notes, "note は作られる (選択肢を切って残す)")
	require.Len(t, pollRepo.Polls, 1)
	for _, poll := range pollRepo.Polls {
		require.Len(t, poll.Choices, 2)
		// **全角で数える。** byte で切る実装なら 256 にならない。
		assert.Equal(t, 256, len([]rune(poll.Choices[0])))
		assert.Equal(t, "ab", poll.Choices[1], "NUL は落とす")
	}
}

// 切った選択肢に対する Update(Question) が name 照合で当たること。生値で引くと
// 空振りして vote が永久に更新されない (#2726)。
func TestUpdateRemoteQuestion_MatchesTruncatedChoice(t *testing.T) {
	r, userRepo, noteRepo, pollRepo := newResolverWithPoll(t)
	host := "remote.example"
	authorURI := "https://remote.example/users/alice"
	noteURI := "https://remote.example/notes/q1"
	userRepo.Users["alice"] = &model.User{ID: "alice", URI: &authorURI, Host: &host}
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "alice", UserHost: &host, URI: &noteURI, HasPoll: true}
	stored := strings.Repeat("あ", 256)
	require.NoError(t, pollRepo.Create(&model.Poll{NoteID: "n1", Choices: []string{stored}, Votes: []int64{1}}))

	obj := []byte(`{"id":"https://remote.example/notes/q1","type":"Question","oneOf":[` +
		`{"name":"` + strings.Repeat("あ", 300) + `","replies":{"totalItems":42}}]}`)
	require.NoError(t, r.UpdateRemoteQuestion(obj, authorURI))

	got, err := pollRepo.FindByNoteID("n1")
	require.NoError(t, err)
	assert.Equal(t, []int64{42}, []int64(got.Votes))
}
