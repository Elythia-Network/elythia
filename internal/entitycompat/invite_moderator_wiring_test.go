package entitycompat

import (
	"testing"
)

// invite/delete のモデレーター bypass は router で配線しないと効かない (#2812)。
//
// **配線を消しても build もテストも通る (実測)。** handler 側のテストは checker を
// 自分で注入するので router を見ておらず、`internal/server` に実 router を組み立てる
// テストはあるが (`role_assignment_routes_test.go`)、invite/delete のモデレーター
// 判定を通す経路が無い。#2762 の `TestTimelineTogglesAreWired` と同じ形でソースと
// して読んで固定する。
//
// router を通す挙動テストを書けば条件付きの配線まで押さえられるが、認証と role の
// 仕込みが要るのでここでは採っていない。強くするならその方向。
//
// 消えたときに壊れるのは「モデレーターが他人の / `createdById` が NULL の招待を
// 消せる」ところ。**厳しい側に倒れるので運用が壊れるわけではない**が、それは
// #2812 以前の状態そのもので、管理画面の削除ボタンが 400 を返し続ける形で
// 静かに戻る。
//
// **コメント行は数えない。** コメントアウトして残すのは消すのと同じ。判定は
// `wiringNodes` の AST 照合なので、`//` でも `/* */` でも同じように落ちる (#2856)。
//
// **引数まで含めて照合する。** 関数名だけを見ると `SetModeratorChecker(nil)` が
// 通り抜け、bypass が死んだまま緑になる。それでも `if cond { ... }` のような
// 条件付きの配線は検出できない — ソースを読む gate 共通の限界。
func TestInviteModeratorCheckerIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"inviteHandler.SetModeratorChecker(roleService)",
		"モデレーターが他人の / createdById が NULL の招待を消せなくなる (#2812)")
}
