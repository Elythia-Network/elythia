package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// blockCommentRe matches a `/* ... */` comment.
var blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)

// htmlCommentRe matches an `<!-- ... -->` comment.
//
// **template を無効化できる唯一の書き方。** Vue の template では `//` も `/* */` も
// コメントにならないので、`@contextmenu` を殺すならこれしかない。開始デリミタだけ
// 落とす実装では中身が残り、この gate の template 側の検査が空振りする。
var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

// lineCommentRe matches a `//` comment to end of line.
//
// 行頭とは限らない (`foo(); // メモ`)。URL の `//` を巻き込むが、この gate が
// 探すのは識別子の有無だけなので害が無い。
var lineCommentRe = regexp.MustCompile(`(?m)//.*$`)

// TestReactionLongPressIsWired asserts that the reaction button still binds the
// long-press handler that makes the emoji menu reachable on iOS (#2932).
//
// **配線を消してもビルドもテストも通る。** `bindLongPress` 自体は単体テストで
// 固定してあるが (`test/unit/long-press.test.ts`)、`MkReactionsViewer.reaction.vue`
// から呼ぶのをやめても vue-tsc も vitest も何も言わない。症状は「iOS でリアクションを
// 長押ししてもメニューが出ない」だけで、エラーもログも出ない。#2762 が
// `wiring-check` を作ったのと同じ型。
//
// **upstream 追従で落ちるのが一番ありうる。** このファイルは upstream のもので、
// fork は差分を載せているだけなので、`git rebase --onto` で衝突を解消するときに
// 3 行が消えても気付けない。実際 2026.9.0 への載せ替えでは同じ frontend の別
// ファイルで衝突が起きている (docs/divergence/frontend.md §4-2)。
//
// **コメントは落としてから探す。** コメントアウトして残すのは消すのと同じ
// (#2762 の初版が行頭 `//` しか除外せず、`/* */` で囲んだ配線を素通りさせた)。
// **`<!-- -->` も落とす** — template では `//` も `/* */` もコメントにならないので、
// `@contextmenu` を殺す唯一の書き方がこれ。初版は開始デリミタだけ消しており、
// template 側の検査が空振りしていた。
//
// **どちらの順序でもネストは解けない。** `//` の除去は URL を巻き込むので、
// `'https://…'` を含む行に配線を書くとその行の識別子ごと消えて gate が落ちる。
// 逆に `<!--` を先に落とす今の順序では、文字列や `//` コメントの中に `<!--` が
// 単独で現れ、後方のどこかに `-->` があると、その間の生きた配線が消える。
// どちらも偽陽性 (検査していないのに落ちる) で、現 corpus では起きない。
// 起きたら行を分けること。
func TestReactionLongPressIsWired(t *testing.T) {
	root := filepath.Join(repoRootDir(t), "frontend", "packages", "frontend")
	component := filepath.Join(root, "src", "components", "MkReactionsViewer.reaction.vue")
	utility := filepath.Join(root, "src", "utility", "long-press.ts")

	if _, err := os.Stat(component); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", component)
	}

	_, err := os.Stat(utility)
	require.NoError(t, err, "%s が無い。リアクションの長押しはこのファイルに実装がある (#2932)", utility)

	raw, err := os.ReadFile(component)
	require.NoError(t, err)
	src := stripComments(string(raw))

	require.Containsf(t, src, `from '@/utility/long-press.js'`,
		"%s が long-press を import していない。iOS ではリアクションからリモート絵文字を"+
			"インポートできなくなる (@contextmenu は iOS Safari では発火しない、#2932)", component)
	require.Containsf(t, src, "bindLongPress(",
		"%s が bindLongPress を呼んでいない。import だけ残っていても配線されていなければ同じこと", component)

	// **右クリックの経路も残っていること。** 長押しに寄せた拍子にデスクトップの
	// 導線を落とすと、今度は PC から開けなくなる。
	require.Containsf(t, src, "@contextmenu",
		"%s から @contextmenu が消えている。デスクトップの右クリックの導線が無くなる", component)

	// **iOS の callout 抑止。** これが無いと、長押しで「画像を保存」の吹き出しが
	// 出てメニューと競合する (ボタンの中身は絵文字の `<img>`)。
	require.Containsf(t, src, "-webkit-touch-callout: none",
		"%s に -webkit-touch-callout: none が無い。iOS で長押しすると画像の callout が出る", component)
}

