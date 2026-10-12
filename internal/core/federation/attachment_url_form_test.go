package federation_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 画面でプロキシに包めない形の URL を持つ添付は取り込まない。サムネイルだけが
// 崩れているときは、添付は残してサムネイルだけ捨てる。
func TestExtractAttachments_DropsMalformedURLs(t *testing.T) {
	docs := federation.ExtractAttachments([]any{
		map[string]any{"type": "Image", "url": "/files/a.png"},
		map[string]any{"type": "Image", "url": "https:///remote.example/b.png"},
		map[string]any{"type": "Image", "url": "https:\\\\remote.example/c.png"},
		map[string]any{"type": "Image", "url": "https://remote.example/ok.png",
			"icon": map[string]any{"type": "Image", "url": "//remote.example/t.png"}},
		map[string]any{"type": "Video", "url": "https://remote.example/v.mp4",
			"icon": map[string]any{"type": "Image", "url": "https://remote.example/t (1).png"}},
	}, false)
	require.Len(t, docs, 2, "崩れた形の url の添付を取り込んでいる")
	assert.Equal(t, "https://remote.example/ok.png", docs[0].URL)
	assert.Nil(t, docs[0].Icon, "崩れた形のサムネイルを残している")
	require.NotNil(t, docs[1].Icon)
	assert.Equal(t, "https://remote.example/t%20%281%29.png", docs[1].Icon.URL.String(), "サムネイルを符号化していない")
}

func TestUpsertAttachments_URLForm(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	newResolver := func(drive *testutil.MockDriveFileRepository) *federation.Resolver {
		r := federation.NewResolver(testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(),
			activitypub.NewURLBuilder("https://example.com"), &stubFetcher{}, idGen)
		r.SetDriveFileRepo(drive)
		return r
	}
	userID, host := "ru", "remote.example"

	t.Run("malformed url drops the attachment", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{
			{URL: "https:///remote.example/a.png", MediaType: "image/png"},
			{URL: "/a.png", MediaType: "image/png"},
		}, &userID, &host)
		assert.Empty(t, ids)
		assert.Empty(t, drive.Files)
	})

	// 表示用の url は符号化した値、dedup の鍵の uri は受け取った値のまま。
	t.Run("url is stored encoded and uri as received", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		raw := "https://remote.example/media/photo (1).png"
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{{
			URL: raw, MediaType: "image/png",
			Icon: &activitypub.Image{URL: activitypub.APLenientHref("https://remote.example/t'1.png")},
		}}, &userID, &host)
		require.Len(t, ids, 1)
		f := drive.Files[ids[0]]
		assert.Equal(t, "https://remote.example/media/photo%20%281%29.png", f.URL)
		require.NotNil(t, f.URI)
		assert.Equal(t, raw, *f.URI)
		require.NotNil(t, f.ThumbnailURL)
		assert.Equal(t, "https://remote.example/t%271.png", *f.ThumbnailURL)
	})

	t.Run("malformed thumbnail is dropped and the attachment kept", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{{
			URL: "https://remote.example/a.png", MediaType: "image/png",
			Icon: &activitypub.Image{URL: activitypub.APLenientHref("/t.png")},
		}}, &userID, &host)
		require.Len(t, ids, 1)
		assert.Nil(t, drive.Files[ids[0]].ThumbnailURL)
	})

	// 符号化で drive_file.url (1024) を超えたら、長すぎる値と同じく添付ごと諦める。
	t.Run("url too long after encoding drops the attachment", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		prefix := "https://remote.example/"
		raw := prefix + strings.Repeat("a", 1024-len(prefix)-1) + " "
		require.Len(t, []rune(raw), 1024, "符号化する前は上限ちょうど")
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{{URL: raw, MediaType: "image/png"}}, &userID, &host)
		assert.Empty(t, ids)
	})

	// 符号化で thumbnailUrl (512) を超えたら、サムネイルだけ捨てる。
	t.Run("thumbnail too long after encoding is dropped", func(t *testing.T) {
		drive := testutil.NewMockDriveFileRepository()
		prefix := "https://remote.example/"
		thumb := prefix + strings.Repeat("a", 512-len(prefix)-1) + " "
		require.Len(t, []rune(thumb), 512)
		ids := newResolver(drive).UpsertAttachments([]activitypub.Document{{
			URL: "https://remote.example/a.png", MediaType: "image/png",
			Icon: &activitypub.Image{URL: activitypub.APLenientHref(thumb)},
		}}, &userID, &host)
		require.Len(t, ids, 1)
		assert.Nil(t, drive.Files[ids[0]].ThumbnailURL)
	})
}
