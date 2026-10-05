package reaction_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ローカルとリモートに同じショートコードの絵文字があるとき、リアクションの
// 解決がどちらへ倒れるかを固定する (#2697)。
//
// **fork frontend の「リアクションの相乗り」がこの不変条件に乗っている。**
// リモートのリアクション (`:foo@host:`) のチップを押したとき、frontend は
// ローカルの `:foo@.:` を送る。送る側のショートコードを選び直すだけで済むのは、
// backend が**文字列に書かれた host を尊重する**からで、ここを「ローカル優先」に
// 変えると AP の受信側が壊れる — リモート利用者が送ってくる `:foo@their.host:` は
// そのホストの絵文字を指しているのに、ローカルの同名絵文字として記録される。
//
// **同名が両方あるときのテストが無かった。** 既存の
// `TestService_Create_CustomEmojiRemote` はリモートだけを seed しているので、
// 「ローカル優先に変えた」変異を検出できない (ローカルが無いので結果が変わらない)。
// 相乗りが成立する条件そのものを固定する。
func TestService_Create_SameShortcodeLocalAndRemote(t *testing.T) {
	host := "remote.example"

	seed := func(t *testing.T) (create func(*model.User, string) (string, error)) {
		t.Helper()
		svc, repo, _, emojiRepo, _ := newService(t)
		seedNote(repo, "n1", "author", model.NoteVisibilityPublic)
		// mock の key は `name@host` で、ローカルは host が空。
		emojiRepo.Emojis["smile@"] = &model.Emoji{Name: "smile"}
		emojiRepo.Emojis["smile@remote.example"] = &model.Emoji{Name: "smile", Host: &host}
		return func(u *model.User, r string) (string, error) { return svc.Create(u, "n1", r) }
	}

	t.Run("host 付きはローカルに同名があってもリモートのまま", func(t *testing.T) {
		create := seed(t)
		got, err := create(&model.User{ID: "viewer"}, ":smile@remote.example:")
		require.NoError(t, err)
		assert.Equal(t, ":smile@remote.example:", got,
			"ローカル優先に変えると AP の受信側が別の絵文字として記録する")
	})

	t.Run("host 無しをローカル利用者が送ればローカル", func(t *testing.T) {
		create := seed(t)
		got, err := create(&model.User{ID: "viewer"}, ":smile:")
		require.NoError(t, err)
		assert.Equal(t, ":smile@.:", got, "相乗りが送るのはこの形")
	})

	t.Run("ローカルホストマーク付きもローカル", func(t *testing.T) {
		create := seed(t)
		got, err := create(&model.User{ID: "viewer"}, ":smile@.:")
		require.NoError(t, err)
		assert.Equal(t, ":smile@.:", got, "fork frontend が実際に送る文字列")
	})

	t.Run("host 無しをリモート利用者が送れば actor の host", func(t *testing.T) {
		// #459 の経路。ローカルに同名があってもこちらが勝つ。
		create := seed(t)
		got, err := create(&model.User{ID: "remote-user", Host: &host}, ":smile:")
		require.NoError(t, err)
		assert.Equal(t, ":smile@remote.example:", got)
	})
}
