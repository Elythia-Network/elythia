package entitycompat

import "testing"

// 通報の in-app 通知は router が notifier を配線しないと作られない (#2868)。
//
// **admin stream 側が配線されているので気付けない。** 管理画面を開いていれば
// newAbuseUserReport は届くため「通知は動いている」ように見え、通知欄に残らない
// ことだけが静かに欠ける。report-abuse は失敗を握り潰す (report 自体は
// 永続化済み) のでログにも出ない。
func TestAbuseReportInAppNotifierIsWired(t *testing.T) {
	assertWired(t, routerGo, "usersHandler.SetAbuseReportInAppNotifier(abuseInAppNotifier)",
		"通報がモデレーターの通知欄に残らない。admin stream は届くので\n"+
			"管理画面を開いている間は動いて見え、欠落に気付けない。")
	// notifier の中身も固定する (#3200)。Set 側だけ見ていると、構築の引数を
	// nil にしても gate は緑のまま NotifyNewReport が黙って no-op になる。
	assertWired(t, routerGo, "coreabuse.NewInAppNotifier(roleService, notificationService, repository.NewAbuseReportRepository(s.db))",
		"通報の通知を作る依存が欠け、通知欄に何も残らなくなる。")
}
