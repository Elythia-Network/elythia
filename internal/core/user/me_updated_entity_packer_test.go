package user_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// meUpdated の本体は、構築子で渡された Packer で包む (#3792、#3825)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestService_MeUpdatedPacksWithTheGivenEntityPacker(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	avatar := "https://remote.example/avatar.png"
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AvatarURL: &avatar}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)
	svc.SetEntityPackerForTest(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)}))

	_, err := svc.UpdateProfile("u1", user.UpdateInput{Name: ptr(ptr("Alice"))})
	require.NoError(t, err)

	require.Len(t, pub.calls, 1)
	raw, err := json.Marshal(pub.calls[0].body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	got, _ := m["avatarUrl"].(string)
	assert.Contains(t, got, "https://local.example/proxy/avatar.webp?", "avatarUrl: %s", got)
}
