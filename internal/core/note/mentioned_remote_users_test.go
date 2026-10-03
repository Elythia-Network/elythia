package note_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// TestCreateService_WritesMentionedRemoteUsers checks the mentionedRemoteUsers
// column against upstream NoteCreateService.insertNote (#3329): the remote
// users among the mentions in mention order, with the profile url (the key is
// omitted when the profile has none) and without escaping `&`.
func TestCreateService_WritesMentionedRemoteUsers(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	remote := "remote.example"
	other := "other.example"
	addUser(repo, "alice-id", "alice", nil)
	addUser(repo, "bob-id", "Bob", &remote)
	addUser(repo, "carol-id", "carol", &other)
	repo.Users["bob-id"].URI = new(string("https://remote.example/users/bob"))
	repo.Users["carol-id"].URI = new(string("https://other.example/u/carol"))
	repo.Profiles["bob-id"] = &model.UserProfile{UserID: "bob-id", URL: new(string("https://remote.example/@Bob?a=1&b=2"))}
	repo.Profiles["carol-id"] = &model.UserProfile{UserID: "carol-id"}

	text := "@carol@other.example @alice @bob@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t,
		`[{"uri":"https://other.example/u/carol","username":"carol","host":"other.example"},`+
			`{"uri":"https://remote.example/users/bob","url":"https://remote.example/@Bob?a=1&b=2","username":"Bob","host":"remote.example"}]`,
		created.MentionedRemoteUsers)
}

// TestCreateService_MentionedRemoteUsersLocalOnly checks that a note that
// mentions only local users gets an empty array, like upstream's
// JSON.stringify([]).
func TestCreateService_MentionedRemoteUsersLocalOnly(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	addUser(repo, "alice-id", "alice", nil)

	text := "@alice"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, "[]", created.MentionedRemoteUsers)
}

// TestCreateService_MentionedRemoteUsersWithoutProfile checks that a remote
// user without a profile row is written without url, so the content links to
// the user's uri.
func TestCreateService_MentionedRemoteUsersWithoutProfile(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	remote := "remote.example"
	addUser(repo, "bob-id", "bob", &remote)
	repo.Users["bob-id"].URI = new(string("https://remote.example/users/bob"))

	text := "@bob@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, `[{"uri":"https://remote.example/users/bob","username":"bob","host":"remote.example"}]`, created.MentionedRemoteUsers)
}

// TestCreateService_MentionedRemoteUsersLookupFailure checks that a failed
// user lookup does not stop the note from being created.
//
// 本家は作成ごと失敗するが、mk-go は列を空にして続ける。
func TestCreateService_MentionedRemoteUsersLookupFailure(t *testing.T) {
	svc, repo, _ := newMentionTestService(t)
	remote := "remote.example"
	addUser(repo, "bob-id", "bob", &remote)
	repo.FindManyByIDsErr = errors.New("boom")

	text := "@bob@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"bob-id"}, []string(created.Mentions))
	assert.Equal(t, "[]", created.MentionedRemoteUsers)
}

// TestCreateService_MentionedRemoteUsersIncludesWebFingerResolved checks that
// a remote user resolved via WebFinger during creation is written to the
// column, i.e. the column is built after note.Mentions is final.
func TestCreateService_MentionedRemoteUsersIncludesWebFingerResolved(t *testing.T) {
	svc, repo, _, f := newRemoteMentionService(t)
	addUser(repo, "local-id", "local", nil)
	f.known["carol@remote.example"] = "carol-id"
	repo.Profiles["carol-id"] = &model.UserProfile{UserID: "carol-id", URL: new(string("https://remote.example/@carol"))}

	text := "@local @Carol@remote.example"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text})
	require.NoError(t, err)
	assert.Equal(t, []string{"local-id", "carol-id"}, []string(created.Mentions))
	assert.Equal(t, `[{"uri":"https://remote.example/users/carol-id","url":"https://remote.example/@carol","username":"carol","host":"remote.example"}]`, created.MentionedRemoteUsers)
}

