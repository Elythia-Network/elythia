package notes

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家 NoteReactionEntityService.packMany は利用者を packMany (UserLite) で組むので、
// リモートの利用者の instance と絵文字を埋める (#3330)。
func TestReactions_List_ResolvesRemoteInstanceAndEmojis(t *testing.T) {
	h, repo, reactRepo := newReactionHandler(t)
	seedReactionNote(repo, "n1", "public")
	host := "remote.example"
	instName := "Remote"
	instances := testutil.NewMockInstanceRepository()
	require.NoError(t, instances.Create(&model.Instance{Host: host, Name: &instName}))
	h.SetInstanceRepo(instances)
	emojis := testutil.NewMockEmojiRepository()
	require.NoError(t, emojis.Create(&model.Emoji{ID: "e1", Name: "blobcat", Host: &host, PublicURL: "https://remote.example/blobcat.png"}))
	h.SetEmojiRepo(emojis)
	idGen, _ := id.NewGenerator("aidx")
	rxID := idGen.Generate(timeNow())
	reactRepo.Reactions[rxID] = &model.NoteReaction{
		ID: rxID, UserID: "r1", NoteID: "n1", Reaction: "❤",
		User: &model.User{ID: "r1", Username: "carol", Host: &host, Emojis: model.StringArray{"blobcat"}},
	}

	c, rec := newJSONRequest(t, "/api/notes/reactions", `{"noteId":"n1"}`)
	require.NoError(t, h.Reactions(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp, 1)
	user := resp[0]["user"].(map[string]any)
	assert.Equal(t, "Remote", user["instance"].(map[string]any)["name"])
	assert.Equal(t, map[string]any{"blobcat": "https://remote.example/blobcat.png"}, user["emojis"])
}
