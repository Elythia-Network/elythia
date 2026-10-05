package note_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// Mention resolution and the mention limit follow upstream
// NoteCreateService.create / extractMentionedUsers (#3330).

func newMentionTestService(t *testing.T) (*note.CreateService, *testutil.MockUserRepository, *testutil.MockNoteRepository) {
	t.Helper()
	svc, noteRepo, _ := newCreateService(t)
	repo := testutil.NewMockUserRepository()
	svc.SetUserRepo(repo)
	return svc, repo, noteRepo
}

func addUser(repo *testutil.MockUserRepository, id, username string, host *string) {
	repo.Users[id] = &model.User{ID: id, Username: username, UsernameLower: strings.ToLower(username), Host: host}
}

// `@user@<this instance's host>` is a local user (upstream resolveUser compares
// toPuny(host) with toPuny(config.host)), whatever the case of the host.
func TestCreateService_MentionOfOwnHostResolvesToLocalUser(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	addUser(repo, "alice-id", "alice", nil)
	addUser(repo, "bob-id", "bob", nil)
	svc.SetLocalHost("Local.Example")

	text := "@alice@local.example and @bob@LOCAL.EXAMPLE"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice-id", "bob-id"}, []string(created.Mentions))
}

func TestCreateService_SetLocalHostEmptyDisablesOwnHostShortcut(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	addUser(repo, "alice-id", "alice", nil)
	svc.SetLocalHost("")

	text := "@alice@local.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Empty(t, []string(created.Mentions))
}

// A host-less mention belongs to the author's host (`m.host ?? user.host`).
func TestResolveMentionUserIDs_HostlessUsesAuthorHost(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	remote := "remote.example"
	addUser(repo, "local-bob", "bob", nil)
	addUser(repo, "remote-bob", "bob", &remote)

	got := svc.ResolveMentionUserIDsForTest([]note.Mention{{Username: "bob"}}, &remote)
	assert.Equal(t, []string{"remote-bob"}, got)

	got = svc.ResolveMentionUserIDsForTest([]note.Mention{{Username: "bob"}}, nil)
	assert.Equal(t, []string{"local-bob"}, got)
}

// The same user mentioned twice (`@Alice @alice`) is one mentioned user, in
// note.mentions and in the mention limit.
func TestCreateService_MentionsDedupedByUserID(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	addUser(repo, "alice-id", "Alice", nil)
	svc.SetRolePolicyProvider(&stubRolePolicies{byUser: map[string]map[string]any{
		"author": {"mentionLimit": 1},
	}})

	text := "@Alice @alice"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err, "同じ利用者は上限の計算でも 1 件")
	assert.Equal(t, []string{"alice-id"}, []string(created.Mentions))
}

// Upstream parses the text, the CW and the poll choices and extracts the
// mentions from the combined trees, in this order.
func TestCreateService_MentionsFromCWAndPollChoices(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	addUser(repo, "a-id", "a", nil)
	addUser(repo, "b-id", "b", nil)
	addUser(repo, "c-id", "c", nil)

	text := "text @a"
	cw := "cw @b"
	expires := time.Now().Add(time.Hour)
	created, err := svc.Create(note.CreateInput{
		User: &model.User{ID: "author"},
		Text: &text,
		CW:   &cw,
		Poll: &note.PollInput{Choices: []string{"@c", "@a again"}, ExpiresAt: &expires},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-id", "b-id", "c-id"}, []string(created.Mentions))

	t.Run("poll without text", func(t *testing.T) {
		created, err := svc.Create(note.CreateInput{
			User: &model.User{ID: "author"},
			Poll: &note.PollInput{Choices: []string{"@c", "x"}, ExpiresAt: &expires},
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"c-id"}, []string(created.Mentions))
	})

	t.Run("noExtractMentions", func(t *testing.T) {
		created, err := svc.Create(note.CreateInput{
			User:              &model.User{ID: "author"},
			Text:              &text,
			CW:                &cw,
			NoExtractMentions: true,
		})
		require.NoError(t, err)
		assert.Empty(t, []string(created.Mentions))
	})
}

// The mention limit counts the CW's mentions too.
func TestCreateService_MentionLimitCountsCWMentions(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	addUser(repo, "a-id", "a", nil)
	addUser(repo, "b-id", "b", nil)
	svc.SetRolePolicyProvider(&stubRolePolicies{byUser: map[string]map[string]any{
		"author": {"mentionLimit": 1},
	}})

	text := "@a"
	cw := "@b"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, CW: &cw})
	assert.ErrorIs(t, err, note.ErrContainsTooManyMentions)
}

// Upstream counts the resolved users only: a mention that resolves to no
// user is not counted, and noExtractMentions passes apMentions: [] so the
// text's mentions are not counted either.
func TestCreateService_MentionLimitCountsResolvedUsersOnly(t *testing.T) {
	over := ""
	for i := 0; i < note.DefaultMentionLimit+1; i++ {
		over += "@user" + strPtr254Str(i) + " "
	}

	t.Run("unresolved mentions are not counted", func(t *testing.T) {
		svc, _, _ := newMentionTestService(t)
		_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &over})
		assert.NoError(t, err)
	})

	t.Run("noExtractMentions does not count the text", func(t *testing.T) {
		svc, _, _ := newCreateService(t)
		withMentionableUsers(svc, note.DefaultMentionLimit+1)
		_, err := svc.Create(note.CreateInput{
			User: &model.User{ID: "author"}, Text: &over, NoExtractMentions: true,
		})
		assert.NoError(t, err)
	})
}

// A reply to the author's own note does not add the author to the mentioned
// users (`user.id !== data.reply.userId`), so mentionLimit 0 still allows it.
func TestCreateService_MentionLimitIgnoresSelfReply(t *testing.T) {
	svc, _, noteRepo := newMentionTestService(t)
	svc.SetRolePolicyProvider(&stubRolePolicies{byUser: map[string]map[string]any{
		"author": {"mentionLimit": 0},
	}})
	noteRepo.Notes["own"] = &model.Note{ID: "own", UserID: "author", Visibility: model.NoteVisibilityPublic}

	text := "self reply"
	replyID := "own"
	_, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, ReplyID: &replyID})
	assert.NoError(t, err)

	noteRepo.Notes["other"] = &model.Note{ID: "other", UserID: "someone", Visibility: model.NoteVisibilityPublic}
	otherID := "other"
	_, err = svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, ReplyID: &otherID})
	assert.ErrorIs(t, err, note.ErrContainsTooManyMentions, "他人への返信は返信先の作者を数える")
}
