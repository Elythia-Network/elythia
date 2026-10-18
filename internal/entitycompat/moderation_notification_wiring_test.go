package entitycompat

import "testing"

// abuseReport 通知の read 時の権限確認は router が checker を配線しないと
// 成立しない (#2868)。
//
// **fail-closed なので機能ごと死ぬ。** checker 未配線だと abuseReport は
// 誰にも返らない (対象ユーザー ID が Extra に入り通報の存在自体が機微なので、判定できないまま出すより
// 出さないほうが安全側にしてある)。通知は作られ unread も飛ぶのに一覧に
// 出ないので、症状が原因から遠い。
func TestNotificationModeratorCheckerIsWired(t *testing.T) {
	assertWired(t, serverPkg, "notificationsHandler.SetModeratorChecker(roleService)",
		"abuseReport 通知が誰にも返らなくなる (fail-closed)。\n"+
			"通知の作成と unread は動くので、一覧に出ないことだけが症状になる。")
}
