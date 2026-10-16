package reaction_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/reaction"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rowNoteRepo returns a fresh copy of the stored row from FindByID with the
// reactions JSON the counter writes produced, like the DB row after the write.
type rowNoteRepo struct {
	*testutil.MockNoteRepository
	withRelations int
	rowReads      int
	rowErr        error
}

func (r *rowNoteRepo) FindByIDWithRelations(id string) (*model.Note, error) {
	r.withRelations++
	return r.MockNoteRepository.FindByIDWithRelations(id)
}

func (r *rowNoteRepo) FindByID(id string) (*model.Note, error) {
	r.rowReads++
	if r.rowErr != nil {
		return nil, r.rowErr
	}
	n, err := r.MockNoteRepository.FindByID(id)
	if err != nil {
		return nil, err
	}
	row := *n
	row.User, row.Poll, row.Renote, row.Reply = nil, nil, nil, nil
	raw, _ := json.Marshal(r.ReactionCounts[id])
	row.Reactions = raw
	return &row, nil
}

type loadingHook struct {
	created   int
	loads     []func() (*model.Note, error)
	notifiee  string
	reactions []string
}

func (h *loadingHook) OnReactionCreated(string, string, string, string) { h.created++ }
func (h *loadingHook) OnReactionRemoved(string, string, string)         {}
func (h *loadingHook) OnReactionCreatedWithNote(notifieeID, _, _, reaction string, load func() (*model.Note, error)) {
	h.notifiee = notifieeID
	h.reactions = append(h.reactions, reaction)
	h.loads = append(h.loads, load)
}

// The notification gets the reacted note as it is after the write: the row is
// read again (so the new reaction is counted) while the relations come from
// the note read at the start, and the full relation read happens only once
// (#3611; it used to happen three times per reaction).
func TestService_Create_NotificationLoadsNoteAfterWrite(t *testing.T) {
	base := testutil.NewMockNoteRepository()
	repo := &rowNoteRepo{MockNoteRepository: base}
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(repo, testutil.NewMockNoteReactionRepository(), testutil.NewMockEmojiRepository(), testutil.NewMockFollowingRepository(), idGen)
	hook := &loadingHook{}
	svc.SetNotificationHook(hook)

	author := &model.User{ID: "author"}
	poll := &model.Poll{NoteID: "n1"}
	renoteID := "quoted"
	text := "body"
	base.Notes["quoted"] = &model.Note{ID: "quoted", UserID: "other", Visibility: model.NoteVisibilityPublic}
	replyID := "parent"
	base.Notes["parent"] = &model.Note{ID: "parent", UserID: "other", Visibility: model.NoteVisibilityPublic}
	base.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic, User: author, Poll: poll, HasPoll: true, RenoteID: &renoteID, ReplyID: &replyID}

	_, err := svc.Create(&model.User{ID: "reactor"}, "n1", "\U0001F44D")
	require.NoError(t, err)
	require.Equal(t, 1, repo.withRelations, "the note is read with relations once")
	require.Zero(t, hook.created, "the loading hook variant is used")
	require.Len(t, hook.loads, 1)
	assert.Equal(t, "author", hook.notifiee)
	require.Zero(t, repo.rowReads, "the row is read only when the notification needs it")

	got, err := hook.loads[0]()
	require.NoError(t, err)
	assert.Equal(t, 1, repo.rowReads)
	assert.JSONEq(t, `{"👍":1}`, string(got.Reactions), "the row is read after the reaction is counted")
	assert.Same(t, author, got.User)
	assert.Same(t, poll, got.Poll)
	require.NotNil(t, got.Renote)
	assert.Equal(t, "quoted", got.Renote.ID)
	assert.Same(t, base.Notes["quoted"], got.Renote)
	assert.Same(t, base.Notes["parent"], got.Reply, "the reply target is carried over (review L-1)")
	assert.NotSame(t, base.Notes["n1"], got, "the stored row is not modified")
	assert.Nil(t, base.Notes["quoted"].User)

	repo.rowErr = errors.New("boom")
	svc2 := reaction.NewService(repo, testutil.NewMockNoteReactionRepository(), testutil.NewMockEmojiRepository(), testutil.NewMockFollowingRepository(), idGen)
	hook2 := &loadingHook{}
	svc2.SetNotificationHook(hook2)
	_, err = svc2.Create(&model.User{ID: "reactor2"}, "n1", "\U0001F44D")
	require.NoError(t, err)
	require.Len(t, hook2.loads, 1)
	_, err = hook2.loads[0]()
	assert.Error(t, err)
}

// Self reactions create no notification and so never load the note.
func TestService_Create_SelfReactionDoesNotLoad(t *testing.T) {
	base := testutil.NewMockNoteRepository()
	repo := &rowNoteRepo{MockNoteRepository: base}
	idGen, _ := id.NewGenerator("aidx")
	svc := reaction.NewService(repo, testutil.NewMockNoteReactionRepository(), testutil.NewMockEmojiRepository(), testutil.NewMockFollowingRepository(), idGen)
	hook := &loadingHook{}
	svc.SetNotificationHook(hook)
	text := "body"
	base.Notes["n1"] = &model.Note{ID: "n1", UserID: "author", Text: &text, Visibility: model.NoteVisibilityPublic}

	_, err := svc.Create(&model.User{ID: "author"}, "n1", "\U0001F44D")
	require.NoError(t, err)
	assert.Empty(t, hook.loads)
	assert.Zero(t, repo.rowReads)
}
