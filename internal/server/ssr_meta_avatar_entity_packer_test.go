package server

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// OGP のアバターは、SetEntityPacker で渡された Packer で media proxy へ通す
// (#3798)。
// 渡した Packer を使わずに nil の Packer で包むと生のリモートの URL
// になって落ちる。
func TestSSRMetaHandler_AvatarURLUsesTheGivenPacker(t *testing.T) {
	h := &ssrMetaHandler{cfg: &config.Config{URL: "https://local.example"}}
	h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)}))
	host := "remote.example"
	avatar := "https://remote.example/avatar.png"
	got := h.avatarURL(&model.User{ID: "alice", Username: "alice", Host: &host, AvatarURL: &avatar})
	if !strings.HasPrefix(got, "https://local.example/proxy/avatar.webp?") {
		t.Errorf("avatarUrl must be proxied by the given Packer: %q", got)
	}
}
