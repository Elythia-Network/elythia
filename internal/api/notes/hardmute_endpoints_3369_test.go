package notes

import (
	"context"
	"net/http"
	"testing"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	coretimeline "github.com/elythia-network/elythia/internal/core/timeline"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// setViewerHardMute makes the "viewer" user hard-mute notes whose text
// contains "muted".
func setViewerHardMute(h *Handler) {
	users := testutil.NewMockUserRepository()
	users.Profiles["viewer"] = &model.UserProfile{UserID: "viewer", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	h.SetUserRepo(users)
}

// TestListEndpoints_ApplyHardMute pins that the list endpoints funnelled
// through packMany drop notes the viewer hard-mutes, while a clean note by the
// same author is still returned (#3369). Each subtest fails when its endpoint
// is switched to packManyWithoutHardMute.
func TestListEndpoints_ApplyHardMute(t *testing.T) {
	viewer := &model.User{ID: "viewer"}

	t.Run("mentions", func(t *testing.T) {
		h, noteRepo, _ := newExtraHandler(t)
		setViewerHardMute(h)
		noteRepo.Notes["clean"] = &model.Note{ID: "clean", UserID: "u2", Visibility: "public", Mentions: []string{"viewer"}, Text: strPtr("hello"), User: &model.User{ID: "u2"}}
		noteRepo.Notes["muted"] = &model.Note{ID: "muted", UserID: "u2", Visibility: "public", Mentions: []string{"viewer"}, Text: strPtr("a muted word"), User: &model.User{ID: "u2"}}
		rec := postExtra(h.Mentions, `{}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"clean"}, responseIDs(t, rec.Body.Bytes()))
	})

	t.Run("user-list-timeline", func(t *testing.T) {
		rows := []*model.Note{
			{ID: "clean", UserID: "member1", Visibility: "public", Text: strPtr("hello"), User: &model.User{ID: "member1"}},
			{ID: "muted", UserID: "member1", Visibility: "public", Text: strPtr("a muted word"), User: &model.User{ID: "member1"}},
		}
		noteRepo := &userListNotesRepo{MockNoteRepository: testutil.NewMockNoteRepository(), rows: rows}
		idGen, _ := id.NewGenerator("aidx")
		querySvc := corenote.NewQueryService(noteRepo, testutil.NewMockFollowingRepository())
		h := newHandlerNilDeps(noteRepo, corenote.NewCreateService(noteRepo, testutil.NewMockPollRepository(), idGen, nil, nil, nil, nil, nil, nil, nil), corenote.NewDeleteService(noteRepo), querySvc, nil, nil, nil, nil, idGen)
		listRepo := testutil.NewMockUserListRepository()
		listRepo.Lists["l1"] = &model.UserList{ID: "l1", UserID: "viewer", Name: "my list"}
		h.SetUserListRepo(listRepo)
		setViewerHardMute(h)
		rec := postExtra(h.UserListTimeline, `{"listId":"l1"}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"clean"}, responseIDs(t, rec.Body.Bytes()))
	})

	t.Run("notes without noteIds", func(t *testing.T) {
		h, noteRepo := newTestHandler(t)
		setViewerHardMute(h)
		noteRepo.Notes["clean"] = &model.Note{ID: "clean", UserID: "u1", Visibility: "public", Text: strPtr("hello"), User: &model.User{ID: "u1"}}
		noteRepo.Notes["muted"] = &model.Note{ID: "muted", UserID: "u1", Visibility: "public", Text: strPtr("a muted word"), User: &model.User{ID: "u1"}}
		rec := postExtra(h.BulkShow, `{"limit":30}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"clean"}, responseIDs(t, rec.Body.Bytes()))
	})

	t.Run("polls/recommendation", func(t *testing.T) {
		idGen, _ := id.NewGenerator("aidx")
		pollRepo := testutil.NewMockPollRepository()
		noteRepo := testutil.NewMockNoteRepository()
		for nid, text := range map[string]string{"clean": "hello", "muted": "a muted word"} {
			noteRepo.Notes[nid] = &model.Note{ID: nid, UserID: "author", Text: strPtr(text), Reactions: datatypes.JSON([]byte("{}"))}
			pollRepo.Polls[nid] = &model.Poll{NoteID: nid, UserID: "author", NoteVisibility: model.NoteVisibilityPublic}
		}
		h := &Handler{idGen: idGen, pollRepo: pollRepo, noteRepo: noteRepo}
		setViewerHardMute(h)
		rec := postDraft(h.PollsRecommendation, `{}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"clean"}, responseIDs(t, rec.Body.Bytes()))
	})

	t.Run("timeline", func(t *testing.T) {
		noteRepo := testutil.NewMockNoteRepository()
		cleanID := seedTimelineNote(noteRepo)
		mutedID := seedTimelineNote(noteRepo)
		require.NotEqual(t, cleanID, mutedID)
		noteRepo.Notes[cleanID].Text = strPtr("hello")
		noteRepo.Notes[mutedID].Text = strPtr("a muted word")
		tl, fanout := newRealTimelineService(t, noteRepo)
		ctx := context.Background()
		for _, nid := range []string{cleanID, mutedID} {
			require.NoError(t, fanout.Push(ctx, coretimeline.HomeTimelineName("viewer"), nid, 100))
		}
		h := newTimelineHandler(t, noteRepo, tl)
		setViewerHardMute(h)
		c, rec := newJSONRequest(t, "/api/notes/timeline", `{}`)
		setAuthUser(c, viewer)
		require.NoError(t, h.Timeline(c))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{cleanID}, responseIDs(t, rec.Body.Bytes()))
	})

	t.Run("search", func(t *testing.T) {
		h, repo := newQueryHandler(t)
		setViewerHardMute(h)
		seedPublicNote(repo, "clean").Text = strPtr("hello clean")
		seedPublicNote(repo, "muted").Text = strPtr("hello muted")
		c, rec := newJSONRequest(t, "/api/notes/search", `{"query":"hello"}`)
		setAuthUser(c, viewer)
		require.NoError(t, h.Search(c))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"clean"}, responseIDs(t, rec.Body.Bytes()))
	})

	t.Run("search-by-tag", func(t *testing.T) {
		h, noteRepo, _ := newExtraHandler(t)
		setViewerHardMute(h)
		noteRepo.Notes["clean"] = &model.Note{ID: "clean", UserID: "u1", Tags: []string{"golang"}, Text: strPtr("#golang"), Visibility: "public", User: &model.User{ID: "u1"}}
		noteRepo.Notes["muted"] = &model.Note{ID: "muted", UserID: "u1", Tags: []string{"golang"}, Text: strPtr("#golang muted"), Visibility: "public", User: &model.User{ID: "u1"}}
		rec := postExtra(h.SearchByTag, `{"tag":"golang"}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"clean"}, responseIDs(t, rec.Body.Bytes()))
	})

	t.Run("featured", func(t *testing.T) {
		h, noteRepo, _ := newExtraHandler(t)
		setViewerHardMute(h)
		noteRepo.Notes["clean"] = &model.Note{ID: "clean", UserID: "u", Text: strPtr("hello"), Visibility: "public", User: &model.User{ID: "u"}}
		noteRepo.Notes["muted"] = &model.Note{ID: "muted", UserID: "u", Text: strPtr("a muted word"), Visibility: "public", User: &model.User{ID: "u"}}
		h.SetFeaturedRanking(&fakeFeaturedReader{global: []string{"clean", "muted"}})
		rec := postExtra(h.Featured, `{}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"clean"}, responseIDs(t, rec.Body.Bytes()))
	})
}
