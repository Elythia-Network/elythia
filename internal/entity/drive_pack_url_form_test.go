package entity

import (
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
)

// PackDriveFile は形の判定を 1 回にまとめているが、出力は公開の getter を
// 個別に呼んだときと同じになる。リモートの正しい形・崩れた形・ローカルの
// ファイルを、どのプロキシ設定でも突き合わせる。
func TestPackDriveFile_URLsMatchGetters(t *testing.T) {
	t.Cleanup(func() { SetMediaURLContext(nil) })
	idGen := newTestIDGen(t)
	id := idGen.Generate(time.Now())
	files := map[string]*model.DriveFile{
		"remote well-formed": {ID: id, Type: "image/png", URL: "https://" + remoteHost + "/a (1).png",
			URI: sp("https://" + remoteHost + "/a (1).png"), ThumbnailURL: sp("https://" + remoteHost + "/t.png"),
			WebpublicURL: sp("https://" + remoteHost + "/w.png"), UserHost: sp(remoteHost), IsLink: true},
		"remote malformed": {ID: id, Type: "image/png", URL: "https:///" + remoteHost + "/a.png",
			URI: sp("https:///" + remoteHost + "/a.png"), ThumbnailURL: sp("/t.png"),
			WebpublicURL: sp("/w.png"), UserHost: sp(remoteHost), IsLink: true},
		"remote video malformed thumbnail": {ID: id, Type: "video/mp4", URL: "https://" + remoteHost + "/v.mp4",
			ThumbnailURL: sp("https:///t.png"), UserHost: sp(remoteHost), IsLink: true},
		"local": {ID: id, Type: "image/png", URL: "/files/a.png", ThumbnailURL: sp("/files/t.png"), WebpublicURL: sp("/files/w.png")},
	}
	for ctxName, c := range map[string]*MediaURLContext{"nil": nil, "internal": internalCtx(), "external": externalCtx(), "disabled": disabledCtx()} {
		SetMediaURLContext(c)
		for name, f := range files {
			got := PackDriveFile(f, idGen)
			assert.Equal(t, c.GetPublicURL(f, modeDefault), got.URL, "%s/%s url", ctxName, name)
			assert.Equal(t, c.GetThumbnailURL(f), got.ThumbnailURL, "%s/%s thumbnailUrl", ctxName, name)
			assert.Equal(t, c.GetWebpublicURL(f), got.WebpublicURL, "%s/%s webpublicUrl", ctxName, name)
			self := PackDriveFileSelf(f, idGen)
			assert.Equal(t, c.GetSelfURL(f, modeDefault), self.URL, "%s/%s self url", ctxName, name)
		}
	}
}

// PackDriveFile も、崩れた形のリモートの URL を返さない。
func TestPackDriveFile_MalformedRemoteURLs(t *testing.T) {
	SetMediaURLContext(internalCtx())
	t.Cleanup(func() { SetMediaURLContext(nil) })
	idGen := newTestIDGen(t)
	f := &model.DriveFile{ID: idGen.Generate(time.Now()), Type: "image/png", URL: "/a.png",
		ThumbnailURL: sp("/t.png"), WebpublicURL: sp("/w.png"), UserHost: sp(remoteHost), IsLink: true}
	got := PackDriveFile(f, idGen)
	assert.Empty(t, got.URL)
	assert.Nil(t, got.ThumbnailURL)
	assert.Nil(t, got.WebpublicURL)
	assert.Empty(t, PackDriveFileSelf(f, idGen).URL)
}
