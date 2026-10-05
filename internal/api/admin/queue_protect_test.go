package admin

import (
	"testing"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/stretchr/testify/require"
)

// **利用者の不可逆な予定を、キュー全体の操作から守ること。**
//
// deliver キューには予約投稿とアカウント削除が同居している。促進すると全利用者の
// 未公開の予約投稿が即時公開され、クリアすると二度と発火しない (失敗通知も
// 出ない)。アカウント削除を消すと、削除フラグだけ立って中身が残るアカウントが
// 生まれ、dedup キーのせいで 24 時間は再実行も効かない。
func TestIsOperatorProtectedTask(t *testing.T) {
	t.Parallel()

	require.True(t, isOperatorProtectedTask(&QueueTaskSummary{Type: queue.TaskTypePostScheduledNote}),
		"予約投稿は守ること")
	require.True(t, isOperatorProtectedTask(&QueueTaskSummary{Type: queue.TaskTypeDeleteAccount}),
		"アカウント削除は守ること")

	// 配送系は従来どおり操作できること (詰まりを流すのが本来の用途)。
	require.False(t, isOperatorProtectedTask(&QueueTaskSummary{Type: queue.TaskTypeDeliver}))
	require.False(t, isOperatorProtectedTask(&QueueTaskSummary{Type: queue.TaskTypeInbox}))
	require.False(t, isOperatorProtectedTask(nil))
}
