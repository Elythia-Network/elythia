package channels

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// チャンネルのバナーは、引数で渡された Packer で media proxy へ通す (#3798)。
// 渡した Packer を使わずに nil の Packer で包むと生のリモートの URL
// になって落ちる。
func TestChannelBannerPublicURL_UsesTheGivenPacker(t *testing.T) {
	pk := entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)})
	got := channelBannerPublicURL(pk, &model.DriveFile{URL: "https://remote.example/banner.png"})
	if !strings.HasPrefix(got, "https://local.example/proxy/image.webp?") {
		t.Errorf("banner must be proxied by the given Packer: %q", got)
	}
}

// handler はバナーを自分の Packer で包む (#3798)。1 件ずつ引く経路と、一覧でまとめて
// 引く経路の両方を見る。
func TestHandler_BannerURLUsesItsPacker(t *testing.T) {
	h, _, _, _ := newHandler(t)
	h.entityPacker = entity.NewPacker(entity.PackerDeps{MediaURL: entity.NewMediaURLContext(
		"https://local.example", "https://local.example/proxy", []byte("secret"), false, true)})
	h.SetDriveFileRepo(&stubBannerResolver{files: map[string]*model.DriveFile{
		"b1": {ID: "b1", URL: "https://remote.example/banner.png"},
	}})
	proxied := func(t *testing.T, got any) {
		t.Helper()
		s, _ := got.(string)
		if !strings.HasPrefix(s, "https://local.example/proxy/image.webp?") {
			t.Errorf("banner must be proxied by the handler's Packer: %v", got)
		}
	}
	bid := "b1"
	proxied(t, h.resolveBannerURL(&bid))
	proxied(t, h.resolveBannerURLs([]*model.Channel{{ID: "c1", BannerID: &bid}})["c1"])
}
