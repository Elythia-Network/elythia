package users

import (
	"context"
	"testing"

	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// FillDetailedExtras は users/show 以外 (main stream の follow 系イベント、
// Webhook、blocking/create・delete) が使う。ピン留めと移行先を users/show と
// 同じ規則で埋める (#3330)。
func TestFillDetailedExtras_PinnedAndMoveTargets(t *testing.T) {
	h, userRepo := newTestHandler(t)
	owner := addTestUser(userRepo)
	movedTo := "https://local.example/users/destination"
	aka := "https://local.example/users/destination,https://unknown.example/users/x"
	owner.MovedToURI = &movedTo
	owner.AlsoKnownAs = &aka
	hidden := 0
	owner.MakeNotesHiddenBefore = &hidden
	userRepo.Users["destination"] = &model.User{ID: "destination", Username: "destination", UsernameLower: "destination",
		AvatarDecorations: datatypes.JSON([]byte("[]"))}
	h.SetUserRepo(userRepo)
	h.SetServerURL("https://local.example")

	piningRepo := testutil.NewMockUserNotePiningRepository()
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p1", UserID: owner.ID, NoteID: "pub"}))
	require.NoError(t, piningRepo.Create(&model.UserNotePining{ID: "p2", UserID: owner.ID, NoteID: "fol"}))
	h.SetPiningRepo(piningRepo)
	nr := h.noteRepo.(*testutil.MockNoteRepository)
	text := "pinned"
	nr.Notes["pub"] = &model.Note{ID: "pub", UserID: owner.ID, User: owner, Text: &text, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))}
	nr.Notes["fol"] = &model.Note{ID: "fol", UserID: owner.ID, User: owner, Text: &text, Visibility: model.NoteVisibilityFollowers, Reactions: datatypes.JSON([]byte("{}"))}

	pageID := "pg"
	profile := &model.UserProfile{UserID: owner.ID, PinnedPageID: &pageID}
	h.SetPageRepo(&stubPageRepoForPin{page: &model.Page{ID: pageID, Title: "page", UserID: owner.ID}})

	d := entity.PackUserDetailed(owner, profile, h.idGen)
	h.FillDetailedExtras(context.Background(), &model.User{ID: "viewer"}, owner, profile, &d)

	require.NotNil(t, d.MovedTo)
	assert.Equal(t, "destination", *d.MovedTo)
	assert.Equal(t, []string{"destination"}, d.AlsoKnownAs)
	// pinnedNoteIds は filter 前の生 ID、本文は閲覧者から見えるものだけ (users/show と同じ)。
	assert.ElementsMatch(t, []string{"pub", "fol"}, d.PinnedNoteIDs)
	require.Len(t, d.PinnedNotes, 1)
	pinned := d.PinnedNotes[0].(entity.NoteEntity)
	assert.Equal(t, "pub", pinned.ID)
	// public のピン留めは作者の時限非公開を越えて見える (#3310)。
	require.NotNil(t, pinned.Text)
	assert.Equal(t, text, *pinned.Text)
	require.NotNil(t, d.PinnedPageID)
	assert.Equal(t, pageID, *d.PinnedPageID)
	assert.NotNil(t, d.PinnedPage)

	// 閲覧者がフォロワーなら followers のピン留めも本文が出る。閲覧者を渡し損ねる
	// (nil にする) と、フォロワーにも見えなくなる。
	followingRepo := testutil.NewMockFollowingRepository()
	require.NoError(t, followingRepo.Create(&model.Following{ID: "f1", FollowerID: "follower", FolloweeID: owner.ID}))
	h.SetFollowingRepo(followingRepo)
	d = entity.PackUserDetailed(owner, profile, h.idGen)
	h.FillDetailedExtras(context.Background(), &model.User{ID: "follower"}, owner, profile, &d)
	ids := make([]string, 0, len(d.PinnedNotes))
	for _, n := range d.PinnedNotes {
		ids = append(ids, n.(entity.NoteEntity).ID)
	}
	assert.ElementsMatch(t, []string{"pub", "fol"}, ids)
}
