package userpack_test

import (
	"context"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const entityPackerProxy = "https://local.example/proxy"

func proxyingEntityPacker() *entity.Packer {
	return entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", entityPackerProxy, []byte("secret"), false, true)})
}

// HasEntityPacker は mediaURL を持つ Packer を受け取ったときだけ true を返す
// (#3792)。criticalWiring がこれで入れ忘れを見る。
func TestPacker_HasEntityPacker(t *testing.T) {
	assert.False(t, userpack.New(nil, userpack.Lookups{}, nil).HasEntityPacker())
	assert.False(t, userpack.New(entity.NewPacker(entity.PackerDeps{}), userpack.Lookups{}, nil).HasEntityPacker())
	assert.True(t, userpack.New(proxyingEntityPacker(), userpack.Lookups{}, nil).HasEntityPacker())
}

// Lite・DetailedNotMe・DetailedMany は、New で渡された Packer で包む (#3792)。
// 渡した Packer を使わずに nil の Packer で包むと avatarUrl が生のリモートの
// URL になって落ちる。
func TestPacker_PacksWithTheGivenEntityPacker(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	u := target()
	avatar := "https://" + remoteHost + "/avatar.png"
	u.AvatarURL = &avatar
	profile := &model.UserProfile{UserID: u.ID}
	p := userpack.New(proxyingEntityPacker(), userpack.Lookups{
		Profiles: fakeProfiles{profiles: map[string]*model.UserProfile{u.ID: profile}},
	}, idGen)

	proxied := func(t *testing.T, got string) {
		t.Helper()
		assert.True(t, strings.HasPrefix(got, entityPackerProxy+"/avatar.webp?"), "avatarUrl: %s", got)
	}
	t.Run("Lite", func(t *testing.T) {
		proxied(t, p.Lite(u).AvatarURL)
	})
	t.Run("DetailedNotMe", func(t *testing.T) {
		d, ok := p.DetailedNotMe(context.Background(), u, viewer())
		require.True(t, ok)
		proxied(t, d.AvatarURL)
	})
	t.Run("DetailedMany", func(t *testing.T) {
		out := p.DetailedMany(context.Background(), viewer(), []*model.User{u}, map[string]*model.UserProfile{u.ID: profile})
		require.Len(t, out, 1)
		proxied(t, out[0].AvatarURL)
	})
}
