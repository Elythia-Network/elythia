package chat

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// push に載せる送り主のアイコンは、構築子で渡された Packer で media
// proxy へ通す (#3798)。
// 渡した Packer を使わずに nil の Packer で包むと生のリモートの URL
// になって落ちる。
func TestService_PackMessagePushUsesTheGivenPacker(t *testing.T) {
	s := &Service{entityPacker: entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)})}
	host := "remote.example"
	avatar := "https://remote.example/avatar.png"
	out := s.packMessagePush(map[string]any{}, &model.User{ID: "alice", Username: "alice", Host: &host, AvatarURL: &avatar}, nil)
	from, _ := out["fromUser"].(map[string]any)
	got, _ := from["avatarUrl"].(string)
	if !strings.HasPrefix(got, "https://local.example/proxy/avatar.webp?") {
		t.Errorf("avatarUrl must be proxied by the given Packer: %q", got)
	}
}
