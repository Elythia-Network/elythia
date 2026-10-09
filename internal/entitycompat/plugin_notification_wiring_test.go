package entitycompat

import "testing"

// 管理するアカウントへの通知をプラグインへ届ける配線 (#3469)。
//
// **どれを落としても build もテストも通る。** observer は nil なら呼ばないだけ
// なので、落ちるのは「bot に通知が届かない」ところ。エラーもログも出ない。
// Blocks を落とすと、ブロックした相手のメンションに bot が返事をするように
// なる (本体はブロックでは通知を止めない)。
func TestPluginNotificationDispatcherIsWired(t *testing.T) {
	const symptom = "プラグインが管理するアカウントへの通知・チャットが、プラグインの handler に届かない"
	assertWired(t, routerGo,
		"newPluginNotificationDispatcher(registeredPlugins, s.config.Plugins, s.queueClient, corepluginnotify.Deps{Users: userRepo, Mutes: mutingService, Blocks: blockingRepo})",
		symptom+" (またはミュート・ブロックした相手のものまで届く)")
	assertWired(t, routerGo, "notificationHook.SetCreatedObserver(d)", symptom)
	assertWired(t, routerGo, "chatService.SetMessageObserver(d)", symptom)
}
