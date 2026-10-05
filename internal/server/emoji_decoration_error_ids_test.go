package server

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/apierr"
	apii "github.com/elythia-network/elythia/internal/api/i"
)

// 同梱 frontend は絵文字デコレーションの保存エラーに独自の文面を出す (#2975)。
//
// **`os.apiWithDialog` の `customErrors` は `err.id` (UUID) で引く。** code を
// 書いても型は通り、vue-tsc も eslint も何も言わず、**ただ一度も一致しない**。
// 症状は「英語のサーバーメッセージと UUID がそのまま出る」だけなので、
// 本番でも気付きにくい。実際、敵対的レビューで見つかるまで code で書いていた。
//
// **同じ理由で、backend の UUID を変えると向こうの文面が黙って消える。** ここで
// 両方を突き合わせて、片側更新を止める。
func TestEmojiDecorationErrorIDsMatchFrontend(t *testing.T) {
	path := filepath.Join(repoRootDir(t), "frontend",
		"packages", "frontend", "src", "pages", "settings", "avatar-decoration.vue")
	src, err := os.ReadFile(path)
	if err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", path)
	}

	// `const ERR_X = '<uuid>';` を拾う。**書式が変わって 1 つも拾えなければ落とす**
	// — 検査していないのに緑になるのを避ける。
	re := regexp.MustCompile(`const (ERR_[A-Z0-9_]+) = '([0-9a-f-]{36})';`)
	found := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		found[m[1]] = m[2]
	}
	require.NotEmpty(t, found, "%s から ERR_* の UUID 定数を読めなかった (書式が変わった?)", path)

	want := map[string]string{
		"ERR_SENSITIVE_EMOJI_NOT_ALLOWED": apii.UUIDSensitiveEmojiNotAllowed,
		"ERR_NO_SUCH_EMOJI":               apierr.UUIDNoSuchEmoji,
		"ERR_RESTRICTED_BY_ROLE":          apierr.UUIDRestrictedByRole,
	}
	require.Equal(t, want, found,
		"avatar-decoration.vue の customErrors のキーが backend の error id と\n"+
			"食い違っている。`os.apiWithDialog` は `err.id` で引くので、ずれると\n"+
			"用意した文面が出ず、英語のサーバーメッセージ + UUID がそのまま出る。")

	// **定数が宣言されているだけでは足りない。** `emojiDecorationErrors()` の
	// キーとして実際に使われていることまで見る (宣言を残したままリテラルの
	// code に戻す形が、まさに直した不具合そのもの)。
	for name := range want {
		require.Containsf(t, string(src), "["+name+"]: { text:",
			"%s が customErrors のキーとして使われていない", name)
	}
}
