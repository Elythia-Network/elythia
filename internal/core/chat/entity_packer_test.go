package chat

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

type entityPackerHolder interface {
	SetEntityPacker(*entity.Packer)
	HasEntityPacker() bool
}

// HasEntityPacker は mediaURL を持つ Packer を受け取ったときだけ true を返す
// (#3794)。criticalWiring がこれで入れ忘れを見る。
func TestHasEntityPacker(t *testing.T) {
	for name, h := range map[string]entityPackerHolder{"Service": &Service{}} {
		t.Run(name, func(t *testing.T) {
			if h.HasEntityPacker() {
				t.Error("an unwired component must report false")
			}
			h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{}))
			if h.HasEntityPacker() {
				t.Error("a Packer without mediaURL must report false")
			}
			h.SetEntityPacker(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
				"https://local.example", "", []byte("secret"), false, true)}))
			if !h.HasEntityPacker() {
				t.Error("a Packer with mediaURL must report true")
			}
		})
	}
}

// stream と push に載せる fromUser・toUser・reaction の user は、SetEntityPacker で
// 渡された Packer で包む (#3794)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestService_PackUsersStreamUsesTheGivenPacker(t *testing.T) {
	s := &Service{}
	s.SetEntityPacker(entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)}))
	avatar := "https://remote.example/avatar.png"
	out := s.packUsersStream(&model.User{ID: "alice", Username: "alice", AvatarURL: &avatar}, nil)
	if len(out) != 2 || out[0] == nil || out[1] != nil {
		t.Fatalf("unexpected result: %+v", out)
	}
	if !strings.HasPrefix(out[0].AvatarURL, "https://local.example/proxy/avatar.webp?") {
		t.Errorf("avatarUrl must be proxied by the given Packer: %q", out[0].AvatarURL)
	}
}
