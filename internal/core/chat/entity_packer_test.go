package chat

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// NewService は受け取った依存を service に持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewServiceKeepsDependencies(t *testing.T) {
	packer := entity.NewPacker(entity.PackerDeps{})
	lites := &ctorLiteFiller{}
	blocks := testutil.NewMockBlockingRepository()
	s := NewService(nil, nil, packer, lites, blocks)
	if s.entityPacker != packer || s.lites != lites || s.blockingRepo != blocks {
		t.Errorf("NewService dropped a dependency: %+v", s)
	}
}

type ctorLiteFiller struct{}

func (*ctorLiteFiller) FillLites([]*model.User, []*entity.UserLite) {}

// stream と push に載せる fromUser・toUser・reaction の user は、構築子で
// 渡された Packer で包む (#3794)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestService_PackUsersStreamUsesTheGivenPacker(t *testing.T) {
	s := &Service{entityPacker: entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)})}
	avatar := "https://remote.example/avatar.png"
	out := s.packUsersStream(&model.User{ID: "alice", Username: "alice", AvatarURL: &avatar}, nil)
	if len(out) != 2 || out[0] == nil || out[1] != nil {
		t.Fatalf("unexpected result: %+v", out)
	}
	if !strings.HasPrefix(out[0].AvatarURL, "https://local.example/proxy/avatar.webp?") {
		t.Errorf("avatarUrl must be proxied by the given Packer: %q", out[0].AvatarURL)
	}
}
