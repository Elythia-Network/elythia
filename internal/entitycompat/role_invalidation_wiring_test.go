package entitycompat

import (
	"go/ast"
	"testing"

	"github.com/stretchr/testify/require"
)

// ロール / ポリシーのキャッシュ無効化は**プロセス内で閉じている**ので、
// `internal:rolesUpdated` に繋がないと他のワーカーへ届かない。
//
// 症状が見えないのが問題。`roleCacheTTL` は 5 分で、複数プロセス構成
// (`MK_ONLY_SERVER` / `MK_ONLY_QUEUE`) は明示的にサポートされている。
// **侵害された管理者のロールを剥奪しても、剥奪操作を受け付けなかった側の
// ノードでは最大 5 分間、管理 API が通り続ける。**
//
// `internal/server` は CI のカバレッジ対象外 (CLAUDE.md Section 4) で router を
// 組み立てるテストも無いため、配線の抜けは build もテストも緑のまま起きる。
// captcha (`captcha_reload_wiring_test.go`) と同じ理由で AST で見る。
//
// **2 箇所とも要る。** 自 worker の更新は hook、他 worker からの受信は
// subscriber を通る。
//
//   - hook だけ → 自分の変更を他へ伝えられない
//   - subscriber だけ → 他 worker の変更を受け取れない
//
// **受信側が呼ぶのは `InvalidateAllCachesLocally`。** 通常の invalidate を
// 呼ぶとそこから再び publish され、ワーカー同士が通知を投げ合って止まらなく
// なる。
func TestRoleInvalidationIsWired(t *testing.T) {
	// internal/server の全ファイルを読む (#3672)。組み立てのコードを別の
	// ファイルへ移しても、同じ配線を見続ける。
	_, files := parseServerFiles(t, 0)

	var (
		sawHook        bool
		sawSubscriber  bool
		hookPublishes  bool
		subInvalidates bool
		subRepublishes bool
	)

	callsSelector := func(n ast.Node, recv, name string) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != name {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && (recv == "" || id.Name == recv) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	publishesRolesUpdated := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Publish" {
				return true
			}
			if hasStringArg(call, "rolesUpdated") {
				found = true
				return false
			}
			return true
		})
		return found
	}

	inspectServerFiles(files, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "SetInvalidationHook":
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "roleService" {
				return true
			}
			sawHook = true
			for _, a := range call.Args {
				if publishesRolesUpdated(a) {
					hookPublishes = true
				}
			}
		case "Subscribe":
			if !hasStringArg(call, "rolesUpdated") {
				return true
			}
			sawSubscriber = true
			for _, a := range call.Args {
				if callsSelector(a, "roleService", "InvalidateAllCachesLocally") {
					subInvalidates = true
				}
				if publishesRolesUpdated(a) {
					subRepublishes = true
				}
			}
		}
		return true
	})

	// **見つからなかったら落とす。** 書き方が変わって探せなくなると、検査して
	// いないのに緑になる。
	require.True(t, sawHook, "roleService.SetInvalidationHook の呼び出しが見つからない。書き方を変えたならこの gate も直すこと")
	require.True(t, sawSubscriber, `"rolesUpdated" の Subscribe が見つからない。書き方を変えたならこの gate も直すこと`)

	require.True(t, hookPublishes,
		"roleService の invalidation hook が rolesUpdated を publish していない。\n"+
			"ロールを剥奪しても他 worker には最大 roleCacheTTL (5 分) 届かない")
	require.True(t, subInvalidates,
		`"rolesUpdated" の subscriber が roleService.InvalidateAllCachesLocally を呼んでいない。`+"\n"+
			"他 worker の変更を受け取れない")
	require.False(t, subRepublishes,
		`"rolesUpdated" の subscriber が同じ channel へ publish し返している。`+"\n"+
			"ワーカー同士が通知を投げ合って止まらなくなる")
}
