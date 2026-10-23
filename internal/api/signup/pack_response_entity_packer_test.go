package signup

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// packSignupResponse は引数で渡された Packer で包む (#3792)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestPackSignupResponse_UsesTheGivenEntityPacker(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	if err != nil {
		t.Fatal(err)
	}
	pk := entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)})
	avatar := "https://remote.example/avatar.png"
	u := &model.User{ID: "9zzzzzzzzz", Username: "alice", AvatarURL: &avatar}
	out := packSignupResponse(pk, u, &model.UserProfile{UserID: u.ID}, "token", idGen, nil)
	got, _ := out["avatarUrl"].(string)
	if !strings.HasPrefix(got, "https://local.example/proxy/avatar.webp?") {
		t.Errorf("avatarUrl must be proxied by the given Packer: %q", got)
	}
}
