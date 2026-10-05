package federation_test

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/core/federation"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newEmojiResolver(t *testing.T) (*federation.Resolver, *testutil.MockEmojiRepository) {
	t.Helper()
	urls := activitypub.NewURLBuilder("https://example.com")
	idGen, _ := id.NewGenerator("aidx")
	r := federation.NewResolver(
		testutil.NewMockUserRepository(), testutil.NewMockNoteRepository(), urls, &stubFetcher{}, idGen)
	emojiRepo := testutil.NewMockEmojiRepository()
	r.SetEmojiRepo(emojiRepo)
	return r, emojiRepo
}

func emojiTag(name, url string) activitypub.EmojiTag {
	return activitypub.EmojiTag{
		Type: "Emoji",
		Name: name,
		Icon: activitypub.Image{Type: "Image", URL: activitypub.APLenientHref(url)},
	}
}

// `emoji.name` は varchar(128) で、同じ値が `note.emojis` / `user.emojis`
// (varchar(128)[]) にも載る。1 箇所の判定で 3 列を守る (#2726)。
func TestUpsertEmojis_DropsOversizedName(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	names := r.UpsertEmojis([]activitypub.EmojiTag{
		emojiTag(":"+strings.Repeat("あ", 129)+":", "https://remote.example/a.png"),
		emojiTag(":ok:", "https://remote.example/ok.png"),
	}, "remote.example")

	assert.Equal(t, []string{"ok"}, []string(names))
	assert.Len(t, emojiRepo.Emojis, 1)
}

// NUL 入りの name は **バッチ取得の前に**落とす。query に渡すと 22021 で
// FindManyByNamesAndHost ごと落ち、その note の絵文字が全部消える。
func TestUpsertEmojis_DropsNameWithNUL(t *testing.T) {
	r, _ := newEmojiResolver(t)
	names := r.UpsertEmojis([]activitypub.EmojiTag{
		emojiTag(":a\x00b:", "https://remote.example/a.png"),
		emojiTag(":ok:", "https://remote.example/ok.png"),
	}, "remote.example")
	assert.Equal(t, []string{"ok"}, []string(names))
}

// icon URL が `originalUrl` (varchar(512) NOT NULL) に収まらない新規 emoji は
// tag ごと落とす。URL 無しの行を作ると壊れた画像になる。
func TestUpsertEmojis_DropsOversizedURLOnCreate(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	long := "https://remote.example/" + strings.Repeat("a", 500)
	require.Greater(t, len([]rune(long)), 512)

	names := r.UpsertEmojis([]activitypub.EmojiTag{emojiTag(":big:", long)}, "remote.example")
	assert.Empty(t, []string(names))
	assert.Empty(t, emojiRepo.Emojis)
}

// 既存 emoji では**古い URL を残す**。行はあるので、壊れた URL で上書きするより
// 古い URL のほうが役に立つ。名前は返す (note.emojis に載る)。
func TestUpsertEmojis_KeepsStoredURLWhenNewOneDoesNotFit(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	host := "remote.example"
	require.NoError(t, emojiRepo.Create(&model.Emoji{
		ID: "e1", Name: "big", Host: &host,
		OriginalURL: "https://remote.example/old.png",
		PublicURL:   "https://remote.example/old.png",
	}))

	long := "https://remote.example/" + strings.Repeat("a", 500)
	names := r.UpsertEmojis([]activitypub.EmojiTag{emojiTag(":big:", long)}, host)
	assert.Equal(t, []string{"big"}, []string(names))

	got, err := emojiRepo.FindByNameAndHost("big", &host)
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/old.png", got.OriginalURL)
}

// `emoji.uri` は URL 系なので**値だけ捨てて行は作る**。dedup / 更新判定に
// 使うだけで、無くても絵文字は表示できる。
func TestUpsertEmojis_DropsOversizedURIButKeepsRow(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	host := "remote.example"
	tag := emojiTag(":u:", "https://remote.example/u.png")
	tag.ID = "https://remote.example/" + strings.Repeat("a", 500)

	names := r.UpsertEmojis([]activitypub.EmojiTag{tag}, host)
	assert.Equal(t, []string{"u"}, []string(names))
	got, err := emojiRepo.FindByNameAndHost("u", &host)
	require.NoError(t, err)
	assert.Nil(t, got.URI)
}

// `emoji.license` は varchar(1024) の本文なので切って NUL を落とす。
func TestUpsertEmojis_TruncatesLicense(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	host := "remote.example"
	long := strings.Repeat("あ", 2000)
	tag := emojiTag(":l:", "https://remote.example/l.png")
	tag.License = &activitypub.MisskeyLicense{FreeText: &long}

	r.UpsertEmojis([]activitypub.EmojiTag{tag}, host)
	got, err := emojiRepo.FindByNameAndHost("l", &host)
	require.NoError(t, err)
	require.NotNil(t, got.License)
	assert.Equal(t, 1024, len([]rune(*got.License)))
}

// license wrapper はあるが freeText が null のケースは nil のまま
// (「明示的に未設定」を空文字に潰さない)。
func TestUpsertEmojis_NullLicenseStaysNil(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	host := "remote.example"
	tag := emojiTag(":n:", "https://remote.example/n.png")
	tag.License = &activitypub.MisskeyLicense{}

	r.UpsertEmojis([]activitypub.EmojiTag{tag}, host)
	got, err := emojiRepo.FindByNameAndHost("n", &host)
	require.NoError(t, err)
	assert.Nil(t, got.License)
}

// 上限ちょうどは通す (境界を締めすぎていないこと)。
func TestUpsertEmojis_MaxLengthNameAndURLAccepted(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	host := "remote.example"
	name := strings.Repeat("あ", 128)
	url := "https://remote.example/" + strings.Repeat("a", 512-23)
	require.Equal(t, 512, len([]rune(url)))

	names := r.UpsertEmojis([]activitypub.EmojiTag{emojiTag(":"+name+":", url)}, host)
	require.Equal(t, []string{name}, []string(names))
	got, err := emojiRepo.FindByNameAndHost(name, &host)
	require.NoError(t, err)
	assert.Equal(t, url, got.OriginalURL)
}
