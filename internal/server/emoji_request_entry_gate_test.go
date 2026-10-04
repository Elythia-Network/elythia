package server

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// カスタム絵文字の登録申請への導線は 3 箇所ある (#2989)。
//
// **判定を書き分けさせない。** 条件は「ログイン済み + `canRequestCustomEmojis` +
// モデレーターでない + `canManageCustomEmojis` を持たない」で、3 箇所で同じ式を
// 書くと、条件を変えたときに一部の画面だけ違う状態が残る。#2984 が「画面が
// 2 つあるのに片方だけ」で踏んだのと同じ形なので、**共通ヘルパーを通っているか**を
// 機械で見る。
//
// **式そのものは検査しない** — ヘルパーの中身は自由に変えてよい。見るのは
// 「各導線がヘルパーを経由しているか」と「生の policy 判定を書き戻していないか」。
type emojiRequestEntry struct {
	// why is the human-readable place, used in failure messages.
	why string
	// condition must appear verbatim — **ヘルパーの戻り値が表示条件に
	// 使われているか**まで見る。import があるかだけだと、条件を生の policy
	// 判定へ書き換えても素通りする (敵対的レビューで実測: モデレーターにも
	// メニューが出る形、`=== true` を落として非 bool でも出る形の両方)。
	condition string
}

var emojiRequestEntryFiles = map[string]emojiRequestEntry{
	"pages/settings/other.vue": {"設定 → その他", `v-if="canRequestCustomEmojis"`},
	"pages/about.emojis.vue":   {"カスタム絵文字一覧 (/about#emojis)", `v-else-if="canRequestEmoji"`},
	"ui/_common_/common.ts":    {"インスタンスメニュー", "if (canShowEmojiRequestEntry()) {"},
}

// emojiRequestEntryHelper is the module every entry point must go through.
const emojiRequestEntryHelper = "@/utility/emoji-request-entry.js"

func TestEmojiRequestEntriesUseTheSharedHelper(t *testing.T) {
	fe := filepath.Join(repoRootDir(t), "frontend", "packages", "frontend", "src")
	if _, err := os.Stat(filepath.Join(fe, "utility", "emoji-request-entry.ts")); err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、ヘルパーを読めないのはパスの誤り")
	}

	// 生の policy 判定。ヘルパーへ寄せたあとにこれが戻ると、そこだけ条件が
	// ずれても誰も気付かない。
	// **truthy 判定への書き戻しも拾う。** `=== true` 限定にすると、
	// `$i.policies.canRequestCustomEmojis` と書くだけで素通りする (policy は
	// admin API から型検証なしに書けるので、文字列が届くと導線が出る)。
	rawCheck := regexp.MustCompile(`policies[.\['"]+canRequestCustomEmojis`)

	// **遷移先は「引用符で囲まれた完全なパス」で見る。** 部分一致にすると
	// ヘルパーの import パス (`@/utility/emoji-request-entry.js`) が
	// `/emoji-request` を含むので、**導線を消しても通る** (実測)。
	routeRe := regexp.MustCompile(`['"]/emoji-request['"]`)

	for rel, entry := range emojiRequestEntryFiles {
		path := filepath.Join(fe, rel)
		raw, err := os.ReadFile(path)
		require.NoErrorf(t, err, "%s (%s) を読めない", rel, entry.why)
		src := string(raw)

		require.Regexpf(t, routeRe, src,
			"%s (%s) に申請ページへの導線が無い", rel, entry.why)
		require.Containsf(t, src, emojiRequestEntryHelper,
			"%s (%s) が共通ヘルパーを読み込んでいない", rel, entry.why)
		// **読み込んでいるだけでは足りない。** 表示条件がヘルパーの戻り値で
		// あることまで見る (#2900 が policy 編集で踏んだのと同じ形)。
		require.Containsf(t, src, entry.condition,
			"%s (%s) の表示条件が共通ヘルパーの戻り値になっていない。\n"+
				"期待する式: %s\n"+
				"import を残したまま条件だけ書き換えると、そこだけ判定がずれる", rel, entry.why, entry.condition)
		require.NotRegexpf(t, rawCheck, src,
			"%s (%s) が policy を直接見ている。判定はヘルパーに寄せること", rel, entry.why)
	}
}
