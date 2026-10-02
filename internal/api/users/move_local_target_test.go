package users

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestShow_MoveTargetResolution(t *testing.T) {
	localMoveURI := "https://local.example/users/destination"
	remoteMoveURI := "https://remote.example/users/destination"
	unknownMoveURI := "https://unknown.example/users/destination"
	remoteHost := "remote.example"

	tests := []struct {
		name        string
		sourceHost  *string
		movedToURI  *string
		destination *model.User
		wantMovedTo any
	}{
		{
			name:       "local source to local destination",
			movedToURI: &localMoveURI,
			destination: &model.User{
				ID: "destination", Username: "destination", UsernameLower: "destination",
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			},
			wantMovedTo: "destination",
		},
		{
			name:       "remote source to local destination",
			sourceHost: &remoteHost,
			movedToURI: &localMoveURI,
			destination: &model.User{
				ID: "destination", Username: "destination", UsernameLower: "destination",
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			},
			wantMovedTo: "destination",
		},
		{
			name:       "known remote destination",
			movedToURI: &remoteMoveURI,
			destination: &model.User{
				ID: "destination", Username: "destination", UsernameLower: "destination",
				Host: &remoteHost, URI: &remoteMoveURI,
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			},
			wantMovedTo: "destination",
		},
		{
			name:        "missing local destination",
			movedToURI:  &localMoveURI,
			wantMovedTo: nil,
		},
		{
			name:        "unknown destination",
			movedToURI:  &unknownMoveURI,
			wantMovedTo: nil,
		},
		{
			name:        "unmigrated user",
			wantMovedTo: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, userRepo := newTestHandler(t)
			source := &model.User{
				ID: "source", Username: "source", UsernameLower: "source",
				Host: tt.sourceHost, MovedToURI: tt.movedToURI,
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			}
			userRepo.Users[source.ID] = source
			if tt.destination != nil {
				userRepo.Users[tt.destination.ID] = tt.destination
			}
			h.SetUserRepo(userRepo)
			h.SetServerURL("https://local.example")

			rec := postStub(h.Show, `{"userId":"source"}`, nil)
			require.Equal(t, http.StatusOK, rec.Code)
			var response map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			assert.Equal(t, tt.wantMovedTo, response["movedTo"])
		})
	}
}
