package entitycompat

import "testing"

// 取り消したリノートの通知を消すには、router が DeleteService に notification hook を
// 配線する必要がある (#3201)。
//
// **消しても何も落ちない。** 一覧は read 時に削除済みノートの通知を落とす (#1953) ので
// 見た目は変わらず、未読件数に中身の無い通知が数えられ続けるだけになる。
// リアクション (SetNotificationHook は作成側と同じ配線) と連合の Undo(Announce)
// (federationProcessor.SetNotificationHook) は既存の配線を共有する。
func TestUndoNotificationHookIsWired(t *testing.T) {
	assertWired(t, routerGo, "noteDeleteService.SetNotificationHook(notificationHook)",
		"取り消したリノートの通知が stream に残り、未読件数に数えられ続ける。")
}
