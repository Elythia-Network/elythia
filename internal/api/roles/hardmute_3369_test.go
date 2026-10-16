package roles_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestNotes_AppliesHardMute(t *testing.T) {
	h, roleRepo := newTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true}
	userRepo := testutil.NewMockUserRepository()
	userRepo.Profiles["viewer"] = &model.UserProfile{UserID: "viewer", HardMutedWords: datatypes.JSON([]byte(`[["muted"]]`))}
	h.SetUserRepo(userRepo)

	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	now := time.Now()
	author := &model.User{ID: "author", Username: "author"}
	cleanNote := &model.Note{ID: gen.Generate(now), UserID: author.ID, User: author, Text: strPtr("clean ok"), Visibility: "public"}
	mutedNote := &model.Note{ID: gen.Generate(now), UserID: author.ID, User: author, Text: strPtr("this is muted"), Visibility: "public"}
	targetNote := &model.Note{ID: gen.Generate(now), UserID: author.ID, User: author, Text: strPtr("target muted"), Visibility: "public"}
	// 本文の無い純粋なrenoteは、renote先の本文で判定される。
	pureRenote := &model.Note{ID: gen.Generate(now), UserID: author.ID, User: author, RenoteID: &targetNote.ID, Renote: targetNote, Visibility: "public"}
	h.SetNotesQuery(&mockRoleNotesQuery{Notes: []*model.Note{cleanNote, mutedNote, pureRenote}})

	c := newCtxWithViewer(`{"roleId":"r1"}`, "viewer")
	require.NoError(t, h.Notes(c.ctx))
	require.Equal(t, http.StatusOK, c.rec.Code, c.rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(c.rec.Body.Bytes(), &out))
	require.Len(t, out, 1, c.rec.Body.String())
	assert.Equal(t, cleanNote.ID, out[0]["id"])
}
