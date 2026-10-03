package i

import (
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/testutil"
)

// TestHandlers_BindErrorIsInvalidParam pins that a body these endpoints
// cannot bind (a JSON array, not an object) is answered with 400
// INVALID_PARAM, as upstream's ajv `type: 'object'` check does. The bind
// error used to be ignored (#3330).
func TestHandlers_BindErrorIsInvalidParam(t *testing.T) {
	h, _ := newExtraHandler(t)
	// 依存が未配線だと bind より前に早期 return するので配線する。
	h.SetAccessTokenRepo(testutil.NewMockAccessTokenRepository())
	h.SetRegistryRepo(testutil.NewMockRegistryRepository())
	h.SetTransferEnqueuer(&stubTransferEnqueuer{})
	h.SetGalleryRepo(&stubGalleryRepo{})
	h.SetPageRepo(testutil.NewMockPageRepository())
	h.SetPageLikeRepo(testutil.NewMockPageLikeRepository())
	cases := map[string]func(echo.Context) error{
		"Apps":            h.Apps,
		"AuthorizedApps":  h.AuthorizedApps,
		"RegistryKeys":    h.RegistryKeys,
		"ExportNotes":     h.ExportNotes,
		"ExportFollowing": h.ExportFollowing,
		"GalleryLikes":    h.GalleryLikes,
		"GalleryPosts":    h.GalleryPosts,
		"PageLikes":       h.PageLikes,
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			testutil.AssertInvalidParam(t, postExtra(fn, `[]`, stubUser))
		})
	}
}