// findManyCountingUserRepo counts FindManyByIDs calls on top of the mock.
type findManyCountingUserRepo struct {
	*testutil.MockUserRepository
	findManyByIDs int
}

func (c *findManyCountingUserRepo) FindManyByIDs(ids []string) ([]*model.User, error) {
	c.findManyByIDs++
	return c.MockUserRepository.FindManyByIDs(ids)
}

func newCountingMentionService(t *testing.T) (*note.CreateService, *findManyCountingUserRepo, *testutil.MockNoteRepository) {
	t.Helper()
	svc, noteRepo, _ := newCreateService(t)
	repo := &findManyCountingUserRepo{MockUserRepository: testutil.NewMockUserRepository()}
	svc.SetUserRepo(repo)
	return svc, repo, noteRepo
}

// TestCreateService_MentionedRemoteUsersSkipsLookupForLocal checks that a note
// whose mentions are all known to be local (resolved local mentions and the
// author of a local replied note) does not look the users up again, and still
// gets `[]`.
func TestCreateService_MentionedRemoteUsersSkipsLookupForLocal(t *testing.T) {
	svc, repo, noteRepo := newCountingMentionService(t)
	addUser(repo.MockUserRepository, "alice-id", "alice", nil)
	noteRepo.Notes["local-note"] = &model.Note{ID: "local-note", UserID: "carol-id", Visibility: model.NoteVisibilityPublic}

	text := "@alice"
	replyID := "local-note"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, ReplyID: &replyID})
	require.NoError(t, err)
	assert.Equal(t, []string{"alice-id", "carol-id"}, []string(created.Mentions))
	assert.Equal(t, "[]", created.MentionedRemoteUsers)
	assert.Zero(t, repo.findManyByIDs)
}

// TestCreateService_MentionedRemoteUsersIncludesRemoteReplyAuthor checks that
// the author of a remote replied note is looked up and written, as upstream
// pushes the reply author into mentionedUsers.
func TestCreateService_MentionedRemoteUsersIncludesRemoteReplyAuthor(t *testing.T) {
	svc, repo, noteRepo := newCountingMentionService(t)
	remote := "remote.example"
	addUser(repo.MockUserRepository, "alice-id", "alice", nil)
	addUser(repo.MockUserRepository, "bob-id", "bob", &remote)
	repo.Users["bob-id"].URI = new(string("https://remote.example/users/bob"))
	noteRepo.Notes["remote-note"] = &model.Note{ID: "remote-note", UserID: "bob-id", UserHost: &remote, Visibility: model.NoteVisibilityPublic}

	text := "@alice"
	replyID := "remote-note"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, ReplyID: &replyID})
	require.NoError(t, err)
	assert.Equal(t, `[{"uri":"https://remote.example/users/bob","username":"bob","host":"remote.example"}]`, created.MentionedRemoteUsers)
	assert.Equal(t, 1, repo.findManyByIDs)
}

// TestCreateService_MentionedRemoteUsersLooksUpSpecifiedRecipients checks that
// recipients of a specified note, whose hosts are not known here, are looked
// up and written when remote.
func TestCreateService_MentionedRemoteUsersLooksUpSpecifiedRecipients(t *testing.T) {
	svc, repo, _ := newCountingMentionService(t)
	remote := "remote.example"
	addUser(repo.MockUserRepository, "bob-id", "bob", &remote)
	repo.Users["bob-id"].URI = new(string("https://remote.example/users/bob"))

	text := "hi"
	created, err := svc.Create(note.CreateInput{User: &model.User{ID: "author"}, Text: &text, Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"bob-id"}})
	require.NoError(t, err)
	assert.Equal(t, `[{"uri":"https://remote.example/users/bob","username":"bob","host":"remote.example"}]`, created.MentionedRemoteUsers)
	assert.Equal(t, 1, repo.findManyByIDs)
}