// stripComments removes `//` and `/* */` comments so that commented-out code is
// not mistaken for live wiring.
func stripComments(src string) string {
	// **HTML コメントを先に落とす。** 中に `//` や `/* */` があると、先に行/ブロック
	// コメントを剥がした結果 `<!--` と `-->` が分断されて対応が取れなくなる。
	src = htmlCommentRe.ReplaceAllString(src, "")
	src = blockCommentRe.ReplaceAllString(src, "")
	src = lineCommentRe.ReplaceAllString(src, "")
	return src
}

// TestReactableRemoteReactionIsWired asserts that piggybacking on a remote
// reaction with a local same-named emoji stays wired end to end (#2697).
//
// **「設定はあるのに効かない」が一番ありうる壊れ方。** 既定が off なので、
// 配線が落ちても誰も気付かない — 設定画面にスイッチは出るし、切り替えも保存
// されるが、リモートのリアクションは押せないままになる。#2900 (backend は
// policy を読んでいたのに管理画面に UI が無かった) と同じ型。
//
// **upstream 追従で落ちるのが一番ありうる。** `MkReactionsViewer.reaction.vue`
// は upstream のファイルで、fork は差分を載せているだけなので、
// `git rebase --onto` の衝突解消で消えても気付けない。
//
// 見るのは 3 層。どれが欠けても「効かない設定」になる:
//
//   - 判定の実装 (`utility/reaction-alternative.ts`)
//   - コンポーネントの配線 (import / 呼び出し / 設定の読み出し / 押せるかの判定)
//   - 設定の定義と設定画面 (`preferences/def.ts` / `pages/settings/preferences.vue`)
func TestReactableRemoteReactionIsWired(t *testing.T) {
	root := filepath.Join(repoRootDir(t), "frontend", "packages", "frontend")
	component := filepath.Join(root, "src", "components", "MkReactionsViewer.reaction.vue")

	if _, err := os.Stat(component); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", component)
	}

	utility := filepath.Join(root, "src", "utility", "reaction-alternative.ts")
	_, err := os.Stat(utility)
	require.NoErrorf(t, err, "%s が無い。相乗りの判定はこのファイルにある (#2697)", utility)

	// **識別子の有無ではなく「接続」を名指しする。** `canReact` や
	// `sendingReaction` という**単語**を探す形だと、定義側を
	// `computed(() => canToggle.value)` / `computed(() => props.reaction)` に
	// 戻す変異が素通りする — 使う側の行は残るので単語は見つかり、しかし機能は
	// 完全に死ぬ (実測)。定義を丸ごと needle にする。
	//
	// **書式に強く依存するのは意図的。** 整形で needle が外れたらこの gate は
	// **落ちる** = 人が気付いて直せる。単語だけ見る形の失敗は「黙って検査が
	// 止まる」で、そちらのほうが悪い。
	for _, c := range []struct {
		path string
		want []string
	}{
		{
			path: component,
			want: []string{
				// 判定を呼んでいるか。設定を見ずに常時有効 / 常時無効にする変異も塞ぐ。
				`from '@/utility/reaction-alternative.js'`,
				"if (!prefer.s.reactableRemoteReactionEnabled) return null;",
				"\tvoid customEmojis.value;",
				"return localAlternativeReaction(props.reaction);",
				// 判定から「押せるか」「送る文字列」へ繋がっているか。
				// **ここが本体** — 単語検索ではこの 2 行を戻す変異を捕まえられない。
				"const canReact = computed(() => canToggle.value || ($i != null && localAlternative.value != null));",
				"const sendingReaction = computed(() => localAlternative.value ?? props.reaction);",
				// 押したときに送るのは sendingReaction (#3187 で送信部を sendReaction に
				// 切り出した)。mock の emit はチップ自身のキーのとき = 相乗りでないときだけ。
				"await sendReaction(sendingReaction.value, localAlternative.value == null);",
				// mock の emit は ownChip のときだけ (押す / 外すの 2 箇所)。
				"if (ownChip) emit('reactionToggled', reaction, (props.count - 1));",
				"if (ownChip) emit('reactionToggled', reaction, (props.count + 1));",
				// 「押せる」の述語を使う 4 箇所。どれも upstream 自身の行に戻せるので、
				// rebase の衝突解消で theirs を採ると自動的に壊れる。
				"[$style.reacted]: myReaction == reaction,",
				"[$style.canToggle]: canReact",
				`v-ripple="canReact"`,
				"if (!canReact.value) return;",
				"	if (canReact.value) {",
				// 絵文字の引き当てとパレットに入れる文字列も送る側で。
				// パレットに `:foo@host:` が入ると、パレットからだけリモートのキーを送る。
				"const reactionEmojiName = getEmojiNameFromReaction(reaction);",
				"customEmojisMap.get(reactionEmojiName)",
				"addToEmojiPalette(isLocalCustomEmoji.value || localAlternative.value != null",
				"`:${bareEmojiName(emojiName.value)}:`",
			},
		},
		{
			// 親の並び替え述語も揃っているか。揃っていないと
			// `showAvailableReactionsFirstInNote` が押せるチップを後ろへ並べる。
			path: filepath.Join(root, "src", "components", "MkReactionsViewer.vue"),
			want: []string{
				`from '@/utility/reaction-alternative.js'`,
				"return prefer.s.reactableRemoteReactionEnabled && localAlternativeReaction(reaction) != null;",
			},
		},
		{
			path: filepath.Join(root, "src", "preferences", "def.ts"),
			want: []string{"reactableRemoteReactionEnabled: {"},
		},
		{
			// **スイッチと model の両方を見る。** どちらかだけだと、
			// 片方を別のキーに書き換える変異が素通りする (実測)。
			path: filepath.Join(root, "src", "pages", "settings", "preferences.vue"),
			want: []string{
				`k="reactableRemoteReactionEnabled"`,
				`prefer.model('reactableRemoteReactionEnabled')`,
			},
		},
	} {
		raw, err := os.ReadFile(c.path)
		require.NoError(t, err, "read %s", c.path)
		src := stripComments(string(raw))
		for _, want := range c.want {
			require.Containsf(t, src, want,
				"%s に %q が無い。相乗り (#2697) の配線が落ちていると、設定は出るのに効かない",
				c.path, want)
		}
	}

	// **`toggleReaction` / `sendReaction` の中に `props.reaction` を残さない。**
	//
	// **例外を作らないこと。** mock (`MkTutorialDialog.*`) の emit だけはチップの identity を
	// 送りたくなるが、それをやると相乗り時に別のチップの count を動かす。
	// 相乗りのときは emit しない形にして、この禁止を保っている (#3187 以降は
	// `sendReaction` の `ownChip` 引数で受ける。本体で `props.reaction` と比べない)。
	// 差し替えた箇所は `props.reaction` が 12 / `emojiName.value` が 2 の計 14 で
	// (数え方は upstream 版の `toggleReaction` の中を `grep -c`)、名指しの検査では
	// **そのうち 1 つだけを戻す変異が素通りする** (実測)。関数の中身をまとめて見る。
	//
	// **送信の本体は `sendReaction` にある** (#3187 で「インポートしてリアクション」と
	// 共有するために切り出した)。`toggleReaction` だけを見ると、本体を丸ごと素通しする。
	//
	// `props.reaction` を読んでよいのは関数の外 (`localAlternativeReaction` に
	// 渡す computed や表示用の `emojiName`)。
	raw, err := os.ReadFile(component)
	require.NoError(t, err)
	src := stripComments(string(raw))
	for _, marker := range []string{"async function toggleReaction(", "async function sendReaction("} {
		start := strings.Index(src, marker)
		require.GreaterOrEqualf(t, start, 0, "%s に %s が無い (書式が変わった?)", component, marker)
		end := strings.Index(src[start:], "\n}\n")
		require.GreaterOrEqualf(t, end, 0, "%s の %s の終わりを読めない", component, marker)
		//
		// `emojiName.value` も同じ。`reactionEmojiName` は大文字の `E` で `.value` も
		// 付かないので、この禁止に引っかからない。
		body := src[start : start+end]
		forbidden := []string{"props.reaction", "emojiName.value"}
		// **`sendReaction` はチップ自身を読まない。** 「インポートしてリアクション」
		// (#3187) は取り込んだ `:name@.:` を引数で渡すので、本体が `sendingReaction` や
		// `localAlternative` (= チップのキー) を読むと、取り込んだ絵文字ではなく
		// チップのキーを送る。押したときの挙動は変わらないので、ここでしか捕まらない
		// (敵対的レビューが実測)。
		if marker == "async function sendReaction(" {
			forbidden = append(forbidden, "sendingReaction", "localAlternative")
		}
		for _, forbidden := range forbidden {
			require.NotContainsf(t, body, forbidden,
				"%s の %s が %s を使っている。相乗りのときリモートの"+
					"ショートコードを送る / 絵文字が引き当たらず楽観更新が落ちる (#2697)",
				component, marker, forbidden)
		}
	}
}
