package entitycompat

import "testing"

// 個人ごとの受信の設定 (notificationRecieveConfig) は、Service に判定役を渡さないと、
// Service を直接呼ぶ通知 (実績・アプリ・投票の終了・予約投稿など) に効かない (#4058)。
// 渡し忘れても build も `internal/core/notification` のテストも緑のまま (テストは
// 自分で渡す) で、設定が「保存はされるが効かない」状態に戻る。
func TestNotificationReceiveConfigCheckerIsWired(t *testing.T) {
	assertWired(t, serverPkg, "notificationService.SetReceiveConfigChecker(notificationHook)",
		"実績・アプリ・投票の終了・予約投稿の通知が、受け取りの設定を無視して届く (#4058)")
}
