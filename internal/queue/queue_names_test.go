package queue_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
)

// AllQueueNames が queue 名の唯一の出どころであること (#2690)。
//
// **定数を足したのに一覧へ入れ忘れる**のがこのテストで防ぎたい事故。以前は
// admin が upstream の名前を手書きで持っていて、mk-go が queue を足しても
// 追従せず、一時停止が 400 になっていた。
func TestAllQueueNames(t *testing.T) {
	got := queue.AllQueueNames()
	require.NotEmpty(t, got)

	// 定数として公開されている queue 名はすべて含まれること。
	for _, want := range []string{
		queue.QueueName,
		queue.InboxQueueName,
		queue.PushQueueName,
		queue.ExportQueueName,
		queue.WebhookQueueName,
		queue.MaintenanceQueueName,
		queue.ObjectStorageQueueName,
		queue.RelationshipQueueName,
	} {
		assert.Contains(t, got, want)
	}

	seen := map[string]bool{}
	for _, q := range got {
		assert.NotEmpty(t, q)
		assert.False(t, seen[q], "重複: %s", q)
		seen[q] = true
	}
	// 呼び出し側が書き換えても次の呼び出しに影響しないこと。
	got[0] = "mutated"
	assert.NotEqual(t, "mutated", queue.AllQueueNames()[0])
}
