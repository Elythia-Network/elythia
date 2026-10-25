package i

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// i/update refuses a drive file marked as sensitive as the avatar or the
// banner with upstream 2026.10.1's error codes and ids.
func TestUpdate_SensitiveAvatarAndBannerAreRefused(t *testing.T) {
	cases := []struct {
		name, body, code, id string
	}{
		{"avatar", `{"avatarId":"f1"}`, "AVATAR_IS_SENSITIVE", "71bb5e53-4742-4609-b465-36081e131208"},
		{"banner", `{"bannerId":"f1"}`, "BANNER_IS_SENSITIVE", "e148b34c-9f33-4300-93e0-7817008fb366"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo, driveRepo := newSensitiveMediaHandler(t)
			owner := "u1"
			driveRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png", URL: "https://x/a.png", IsSensitive: true}

			rec := post(h.Update, tc.body, userRepo.Users["u1"])
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.code)
			assert.Contains(t, rec.Body.String(), tc.id)
			assert.Nil(t, userRepo.Users["u1"].AvatarID, "the avatar is set although the file is sensitive")
			assert.Nil(t, userRepo.Users["u1"].BannerID, "the banner is set although the file is sensitive")
		})
	}

	t.Run("not sensitive", func(t *testing.T) {
		h, userRepo, driveRepo := newSensitiveMediaHandler(t)
		owner := "u1"
		driveRepo.Files["f1"] = &model.DriveFile{ID: "f1", UserID: &owner, Type: "image/png", URL: "https://x/a.png"}

		rec := post(h.Update, `{"avatarId":"f1"}`, userRepo.Users["u1"])
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotNil(t, userRepo.Users["u1"].AvatarID)
		assert.Equal(t, "f1", *userRepo.Users["u1"].AvatarID)
	})
}

func newSensitiveMediaHandler(t *testing.T) (*Handler, *testutil.MockUserRepository, *testutil.MockDriveFileRepository) {
	t.Helper()
	userRepo := testutil.NewMockUserRepository()
	idGen, _ := id.NewGenerator("aidx")
	svc := coreuser.NewService(userRepo, testutil.NewMockNoteRepository(), testutil.NewMockUserNotePiningRepository(), idGen, nil)
	driveRepo := testutil.NewMockDriveFileRepository()
	svc.SetDriveFileRepository(driveRepo)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	userRepo.Profiles["u1"] = &model.UserProfile{UserID: "u1", Fields: datatypes.JSON([]byte("[]"))}
	return NewHandler(svc, idGen), userRepo, driveRepo
}
