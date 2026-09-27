package entitycompat

import "testing"

// リモートからの通報 (AP Flag) の通知も router の配線が要る (#2868)。
//
// **配線しないと通報の出どころで通知の有無が変わる。** local の
// report-abuse だけ通知が出て、連合経由の通報は通知欄に現れない。どちらも
// 同じ abuse_user_report 行として管理画面には出るので、通知が来ないことだけが
// 症状になる。
func TestRemoteAbuseReportNotificationIsWired(t *testing.T) {
	assertWired(t, routerGo, "federationProcessor.SetAbuseReportNotification(abuseInAppNotifier)",
		"連合経由の通報がモデレーターの通知欄に出ない。\n"+
			"管理画面には出るので、通知だけが欠ける形になる。")
}
