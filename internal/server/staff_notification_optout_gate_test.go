package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/notification"
)

// 運営向けに配られる通知は、**利用者が自分では切れない** (#2987)。個人設定の
// `notificationRecieveConfig` は upstream の型しか知らないので、止める手段は
// ロールの `optOutNotificationTypes` だけになる。
//
// その候補一覧は fork frontend のリテラル (`mkGoOptOutTargetTypes`) で、
// **registry に型を足しても自動では増えない**。増やし忘れると「運営者へ配られる
// のにロールから切れない通知」が静かに生まれる。#2984 が「画面が 2 つあるのに
// 片方だけ」で踏んだのと同じ片側更新。
//
// **registry を truth にする。** 一覧を 2 箇所に置くのをやめられない以上、
// 少なくとも食い違いは検出する。
func TestStaffNotificationTypesAreOptOutable(t *testing.T) {
	path := filepath.Join(repoRootDir(t), "frontend",
		"packages", "frontend", "src", "pages", "admin", "roles.policy-editor.vue")
	src, err := os.ReadFile(path)
	if err != nil {
		require.NoError(t, err, "frontend/ は追跡しているので、%s を読めないのはパスの誤り", path)
	}

	// `const mkGoOptOutTargetTypes = ['a', 'b'] as const;` を読む。
	// **拾えなかったら落とす** — 書式が変わって空振りすると、検査していないのに
	// 緑になる。
	decl := regexp.MustCompile(`const mkGoOptOutTargetTypes = \[([^\]]*)\] as const;`).FindSubmatch(src)
	require.NotNil(t, decl,
		"%s から mkGoOptOutTargetTypes の宣言を読めない (書式が変わった?)", path)

	listed := []string{}
	for _, m := range regexp.MustCompile(`'([A-Za-z0-9_:]+)'`).FindAllSubmatch(decl[1], -1) {
		listed = append(listed, string(m[1]))
	}
	require.NotEmpty(t, listed, "%s の候補一覧が空", path)

	want := notification.StaffTypeNames()
	require.NotEmpty(t, want, "registry に Staff の通知が 1 つも無い; この gate は何も検査していない")

	sort.Strings(want)
	sort.Strings(listed)
	require.Equal(t, want, listed,
		"運営向け通知 (registry の Staff) と、ロールの opt-out の候補一覧が食い違っている。\n"+
			"足りないものは**配られるのにロールから切れない**。余っているものは\n"+
			"管理画面に存在しない型のスイッチが出る。")

	// **宣言されているだけでは出ない。** ループで回していることまで見る
	// (#2898 が policy キーの一覧で踏んだのと同じ形)。
	require.Regexp(t, `v-for="type in mkGoOptOutTargetTypes"`, string(src),
		"%s は宣言されているだけで、候補一覧を描画していない", path)

	// **ラベルの分岐も要る。** mk-go 固有の型は `_notification._types` に
	// 無いので、`mkGoNotificationTypeLabel` に分岐が無いと `table[type] ?? type`
	// が**生の識別子**をそのままスイッチのラベルとして描く (全言語)。
	// 一覧に足しただけで満足すると、管理画面に `emojiApplicationReceived` と
	// いう文字列が並ぶ。
	label := regexp.MustCompile(`(?s)function mkGoNotificationTypeLabel\([^)]*\)[^{]*\{(.*?)
\}`).FindSubmatch(src)
	require.NotNil(t, label,
		"%s から mkGoNotificationTypeLabel を読めない (書式が変わった?)", path)
	for _, typ := range want {
		require.Containsf(t, string(label[1]), "'"+typ+"'",
			"mkGoNotificationTypeLabel に %q の分岐が無い。"+
				"ロール編集画面のスイッチに生の型名が出る", typ)
	}
}
