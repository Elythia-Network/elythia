package i

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestMe_ResolvesLocalMoveTargetWithoutURIColumn(t *testing.T) {
	h, userRepo, _, _ := newTestHandler(t)
	movedToURI := "https://local.example/users/destination"
	source := &model.User{
		ID:                "source",
		Username:          "source",
		UsernameLower:     "source",
		MovedToURI:        &movedToURI,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
		ChatScope:         "mutual",
	}
	destination := &model.User{
		ID:                "destination",
		Username:          "destination",
		UsernameLower:     "destination",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	userRepo.Users[source.ID] = source
	userRepo.Users[destination.ID] = destination // Local users have URI == nil.
	userRepo.Profiles[source.ID] = &model.UserProfile{
		UserID:              source.ID,
		Fields:              datatypes.JSON([]byte("[]")),
		FollowersVisibility: model.FollowingVisibilityPublic,
		FollowingVisibility: model.FollowingVisibilityPublic,
	}
	h.SetUserRepo(userRepo)
	h.SetServerURL("https://local.example")

	rec := post(h.Me, `{}`, source)
	require.Equal(t, http.StatusOK, rec.Code)
	var response map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	assert.Equal(t, destination.ID, response["movedTo"])
}
