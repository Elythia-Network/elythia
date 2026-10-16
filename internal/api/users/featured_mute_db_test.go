package users

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// On the real repositories and the relation cache (#3607), users/featured-notes
// keeps the muted target's own notes and drops the ones replying to or
// renoting another muted user, as upstream isUserRelated(note, mutings, true)
// does (#3369). The rows come from the real SQL fallback path, which fills
// both the replyUserId / renoteUserId columns and the reply / renote embeds,
// so this does not pin which of the two the check reads.
func TestFeaturedNotes_DBMutedTargetKeepsOwnNotes(t *testing.T) {
	db := openFollowersDB(t)
	const p = "ufm3369_"
	t.Cleanup(func() {
		db.Exec(`DELETE FROM "note" WHERE id LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "muting" WHERE id LIKE ?`, p+"%")
		db.Exec(`DELETE FROM "user" WHERE id LIKE ?`, p+"%")
	})
	for _, u := range []string{"t", "v", "m"} {
		id := p + u
		require.NoError(t, db.Create(&model.User{ID: id, Username: id, UsernameLower: id, AvatarDecorations: datatypes.JSON([]byte("[]"))}).Error)
	}
	s := func(v string) *string { return &v }
	notes := []*model.Note{
		{ID: p + "a0", UserID: p + "m", Visibility: "public", Text: s("by muted other")},
		{ID: p + "n1", UserID: p + "t", Visibility: "public", Text: s("own"), RenoteCount: 3},
		{ID: p + "n2", UserID: p + "t", Visibility: "public", Text: s("reply to muted"), ReplyID: s(p + "a0"), ReplyUserID: s(p + "m")},
		{ID: p + "n3", UserID: p + "t", Visibility: "public", RenoteID: s(p + "a0"), RenoteUserID: s(p + "m")},
		{ID: p + "n4", UserID: p + "t", Visibility: "public", Text: s("self reply"), ReplyID: s(p + "n1"), ReplyUserID: s(p + "t")},
	}
	for _, n := range notes {
		require.NoError(t, db.Create(n).Error)
	}
	for i, mutee := range []string{"t", "m"} {
		require.NoError(t, db.Create(&model.Muting{ID: fmt.Sprintf("%smu%d", p, i), MuterID: p + "v", MuteeID: p + mutee}).Error)
	}

	h := dbFollowersHandler(t, db)
	h.SetRelationCache(repository.NewRelationCache(db))
	rec := postStub(h.FeaturedNotes, fmt.Sprintf(`{"userId":%q}`, p+"t"), &model.User{ID: p + "v"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	got := make([]string, 0, len(out))
	for _, n := range out {
		got = append(got, n["id"].(string))
	}
	assert.Equal(t, []string{p + "n4", p + "n1"}, got)
}
