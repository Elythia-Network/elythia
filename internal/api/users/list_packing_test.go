package users

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

// perPairFollowing counts the per-pair lookups Apply makes, so a test can
// assert that a list path reads the relations in one batch instead.
type perPairFollowing struct {
	*testutil.MockFollowingRepository
	perPair atomic.Int64
}

func (f *perPairFollowing) Exists(a, b string) (bool, error) {
	f.perPair.Add(1)
	return f.MockFollowingRepository.Exists(a, b)
}

func (f *perPairFollowing) FindByPair(a, b string) (*model.Following, error) {
	f.perPair.Add(1)
	return f.MockFollowingRepository.FindByPair(a, b)
}

// ListFollowingRowsFromAnchor is the batch reader the production repository
// has (repository.FollowingRowsFromAnchorReader); it is not counted.
func (f *perPairFollowing) ListFollowingRowsFromAnchor(anchorID string, candidateIDs []string) ([]*model.Following, error) {
	var rows []*model.Following
	for _, id := range candidateIDs {
		if row, err := f.MockFollowingRepository.FindByPair(anchorID, id); err == nil && row != nil {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// 本家の explore (users)・pinned-users・users/recommendation・users/search・
// search-by-username-and-host・users/show (userIds) は packMany(users, me,
// {schema: 'UserDetailed'}) で組む。モデレーターの閲覧者には moderationNote と
// 2FA の項目が付き、非公開のカウントも見える。profile と関係は一覧ぶんを
// まとめて引き、利用者ごとに引かない (#3330)。
func TestListPaths_PackLikeUpstreamPackMany(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		call func(h *Handler) func(echo.Context) error
		meta bool
	}{
		{"explore", `{}`, func(h *Handler) func(echo.Context) error { return h.List }, false},
		{"pinned-users", `{}`, func(h *Handler) func(echo.Context) error { return h.PinnedUsers }, true},
		{"recommendation", `{}`, func(h *Handler) func(echo.Context) error { return h.UserRecommendation }, false},
		{"search", `{"query":"target person"}`, func(h *Handler) func(echo.Context) error { return h.Search }, false},
		{"search-by-username-and-host", `{"username":"tgtuser"}`, func(h *Handler) func(echo.Context) error { return h.SearchByUsernameAndHost }, false},
		{"show-userIds", `{"userIds":["tgt"]}`, func(h *Handler) func(echo.Context) error { return h.Show }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newTestHandler(t)
			h.SetUserRepo(repo)
			h.SetModeratorChecker(modByID{id: "mod"})
			following := &perPairFollowing{MockFollowingRepository: testutil.NewMockFollowingRepository()}
			h.SetFollowingRepo(following)
			name := "Target Person"
			updated := time.Now()
			repo.Users["tgt"] = &model.User{
				ID: "tgt", Username: "tgtuser", UsernameLower: "tgtuser", Name: &name,
				IsExplorable: true, UpdatedAt: &updated, FollowersCount: 7, FollowingCount: 3,
				AvatarDecorations: datatypes.JSON([]byte("[]")),
			}
			note := "mod note"
			repo.Profiles["tgt"] = &model.UserProfile{
				UserID: "tgt", ModerationNote: &note, TwoFactorEnabled: true,
				FollowersVisibility: model.FollowingVisibilityPrivate,
				FollowingVisibility: model.FollowingVisibilityPrivate,
			}
			// 利用者ごとの profile の取得を失敗させる。まとめて引いていれば影響しない。
			repo.FindProfileErr = errors.New("per-row profile lookup must not be used")
			if tc.meta {
				meta := testutil.NewMockMetaRepository()
				meta.Meta = &model.Meta{PinnedUsers: model.StringArray{"@tgtuser"}}
				h.SetMetaRepo(meta)
			}

			row := func(viewerID string) map[string]any {
				t.Helper()
				rec := postStub(tc.call(h), tc.body, &model.User{ID: viewerID})
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var rows []map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
				require.Len(t, rows, 1)
				return rows[0]
			}

			mod := row("mod")
			assert.Equal(t, "mod note", mod["moderationNote"])
			assert.Equal(t, true, mod["twoFactorEnabled"])
			assert.Contains(t, mod, "securityKeys")
			assert.Equal(t, float64(7), mod["followersCount"])
			assert.Equal(t, float64(3), mod["followingCount"])
			assert.Equal(t, false, mod["isFollowing"])

			other := row("viewer2")
			assert.NotContains(t, other, "moderationNote")
			assert.NotContains(t, other, "twoFactorEnabled")
			assert.Equal(t, float64(0), other["followersCount"])
			assert.Equal(t, float64(0), other["followingCount"])

			assert.Zero(t, following.perPair.Load(), "関係は利用者ごとに引かない")
		})
	}
}

// pinned-users に自分が含まれるときは、本家の pack と同じく自分の行を MeDetailed で返す。
func TestPinnedUsers_SelfIsMeDetailed(t *testing.T) {
	h, repo := newTestHandler(t)
	h.SetUserRepo(repo)
	addExplorable(repo, "u1", "alice", nil)
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{PinnedUsers: model.StringArray{"@alice"}}
	h.SetMetaRepo(meta)

	rec := postStub(h.PinnedUsers, `{}`, repo.Users["u1"])
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Contains(t, rows[0], "twoFactorBackupCodesStock", "自分の行は MeDetailed")
}

// profile を読めない利用者 (行が無い・読み込みに失敗した) は公開範囲が分からない
// ので、本人とモデレーター以外にはカウントを伏せる (#3330)。
func TestListPaths_ProfileMissingClosesCounts(t *testing.T) {
	h, repo := newTestHandler(t)
	h.SetUserRepo(repo)
	h.SetModeratorChecker(modByID{id: "mod"})
	u := addExplorable(repo, "u1", "alice", nil)
	u.FollowersCount, u.FollowingCount = 5, 6

	for _, tc := range []struct {
		viewer *model.User
		want   float64
	}{
		{nil, 0},
		{&model.User{ID: "viewer2"}, 0},
		{&model.User{ID: "mod"}, 5},
		{u, 5},
	} {
		rec := postStub(h.List, `{}`, tc.viewer)
		require.Equal(t, http.StatusOK, rec.Code)
		var rows []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
		require.Len(t, rows, 1)
		assert.Equal(t, tc.want, rows[0]["followersCount"], "viewer=%v", tc.viewer)
	}
}
