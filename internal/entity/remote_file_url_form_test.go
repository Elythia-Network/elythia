package entity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

// 取り込みで形を確かめる前に保存されたリモートの添付の URL は、崩れた形なら
// 返さない。プロキシで包めずに、そのまま img の src へ出てしまうため。
func TestRemoteFileURLForm(t *testing.T) {
	malformed := []string{"/files/a.png", "https:///" + remoteHost + "/a.png", "https:\\\\" + remoteHost + "/a.png"}
	for _, c := range []*MediaURLContext{nil, internalCtx(), disabledCtx()} {
		for _, raw := range malformed {
			f := &model.DriveFile{Type: "image/png", URL: raw, UserHost: sp(remoteHost), IsLink: true,
				ThumbnailURL: sp(raw), WebpublicURL: sp(raw)}
			assert.Empty(t, c.GetPublicURL(f, modeDefault), "public url: %q", raw)
			assert.Empty(t, c.GetSelfURL(f, modeDefault), "self url: %q", raw)
			assert.Nil(t, c.GetThumbnailURL(f), "thumbnail: %q", raw)
			assert.Nil(t, c.GetWebpublicURL(f), "webpublic: %q", raw)

			v := &model.DriveFile{Type: "video/mp4", URL: raw, UserHost: sp(remoteHost), IsLink: true, ThumbnailURL: sp(raw)}
			assert.Nil(t, c.GetThumbnailURL(v), "video thumbnail: %q", raw)
		}
	}
}

// サムネイルだけが崩れているときは、本体の URL からサムネイルを作る。
func TestRemoteFileMalformedThumbnailFallsBackToURL(t *testing.T) {
	f := &model.DriveFile{Type: "image/png", URL: "https://" + remoteHost + "/a.png", UserHost: sp(remoteHost), IsLink: true,
		ThumbnailURL: sp("/t.png")}
	got := internalCtx().GetThumbnailURL(f)
	require.NotNil(t, got)
	assert.True(t, strings.HasPrefix(*got, testInternalProxy+"/static.webp?"), *got)
	assert.Equal(t, "https://"+remoteHost+"/a.png", u(t, *got).Query().Get("url"))
}

// 括弧や空白を含むリモートの URL は、符号化してから返す。
func TestRemoteFileURLIsEncoded(t *testing.T) {
	f := &model.DriveFile{Type: "application/pdf", URL: "https://" + remoteHost + "/a (1).pdf", UserHost: sp(remoteHost), IsLink: true}
	assert.Equal(t, "https://"+remoteHost+"/a%20%281%29.pdf", internalCtx().GetPublicURL(f, modeDefault))
	var nilCtx *MediaURLContext
	assert.Equal(t, "https://"+remoteHost+"/a%20%281%29.pdf", nilCtx.GetSelfURL(f, modeDefault))
}

// ローカルのファイルは形の判定に掛けない (相対パスのまま返す)。
func TestLocalFileURLIsNotChecked(t *testing.T) {
	f := &model.DriveFile{Type: "image/png", URL: "/files/a.png", ThumbnailURL: sp("/files/t.png"), WebpublicURL: sp("/files/w.png")}
	c := internalCtx()
	assert.Equal(t, "/files/w.png", c.GetPublicURL(f, modeDefault))
	assert.Equal(t, "/files/a.png", c.GetSelfURL(f, modeDefault))
	require.NotNil(t, c.GetThumbnailURL(f))
	assert.Equal(t, "/files/t.png", *c.GetThumbnailURL(f))
	require.NotNil(t, c.GetWebpublicURL(f))
	assert.Equal(t, "/files/w.png", *c.GetWebpublicURL(f))
}
