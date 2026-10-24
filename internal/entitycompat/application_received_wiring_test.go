package entitycompat

import "testing"

// 申請の受付通知 (#2987) は router が配線しないと出ない。
//
// **未配線でも build もテストも通る。** 症状は「通知が来ない」だけで、エラーも
// ログも出ない。しかも**気付く手段が無い** — 申請は成立し、審査画面にも並ぶので、
// 運営者は「通知機能が無い」と思うだけになる。#2762 が wiring-check を作ったのと
// 同じ型。
func TestApplicationReceivedNotificationIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"emojiApplicationService.SetReceivedNotifier(emojiapplication.NewReceivedNotifier(notificationService, roleService))",
		"絵文字の登録申請が出ても、審査できる人に通知が出なくなる。")
	assertWired(t, serverPkg,
		"signupApplicationService.SetReceivedNotifier(signupapplication.NewReceivedNotifier(notificationService, roleService))",
		"アカウントの登録申請が出ても、モデレーターに通知が出なくなる。")

	// **read 側の lookup も要る。** 未配線だと通知ごと drop されるので、
	// 発火側だけ配線しても通知欄には 1 件も出ない (fail-closed)。一覧
	// (i/notifications) の handler は lookup と権限の再確認の checker を構築子の
	// 必須の引数で受けるので、ここでは realtime の側だけを見る (#3817)。
	assertWired(t, serverPkg,
		"notificationPublisher.SetSignupApplicationLookup(signupApplicationNotifLookup)",
		"アカウントの登録申請の通知が realtime で出なくなる。\n"+
			"一覧には出るので、片方だけ壊れていることに気付きにくい。")
}
