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

// ミュートの判定も同じで、Service に判定役を渡さないと、Service を直接呼ぶ通知 (絵文字の
// 申請を受けた通知) がミュートしている相手からも届く (#4065)。Hook 側の配線も、外すと
// Hook を通る全ての通知でミュートが効かなくなるので一緒に見張る。
func TestNotificationMuteCheckerIsWired(t *testing.T) {
	assertWired(t, serverPkg, "notificationService.SetMuteChecker(mutingService)",
		"ミュートしている相手からの絵文字の申請の通知が届く (#4065)")
	assertWired(t, serverPkg, "notificationHook.SetMuteChecker(mutingService)",
		"ミュートしている相手からの通知が届く")
}
