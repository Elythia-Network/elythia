package entitycompat

import "testing"

// abuseReport 通知の状態表示は router が lookup を配線しないと成立しない (#2868)。
//
// **fail-closed なので機能ごと死ぬ。** lookup 未配線だと abuseReport 通知は
// 誰にも返らない。read 時に状態を引けないまま「未対応」に見せると、対処済みの
// 通報に別のモデレーターが二重で当たるので、出さないほうを選んでいる。
//
// **配線先は 2 つある。** REST (i/notifications) と WebSocket の publisher で
// 別々に pack するので、片方だけ配線すると「一覧には出るが realtime では
// 出ない」という非対称になる。REST の handler は lookup と未対応の件数を
// 構築子の必須の引数で受けるので、ここでは見ない (#3817)。
func TestAbuseReportLookupIsWired(t *testing.T) {
	assertWired(t, serverPkg, "notificationPublisher.SetAbuseReportLookup(abuseNotifLookup)",
		"abuseReport 通知が realtime (WebSocket) で出なくなる。\n"+
			"一覧には出るので、片方だけ壊れていることに気付きにくい。")
	// 未対応の件数 (#3200)。通知をまとめているので、これが無いと最初の通報が
	// 対処済みになった時点で後続の通報に通知欄から気付けない。
	// realtime 側は router の lookup の中で数える。取れなくても通知は落とさない
	// 作りなので、この呼び出しを消しても他のテストは緑のまま件数だけが消える。
	assertWired(t, serverPkg, "abuseReportRepoForNotif.CountUnresolved()",
		"realtime (WebSocket) の通報の通知に未対応の件数が出なくなる。")
	// 呼び出しだけでは足りない。結果を捨てても (`_ = n`) 上の照合は通る。
	assertWired(t, serverPkg, "out.UnresolvedCount = &n",
		"数えた件数を捨てていて、realtime の通報の通知に件数が出ない。")
}
