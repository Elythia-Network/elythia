package repository

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// seedUGCVisitorNotes creates a local note and a newer remote note (both
// public, same text and tag) plus a newest local renote of the remote note,
// and returns their IDs. The IDs sort as local < remote < renote.
func seedUGCVisitorNotes(t *testing.T) (localID, remoteID, renoteID string) {
	t.Helper()
	repo := NewNoteRepository(testDB)
	local := insertTestUser(t, "u_ugcv_l", "ugcvlocal")
	t.Cleanup(func() { cleanupUser(t, local.ID) })
	host := "remote.example"
	remote := insertRemoteTestUser(t, "u_ugcv_r", "ugcvremote", host)
	t.Cleanup(func() { cleanupUser(t, remote.ID) })

	text := "ugcvisitortext"
	localID, remoteID, renoteID = "n_ugcv_1", "n_ugcv_2", "n_ugcv_3"
	notes := []*model.Note{
		{ID: localID, UserID: local.ID, Text: &text, Tags: []string{"ugcvtag"}, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: remoteID, UserID: remote.ID, UserHost: &host, Text: &text, Tags: []string{"ugcvtag"}, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
		{ID: renoteID, UserID: local.ID, RenoteID: &remoteID, RenoteUserID: &remote.ID, RenoteUserHost: &host, Visibility: model.NoteVisibilityPublic, Reactions: datatypes.JSON([]byte("{}"))},
	}
	for _, n := range notes {
		require.NoError(t, repo.Create(n))
		t.Cleanup(func() { cleanupNote(t, n.ID) })
	}
	return localID, remoteID, renoteID
}

// LocalUsersOnly is applied before LIMIT: with limit 1 and the remote note
// newer than the local one, the page still gets the local note.
func TestNoteRepository_SearchByFilter_LocalUsersOnly(t *testing.T) {
	repo := NewNoteRepository(testDB)
	localID, remoteID, _ := seedUGCVisitorNotes(t)

	out, err := repo.SearchByFilter(model.NoteSearchFilter{Query: "ugcvisitortext", Limit: 1, LocalUsersOnly: true})
	require.NoError(t, err)
	assert.Equal(t, []string{localID}, idsOf(out))

	out, err = repo.SearchByFilter(model.NoteSearchFilter{Query: "ugcvisitortext", Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{remoteID}, idsOf(out))
}

func TestNoteRepository_SearchByTag_LocalUsersOnly(t *testing.T) {
	repo := NewNoteRepository(testDB)
	localID, remoteID, _ := seedUGCVisitorNotes(t)

	out, err := repo.SearchByTag([][]string{{"ugcvtag"}}, "", 1, "", "", model.NoteSearchTagFilter{LocalUsersOnly: true})
	require.NoError(t, err)
	assert.Equal(t, []string{localID}, idsOf(out))

	out, err = repo.SearchByTag([][]string{{"ugcvtag"}}, "", 1, "", "", model.NoteSearchTagFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{remoteID}, idsOf(out))
}

// The timeline filter looks at the note's own author only: a local renote of
// a remote note stays (upstream keeps attached remote notes).
func TestNoteRepository_ListGlobalTimeline_LocalUsersOnly(t *testing.T) {
	repo := NewNoteRepository(testDB)
	localID, remoteID, renoteID := seedUGCVisitorNotes(t)

	// 他のテストのノートと混ざらないよう、cursor で範囲を自分のノートに絞る。
	out, err := repo.ListGlobalTimeline(2, "n_ugcv_0", "n_ugcv_9", model.TimelineDBFilter{LocalUsersOnly: true})
	require.NoError(t, err)
	assert.Equal(t, []string{renoteID, localID}, idsOf(out))

	out, err = repo.ListGlobalTimeline(2, "n_ugcv_0", "n_ugcv_9", model.TimelineDBFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{renoteID, remoteID}, idsOf(out))
}
