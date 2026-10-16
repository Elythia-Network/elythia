package notes

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestPackMany_AppliesHardMute pins that every list endpoint funnelled through
// packMany (timelines, search, mentions, ...) drops notes hidden by the
// viewer's hardMutedWords, judging a pure renote by the renoted note (#787 /
// #3369).
func TestPackMany_AppliesHardMute(t *testing.T) {
	h, _ := newTestHandler(t)
	users := testutil.NewMockUserRepository()
	users.Profiles["viewer"] = &model.UserProfile{UserID: "viewer", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	h.SetUserRepo(users)

	str := func(s string) *string { return &s }
	target := &model.Note{ID: "target", UserID: "author", Text: str("a muted word"), Visibility: model.NoteVisibilityPublic}
	notes := []*model.Note{
		{ID: "clean", UserID: "author", Text: str("clean"), Visibility: model.NoteVisibilityPublic},
		{ID: "muted", UserID: "author", Text: str("muted"), Visibility: model.NoteVisibilityPublic},
		{ID: "boost", UserID: "booster", RenoteID: &target.ID, Renote: target, Visibility: model.NoteVisibilityPublic},
	}
	out := h.packMany(context.Background(), notes, &model.User{ID: "viewer"})
	require.Len(t, out, 1)
	assert.Equal(t, "clean", out[0].ID)
}

// detailViewHandler returns a query handler whose viewer hard-mutes "muted",
// plus a repo with a public parent note whose text contains the word.
func detailViewHandler(t *testing.T) (*Handler, *testutil.MockNoteRepository, *model.User) {
	t.Helper()
	h, repo := newQueryHandler(t)
	users := testutil.NewMockUserRepository()
	users.Profiles["viewer"] = &model.UserProfile{UserID: "viewer", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	h.SetUserRepo(users)
	parent := seedPublicNote(repo, "p")
	text := "a muted word"
	parent.Text = &text
	return h, repo, &model.User{ID: "viewer"}
}

func responseIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(body, &resp))
	ids := make([]string, 0, len(resp))
	for _, r := range resp {
		ids = append(ids, r["id"].(string))
	}
	return ids
}

// TestDetailViewEndpoints_DoNotApplyHardMute pins that the endpoints the note
// detail view reads keep notes the viewer hard-mutes, like upstream (#3369):
// upstream renders them without withHardMute.
func TestDetailViewEndpoints_DoNotApplyHardMute(t *testing.T) {
	t.Run("children", func(t *testing.T) {
		h, repo, viewer := detailViewHandler(t)
		parent := repo.Notes["p"]
		r := seedPublicNote(repo, "reply")
		text := "muted reply"
		r.Text, r.ReplyID, r.Reply = &text, &parent.ID, parent
		rec := postExtra(h.Children, `{"noteId":"p","limit":50}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"reply"}, responseIDs(t, rec.Body.Bytes()))
	})
	t.Run("replies", func(t *testing.T) {
		h, repo, viewer := detailViewHandler(t)
		parent := repo.Notes["p"]
		r := seedPublicNote(repo, "reply")
		text := "muted reply"
		r.Text, r.ReplyID, r.Reply = &text, &parent.ID, parent
		rec := postExtra(h.Replies, `{"noteId":"p"}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"reply"}, responseIDs(t, rec.Body.Bytes()))
	})
	t.Run("renotes", func(t *testing.T) {
		h, repo, viewer := detailViewHandler(t)
		parent := repo.Notes["p"]
		r := seedPublicNote(repo, "boost")
		r.RenoteID, r.Renote = &parent.ID, parent
		rec := postExtra(h.Renotes, `{"noteId":"p"}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"boost"}, responseIDs(t, rec.Body.Bytes()))
	})
	t.Run("conversation", func(t *testing.T) {
		h, repo, viewer := detailViewHandler(t)
		leaf := seedPublicNote(repo, "leaf")
		pid := "p"
		leaf.ReplyID = &pid
		rec := postExtra(h.Conversation, `{"noteId":"leaf"}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"p"}, responseIDs(t, rec.Body.Bytes()))
	})
	t.Run("show-partial-bulk", func(t *testing.T) {
		h, _, viewer := detailViewHandler(t)
		rec := postExtra(h.ShowPartialBulk, `{"noteIds":["p"]}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"p"}, responseIDs(t, rec.Body.Bytes()))
	})
	t.Run("notes with noteIds", func(t *testing.T) {
		h, _, viewer := detailViewHandler(t)
		rec := postExtra(h.BulkShow, `{"noteIds":["p"]}`, viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, []string{"p"}, responseIDs(t, rec.Body.Bytes()))
	})
}

// TestConversation_HiddenAncestorIndependentOfHardMute pins that whether a
// note the viewer cannot see appears in the response never depends on the
// viewer's hard mute rules (#3369).
func TestConversation_HiddenAncestorIndependentOfHardMute(t *testing.T) {
	for _, rules := range []string{`[["secret"]]`, `[["nomatch"]]`, `["/secret/"]`} {
		t.Run(rules, func(t *testing.T) {
			h, repo := newQueryHandler(t)
			users := testutil.NewMockUserRepository()
			users.Profiles["viewer"] = &model.UserProfile{UserID: "viewer", HardMutedWords: datatypes.JSON([]byte(rules))}
			h.SetUserRepo(users)
			gp := seedPublicNote(repo, "gp")
			secretTxt := "secret content"
			secret := &model.Note{ID: "secret", UserID: "author", Visibility: model.NoteVisibilityFollowers, ReplyID: &gp.ID, Text: &secretTxt, Reactions: datatypes.JSON([]byte("{}"))}
			putUserOnNote(secret)
			repo.Notes["secret"] = secret
			leaf := seedPublicNote(repo, "leaf")
			sID := "secret"
			leaf.ReplyID = &sID
			rec := postExtra(h.Conversation, `{"noteId":"leaf"}`, &model.User{ID: "viewer"})
			require.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, []string{"secret", "gp"}, responseIDs(t, rec.Body.Bytes()))
			assert.NotContains(t, rec.Body.String(), "secret content")
		})
	}
}
