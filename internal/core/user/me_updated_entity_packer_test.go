package user_test

import (
	"encoding/json"
	"strings"
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
	svc.SetEntityPackerForTest(entity.NewPacker(entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true), entity.PackerDeps{}))

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

// meUpdated の movedTo / alsoKnownAs は、/api/i と同じく利用者の ID に解決して送る (#4059)。
// frontend はこの event で `$i` を上書きするので、URI のままだと /api/i と食い違う。
func TestService_MeUpdatedResolvesMoveTargets(t *testing.T) {
	svc, repo, _, _ := newFullSvc(t)
	aka := "https://local.example/users/alias1,https://remote.example/users/rem1"
	movedTo := "https://remote.example/users/rem1"
	repo.Users["u1"] = &model.User{ID: "u1", Username: "alice", AlsoKnownAs: &aka, MovedToURI: &movedTo}
	repo.Profiles["u1"] = &model.UserProfile{UserID: "u1"}
	repo.Users["alias1"] = &model.User{ID: "alias1", Username: "aliasuser"}
	remoteURI := "https://remote.example/users/rem1"
	remoteHost := "remote.example"
	repo.Users["rem1"] = &model.User{ID: "rem1", Username: "remoteu", Host: &remoteHost, URI: &remoteURI}
	pub := &stubMainStreamPublisher{}
	svc.SetMainStreamPublisher(pub)
	svc.SetMoveTargetResolver(user.NewMoveTargetResolver(repo, func(uri string) string {
		if id, ok := strings.CutPrefix(uri, "https://local.example/users/"); ok {
			return id
		}
		return ""
	}))

	_, err := svc.UpdateProfile("u1", user.UpdateInput{Name: ptr(ptr("Alice"))})
	require.NoError(t, err)

	require.Len(t, pub.calls, 1)
	raw, err := json.Marshal(pub.calls[0].body)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Equal(t, []any{"alias1", "rem1"}, m["alsoKnownAs"])
	assert.Equal(t, "rem1", m["movedTo"])
}

// 解決できない URI (ローカルに無い利用者、取り込んでいないリモート) は落とす。リモートを取りには行かない。
func TestNewMoveTargetResolver_UnknownURIs(t *testing.T) {
	_, repo, _, _ := newFullSvc(t)
	resolve := user.NewMoveTargetResolver(repo, func(uri string) string {
		if id, ok := strings.CutPrefix(uri, "https://local.example/users/"); ok {
			return id
		}
		return ""
	})
	_, ok := resolve("https://local.example/users/nobody")
	assert.False(t, ok)
	_, ok = resolve("https://remote.example/users/unknown")
	assert.False(t, ok)
	_, ok = user.NewMoveTargetResolver(nil, nil)("https://remote.example/users/unknown")
	assert.False(t, ok)
}
