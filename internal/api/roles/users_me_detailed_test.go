package roles_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/api/userrelation"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家 roles/users は packMany(users, me, {schema: 'UserDetailed'}) で、pack は
// isMe なら MeDetailed を返す。閲覧者自身の行だけ MeDetailed になる (#3330)。
func TestUsers_ViewerRowIsMeDetailed(t *testing.T) {
	h, roleRepo, assignRepo, userRepo := newUsersTestHandler(t)
	roleRepo.Roles["r1"] = &model.Role{ID: "r1", IsPublic: true, IsExplorable: true}
	userRepo.Users["alice"] = &model.User{ID: "alice", Username: "alice"}
	userRepo.Users["viewer1"] = &model.User{ID: "viewer1", Username: "viewer1"}
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra1", RoleID: "r1", UserID: "alice"}))
	require.NoError(t, assignRepo.Create(&model.RoleAssignment{ID: "ra2", RoleID: "r1", UserID: "viewer1"}))
	h.SetListPacker(userpack.New(userpack.Lookups{Relations: userrelation.Repos{}}, nil))
	assert.True(t, h.HasListPacker())

	vc := newCtxWithViewer(`{"roleId":"r1"}`, "viewer1")
	require.NoError(t, h.Users(vc.ctx))
	require.Equal(t, http.StatusOK, vc.rec.Code)
	var resp []map[string]any
	require.NoError(t, json.Unmarshal(vc.rec.Body.Bytes(), &resp))
	require.Len(t, resp, 2)
	for _, row := range resp {
		user := row["user"].(map[string]any)
		// avatarId は MeDetailed にだけある項目。
		_, isMe := user["avatarId"]
		assert.Equal(t, user["id"] == "viewer1", isMe, "閲覧者自身だけ MeDetailed")
	}
}
