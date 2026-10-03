package blocking

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/userrelation"
	coreblocking "github.com/shiroha-a/mk/internal/core/blocking"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pinExtras fills pinned notes and movedTo as the users handler does.
type pinExtras struct{ viewerID string }

func (p *pinExtras) FillDetailedExtras(_ context.Context, viewer, _ *model.User, _ *model.UserProfile, d *entity.UserDetailed) {
	p.viewerID = viewer.ID
	d.PinnedNoteIDs = []string{"pinned-note"}
	moved := "destination"
	d.MovedTo = &moved
}

// blocking/create・delete は本家と同じく pack(blockee, blocker, UserDetailedNotMe)
// を返す。ピン留めと移行先も users/show と同じ規則で埋める (#3330)。
func TestCreateDelete_UsePacker(t *testing.T) {
	userRepo := testutil.NewMockUserRepository()
	blockingRepo := testutil.NewMockBlockingRepository()
	idGen, _ := id.NewGenerator("aidx")
	h := NewHandler(coreblocking.NewService(userRepo, blockingRepo, idGen), userRepo, idGen)
	relations := userrelation.Repos{Blocking: blockingRepo}
	h.SetRelationRepos(relations)
	extras := &pinExtras{}
	h.SetUserPacker(userpack.New(userpack.Lookups{Profiles: userRepo, Relations: relations, Extras: extras}, idGen))
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	userRepo.Profiles["bob"] = &model.UserProfile{UserID: "bob"}

	for _, tc := range []struct {
		name       string
		call       echo.HandlerFunc
		isBlocking bool
	}{
		{"create", h.Create, true},
		{"delete", h.Delete, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newReq(t, `{"userId":"bob"}`)
			setUser(c, "alice")
			require.NoError(t, tc.call(c))
			require.Equal(t, http.StatusOK, rec.Code)
			var resp map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, "bob", resp["id"])
			assert.Equal(t, []any{"pinned-note"}, resp["pinnedNoteIds"])
			assert.Equal(t, "destination", resp["movedTo"])
			assert.Equal(t, tc.isBlocking, resp["isBlocking"])
			assert.Equal(t, "alice", extras.viewerID, "pinned notes are gated by the blocker")
		})
	}
}

// profile が無いときは従来の組み方に落とさず 500 にする。profile 無しで組むと
// followersVisibility が public に倒れて、伏せるべきカウントが出る (本家は
// findOneByOrFail の例外で 500)。ブロック自体は済んでいる。
func TestCreate_PackerFailsClosedWithoutProfile(t *testing.T) {
	h, userRepo := newHandler(t)
	extras := &pinExtras{}
	idGen, _ := id.NewGenerator("aidx")
	h.SetUserPacker(userpack.New(userpack.Lookups{Profiles: userRepo, Extras: extras}, idGen))
	addUser(userRepo, "alice")
	addUser(userRepo, "bob")
	userRepo.Users["bob"].FollowersCount = 7

	c, rec := newReq(t, `{"userId":"bob"}`)
	setUser(c, "alice")
	require.NoError(t, h.Create(c))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "followersCount")
	assert.Empty(t, extras.viewerID)
	blocked, err := h.svc.IsBlocked("alice", "bob")
	require.NoError(t, err)
	assert.True(t, blocked, "the block itself is committed, as upstream")
}

func TestHandler_HasUserPacker(t *testing.T) {
	h, userRepo := newHandler(t)
	assert.False(t, h.HasUserPacker())
	h.SetUserPacker(userpack.New(userpack.Lookups{Profiles: userRepo}, nil))
	assert.True(t, h.HasUserPacker())
}
