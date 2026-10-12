package federation_test

import (
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// プロキシで包めない形の icon URL を持つ新規の絵文字は、tag ごと落とす。
func TestUpsertEmojis_DropsMalformedURLOnCreate(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	names := r.UpsertEmojis([]activitypub.EmojiTag{
		emojiTag(":rel:", "/emoji/rel.png"),
		emojiTag(":nohost:", "https:///remote.example/nohost.png"),
		emojiTag(":bs:", "https:\\\\remote.example/bs.png"),
		emojiTag(":ok:", "https://remote.example/ok.png"),
	}, "remote.example")
	assert.Equal(t, []string{"ok"}, []string(names))
	assert.Len(t, emojiRepo.Emojis, 1)
}

// 括弧や空白を含む正規の URL は、符号化した値を保存する。
func TestUpsertEmojis_StoresEncodedURL(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	host := "remote.example"
	names := r.UpsertEmojis([]activitypub.EmojiTag{emojiTag(":p:", "https://remote.example/p (1).png")}, host)
	assert.Equal(t, []string{"p"}, []string(names))
	got, err := emojiRepo.FindByNameAndHost("p", &host)
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/p%20%281%29.png", got.OriginalURL)
	assert.Equal(t, "https://remote.example/p%20%281%29.png", got.PublicURL)
}

// 既存の絵文字では、新しい URL が崩れた形なら古い URL を残す。
func TestUpsertEmojis_KeepsStoredURLWhenNewOneIsMalformed(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	host := "remote.example"
	require.NoError(t, emojiRepo.Create(&model.Emoji{
		ID: "e1", Name: "m", Host: &host,
		OriginalURL: "https://remote.example/old.png",
		PublicURL:   "https://remote.example/old.png",
	}))
	names := r.UpsertEmojis([]activitypub.EmojiTag{emojiTag(":m:", "/new.png")}, host)
	assert.Equal(t, []string{"m"}, []string(names))
	got, err := emojiRepo.FindByNameAndHost("m", &host)
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/old.png", got.OriginalURL)
	assert.Equal(t, "https://remote.example/old.png", got.PublicURL)
}

// 符号化で originalUrl (512) を超えたら、長すぎる値と同じく tag ごと落とす。
func TestUpsertEmojis_DropsURLTooLongAfterEncoding(t *testing.T) {
	r, emojiRepo := newEmojiResolver(t)
	prefix := "https://remote.example/"
	raw := prefix + strings.Repeat("a", 512-len(prefix)-1) + " "
	require.Len(t, []rune(raw), 512, "符号化する前は上限ちょうど")
	names := r.UpsertEmojis([]activitypub.EmojiTag{emojiTag(":big:", raw)}, "remote.example")
	assert.Empty(t, []string(names))
	assert.Empty(t, emojiRepo.Emojis)
}
