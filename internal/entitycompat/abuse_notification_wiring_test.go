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
	assertWired(t, routerGo, "coreabuse.NewInAppNotifier(roleService, notificationService, repository.NewAbuseReportRepository(s.db))",
		"通報の通知を作る依存が欠け、通知欄に何も残らなくなる。")
	assertWired(t, routerGo, "coreabuse.NewCreatedNotifier(abuseInAppNotifier, roleService, stream.NewAdminStreamPublisher(streamPubSub))",
		"通報の通知欄と admin stream (newAbuseUserReport) が出なくなる。")
	assertWired(t, routerGo, "usersHandler.SetAbuseReportCreatedNotifier(abuseCreatedNotifier)",
		"ローカルの通報 (users/report-abuse) で通知が一切出なくなる。")
	assertWired(t, routerGo, "federationProcessor.SetAbuseReportCreatedNotifier(abuseCreatedNotifier)",
		"連合経由の通報 (Flag) で通知が一切出なくなる (#3256 と同じ形の穴)。")
	assertWired(t, routerGo, "abuseCreatedNotifier.SetWebhook(webhookService, recipientRepo, coreabuse.UserLookups{Instances: instanceRepo, Emojis: emojiRepo}, idGen)",
		"通報の abuseReport system webhook が、ローカルでも連合経由でも出なくなる。")
	assertWired(t, routerGo, "abuseCreatedNotifier.SetMail(miscsmtp.SubjectBodySenderFromMeta(metaRepo, s.config.ProxySMTP), recipientRepo, userRepo, metaRepo)",
		"通報のメールが、通知先にも meta.email にも届かなくなる (#3265)。")
}
