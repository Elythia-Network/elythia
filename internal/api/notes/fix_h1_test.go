package notes

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #2106 H1: notes/show は ID-known doctrine (#799) で followers/specified note を
// 200 で返すが、非可視 viewer には本文を blank しなければならない (content leak 防止)。
func TestShow_FollowersNoteBlankedForNonViewer(t *testing.T) {
	h, noteRepo := newShowHandler(t)
	noteRepo.Notes["n1"] = &model.Note{
		ID: "n1", UserID: "author", Visibility: "followers",
		Text: strPtr("secret followers body"),
		User: &model.User{ID: "author", Username: "author"},
	}

	// anonymous (非フォロワー): 200 だが本文は blank。
	c, rec := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["isHidden"])
	assert.Nil(t, resp["text"], "followers note body must not leak to a non-viewer")

	// author 本人: full content。
	c2, rec2 := newJSONRequest(t, "/api/notes/show", `{"noteId":"n1"}`)
	setAuthUser(c2, &model.User{ID: "author"})
	require.NoError(t, h.Show(c2))
	require.Equal(t, http.StatusOK, rec2.Code)
	var resp2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Equal(t, "secret followers body", resp2["text"])
	assert.NotEqual(t, true, resp2["isHidden"])
}
