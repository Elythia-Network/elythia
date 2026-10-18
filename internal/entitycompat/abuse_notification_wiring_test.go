package entitycompat

import "testing"

// 通報の通知 (通知欄 #2868 / admin stream #1549 / abuseReport system webhook
// #1542) は router が配線しないと出ない。
//
// **入口ごとに配線すると、片方だけ抜けても気付けない。** #3256 までは
// ローカルの report-abuse にだけ admin stream と webhook を配線しており、
// 連合経由の Flag では通知欄しか出ていなかった。管理画面の一覧には両方とも
// 出るので、通知が来ないことだけが静かに欠ける。今は両方の入口が同じ
// CreatedNotifier を受け取るので、ここではその 4 つの配線を固定する。
func TestAbuseReportNotifierIsWired(t *testing.T) {
	// 通知欄: notifier の中身も固定する (#3200)。構築の引数を nil にしても
	// Set 側だけ見る gate は緑のまま、NotifyNewReport が黙って no-op になる。
	assertWired(t, serverPkg, "coreabuse.NewInAppNotifier(roleService, notificationService, repository.NewAbuseReportRepository(s.db))",
		"通報の通知を作る依存が欠け、通知欄に何も残らなくなる。")
	// 上の構築の行は buildCore の中にある (#3686)。setupRoutes が使うのは別名なので、
	// 構築から別名までの段も照合する。上の行だけだと、setupRoutes の別名を nil に
	// 書き換えても緑のまま通る (docs/architecture.md 4.2)。別名は buildCore の戻り値を
	// 受けた変数 coreSvcs から取る形にしてあり、別名を buildCore の呼び出しより前に
	// 書くとコンパイルが通らない。照合もその形で行う。
	assertWired(t, serverPkg, "abuseInAppNotifier: abuseInAppNotifier",
		"buildCore が作った通報の通知を core に入れていない。")
	assertWired(t, serverPkg, "coreSvcs := s.buildCore(w, idGen)",
		"setupRoutes が buildCore の core service を使っていない。")
	assertWired(t, serverPkg, "w.core = coreSvcs",
		"buildCore の core service を wiring に入れていない。")
	assertWired(t, serverPkg, "abuseInAppNotifier := coreSvcs.abuseInAppNotifier",
		"setupRoutes の通報の通知が buildCore で作ったものでなくなる。")
	assertWired(t, serverPkg, "coreabuse.NewCreatedNotifier(abuseInAppNotifier, roleService, stream.NewAdminStreamPublisher(streamPubSub))",
		"通報の通知欄と admin stream (newAbuseUserReport) が出なくなる。")
	assertWired(t, serverPkg, "usersHandler.SetAbuseReportCreatedNotifier(abuseCreatedNotifier)",
		"ローカルの通報 (users/report-abuse) で通知が一切出なくなる。")
	assertWired(t, serverPkg, "federationProcessor.SetAbuseReportCreatedNotifier(abuseCreatedNotifier)",
		"連合経由の通報 (Flag) で通知が一切出なくなる (#3256 と同じ形の穴)。")
	assertWired(t, serverPkg, "abuseCreatedNotifier.SetWebhook(webhookService, recipientRepo, coreabuse.UserLookups{Instances: instanceRepo, Emojis: emojiRepo}, idGen)",
		"通報の abuseReport system webhook が、ローカルでも連合経由でも出なくなる。")
	assertWired(t, serverPkg, "abuseCreatedNotifier.SetMail(miscsmtp.SubjectBodySenderFromMeta(metaRepo, s.config.ProxySMTP), recipientRepo, userRepo, metaRepo)",
		"通報のメールが、通知先にも meta.email にも届かなくなる (#3265)。")
}
