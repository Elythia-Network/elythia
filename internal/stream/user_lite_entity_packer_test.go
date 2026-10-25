package stream

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/bubbleversus"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// NewBubbleVersusPublisher は受け取った依存を持たせる (#3823)。依存を構築子の必須の
// 引数にしたので、起動時の検査 (criticalWiring) は入れ忘れを見なくなった。
// 構築子が引数を捨てても build は通るので、ここで固定する。
func TestNewBubbleVersusPublisherKeepsDependencies(t *testing.T) {
	pk := entity.NewPacker(entity.PackerDeps{})
	assert.Same(t, pk, NewBubbleVersusPublisher(nil, pk).entityPacker)
}

// 招待の `invited` に載せる招待者 (UserLite) は、渡された Packer で包む (#3794)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestUserLitePublishers_PackInviterWithTheGivenPacker(t *testing.T) {
	pk := entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)})
	avatar := "https://remote.example/avatar.png"
	inviter := &model.User{ID: "alice", Username: "alice", AvatarURL: &avatar}
	proxied := func(t *testing.T, payload any) {
		t.Helper()
		_, body := decodeEnvelope(t, payload)
		user, _ := body["user"].(map[string]any)
		got, _ := user["avatarUrl"].(string)
		assert.True(t, strings.HasPrefix(got, "https://local.example/proxy/avatar.webp?"), "avatarUrl: %s", got)
	}

	t.Run("reversi", func(t *testing.T) {
		bus := &capturingPubSub{}
		p := NewReversiGamePublisher(bus, pk)
		p.PublishInvited("bob", inviter)
		require.Len(t, bus.calls, 1)
		proxied(t, bus.calls[0].payload)
	})
	t.Run("bubbleVersus", func(t *testing.T) {
		bus := &capturingPubSub{}
		p := NewBubbleVersusPublisher(bus, pk)
		p.PublishInvited("bob", inviter, &bubbleversus.Match{ID: "m1", GameMode: "square"})
		require.Len(t, bus.calls, 1)
		proxied(t, bus.calls[0].payload)
	})
}
