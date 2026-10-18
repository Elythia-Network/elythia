package entitycompat

import "testing"

// ロール単位の通知 opt-out は router が resolver を配線しないと効かない (#2898)。
//
// **効かない側に倒れるので気付けない。** Service は resolver 未設定なら全ての
// 通知を通す (fail-open)。管理者が optOutNotificationTypes を設定しても通知は
// 届き続け、エラーもログも出ない。#2762 の meta.enableFanoutTimelineDbFallback
// と同じ「列と設定 UI はあるが読み取り経路に配線されていない」型。
func TestNotificationPolicyResolverIsWired(t *testing.T) {
	assertWired(t, serverPkg, "notificationService.SetPolicyResolver(roleService)",
		"optOutNotificationTypes を設定しても通知が届き続ける。\n"+
			"エラーもログも出ないので、設定した側からは効いているように見える。")
}
