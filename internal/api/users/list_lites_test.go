package users

import (
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

const liteRemoteHost = "remote.example"

// remoteAlice makes alice a remote user carrying a custom emoji, and wires the
// instance / emoji repositories that resolve them.
func remoteAlice(t *testing.T, h *Handler, userRepo *testutil.MockUserRepository) *model.User {
	t.Helper()
	a := pinUser(userRepo, "alice")
	host := liteRemoteHost
	a.Host = &host
	a.Emojis = model.StringArray{"blobcat"}
	name := "alice"
	a.Name = &name
	now := time.Now()
	a.UpdatedAt = &now
	instName := "Remote"
	instances := testutil.NewMockInstanceRepository()
	require.NoError(t, instances.Create(&model.Instance{Host: liteRemoteHost, Name: &instName}))
	h.SetInstanceRepo(instances)
	emojis := testutil.NewMockEmojiRepository()
	require.NoError(t, emojis.Create(&model.Emoji{ID: "e1", Name: "blobcat", Host: &host, PublicURL: "https://remote.example/blobcat.png"}))
	h.SetEmojiRepo(emojis)
	return a
}

// 一覧の利用者 (本家 packMany) はリモートの instance と絵文字を埋める (#3330)。
// 埋めるのは FillDetailedExtrasMany / packLites の 1 か所で、handler ごとの
// 解決は持たない。
func TestListEndpoints_ResolveInstanceAndEmojis(t *testing.T) {
	viewer := &model.User{ID: "viewer", Username: "viewer"}
	cases := []struct {
		name string
		call func(h *Handler) echo.HandlerFunc
		body string
		pick func(t *testing.T, body []byte) map[string]any
	}{
		{"pinned-users", func(h *Handler) echo.HandlerFunc { return h.PinnedUsers }, `{}`, pickFromArray},
		{"users", func(h *Handler) echo.HandlerFunc { return h.List }, `{"origin":"remote"}`, pickFromArray},
		{"users/search", func(h *Handler) echo.HandlerFunc { return h.Search }, `{"query":"alice"}`, pickFromArray},
		{"users/search detail=false", func(h *Handler) echo.HandlerFunc { return h.Search }, `{"query":"alice","detail":false}`, pickFromArray},
		{"users/search-by-username-and-host", func(h *Handler) echo.HandlerFunc { return h.SearchByUsernameAndHost }, `{"username":"alice","host":"remote.example"}`, pickFromArray},
		{"users/search-by-username-and-host detail=false", func(h *Handler) echo.HandlerFunc { return h.SearchByUsernameAndHost }, `{"username":"alice","host":"remote.example","detail":false}`, pickFromArray},
		{"users/show (userIds)", func(h *Handler) echo.HandlerFunc { return h.Show }, `{"userIds":["alice"]}`, pickFromArray},
		{"users/followers", func(h *Handler) echo.HandlerFunc { return h.Followers }, `{"userId":"bob"}`, pickField("follower")},
		{"users/following", func(h *Handler) echo.HandlerFunc { return h.Following }, `{"userId":"bob"}`, pickField("followee")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, userRepo := newTestHandler(t)
			h.SetUserRepo(userRepo)
			a := remoteAlice(t, h, userRepo)
			b := pinUser(userRepo, "bob")
			meta := testutil.NewMockMetaRepository()
			meta.Meta = &model.Meta{PinnedUsers: model.StringArray{"@alice@remote.example"}}
			h.SetMetaRepo(meta)
			fRepo := h.followingRepo.(*testutil.MockFollowingRepository)
			require.NoError(t, fRepo.Create(&model.Following{ID: "f1", FollowerID: a.ID, FolloweeID: b.ID}))
			require.NoError(t, fRepo.Create(&model.Following{ID: "f2", FollowerID: b.ID, FolloweeID: a.ID}))

			rec := postStub(tc.call(h), tc.body, viewer)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			u := tc.pick(t, rec.Body.Bytes())
			require.NotNil(t, u, rec.Body.String())
			inst, _ := u["instance"].(map[string]any)
			require.NotNil(t, inst, "instance を埋める")
			assert.Equal(t, "Remote", inst["name"])
			assert.Equal(t, map[string]any{"blobcat": "https://remote.example/blobcat.png"}, u["emojis"])
		})
	}
}

func TestPackLitesByIDAndReactionUsers(t *testing.T) {
	h, userRepo := newTestHandler(t)
	a := remoteAlice(t, h, userRepo)
	rows := []*model.NoteReaction{{ID: "r1", User: a}, {ID: "r2", User: a}, {ID: "r3"}}
	users := reactionUsers(rows)
	require.Len(t, users, 1, "同じ利用者は 1 回")
	lites := h.packLitesByID(users)
	require.NotNil(t, lites["alice"].Instance)
	assert.Equal(t, "Remote", *lites["alice"].Instance.Name)
	assert.Equal(t, "https://remote.example/blobcat.png", lites["alice"].Emojis["blobcat"])
}
