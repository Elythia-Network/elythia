package roles_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/api/roles"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedHiddenMembers puts a public role "r1" with five members: a normal local
// user, a suspended local user, a deleted local user, a deleted remote user and
// a suspended remote user. It also makes "mod" a moderator through "modrole".
func seedHiddenMembers(t *testing.T) (*roles.Handler, *testutil.MockRoleAssignmentRepository) {
	t.Helper()
	h, roleRepo, assignRepo, userRepo := newUsersTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", Name: "Public", IsPublic: true, IsExplorable: true, Target: model.RoleTargetManual}
	roleRepo.Roles["modrole"] = &model.Role{ID: "modrole", Name: "Mod", IsModerator: true, Target: model.RoleTargetManual}
	remote := "remote.example"
	userRepo.Users["alive"] = &model.User{ID: "alive", Username: "alive"}
	userRepo.Users["suspended"] = &model.User{ID: "suspended", Username: "suspended", IsSuspended: true}
	userRepo.Users["deleted"] = &model.User{ID: "deleted", Username: "deleted", IsDeleted: true}
	userRepo.Users["rdeleted"] = &model.User{ID: "rdeleted", Username: "rdeleted", Host: &remote, IsDeleted: true}
	userRepo.Users["rsuspended"] = &model.User{ID: "rsuspended", Username: "rsuspended", Host: &remote, IsSuspended: true}
	userRepo.Users["mod"] = &model.User{ID: "mod", Username: "mod"}
	for i, uid := range []string{"alive", "suspended", "deleted", "rdeleted", "rsuspended"} {
		require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra" + string(rune('1'+i)), RoleID: "r1", UserID: uid}))
	}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "rm", RoleID: "modrole", UserID: "mod"}))
	return h, assignRepo
}

func memberIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(body, &resp))
	ids := make([]string, 0, len(resp))
	for _, e := range resp {
		ids = append(ids, e["user"].(map[string]any)["id"].(string))
	}
	return ids
}

func usersCount(t *testing.T, body []byte) float64 {
	t.Helper()
	var r map[string]any
	require.NoError(t, json.Unmarshal(body, &r))
	return r["usersCount"].(float64)
}

// #3999: モデレーターでない人には、凍結中とローカルの削除済みのメンバーを
// 見せない。リモートの削除済みは users/show と同じく出す。
func TestUsers_HidesSuspendedAndDeletedLocalMembers(t *testing.T) {
	h, _ := seedHiddenMembers(t)

	rec := doPost(h.Users, `{"roleId":"r1"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.ElementsMatch(t, []string{"alive", "rdeleted"}, memberIDs(t, rec.Body.Bytes()), "anonymous")

	vc := newCtxWithViewer(`{"roleId":"r1"}`, "viewer1")
	require.NoError(t, h.Users(vc.ctx))
	require.Equal(t, http.StatusOK, vc.rec.Code)
	assert.ElementsMatch(t, []string{"alive", "rdeleted"}, memberIDs(t, vc.rec.Body.Bytes()), "non-moderator")
}

// #3999: モデレーターには全員を見せる (users/show と同じ)。
func TestUsers_ModeratorSeesAllMembers(t *testing.T) {
	h, _ := seedHiddenMembers(t)

	vc := newCtxWithViewer(`{"roleId":"r1"}`, "mod")
	require.NoError(t, h.Users(vc.ctx))
	require.Equal(t, http.StatusOK, vc.rec.Code)
	assert.ElementsMatch(t, []string{"alive", "suspended", "deleted", "rdeleted", "rsuspended"}, memberIDs(t, vc.rec.Body.Bytes()))
}

// #3999: usersCount は、見る人に roles/users が返す人数と一致させる。
func TestShowAndList_UsersCountMatchesVisibleMembers(t *testing.T) {
	h, _ := seedHiddenMembers(t)

	for _, tc := range []struct {
		name   string
		viewer string
		want   float64
	}{
		{name: "anonymous", want: 2},
		{name: "non-moderator", viewer: "viewer1", want: 2},
		{name: "moderator", viewer: "mod", want: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := func(handler func(echo.Context) error, body string) []byte {
				if tc.viewer == "" {
					rec := doPost(handler, body)
					require.Equal(t, http.StatusOK, rec.Code)
					return rec.Body.Bytes()
				}
				vc := newCtxWithViewer(body, tc.viewer)
				require.NoError(t, handler(vc.ctx))
				require.Equal(t, http.StatusOK, vc.rec.Code)
				return vc.rec.Body.Bytes()
			}

			assert.Equal(t, tc.want, usersCount(t, call(h.Show, `{"roleId":"r1"}`)), "roles/show")

			var list []json.RawMessage
			require.NoError(t, json.Unmarshal(call(h.List, `{}`), &list))
			require.Len(t, list, 1)
			assert.Equal(t, tc.want, usersCount(t, list[0]), "roles/list")
		})
	}
}
