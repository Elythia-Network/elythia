package queue_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestClient_EnqueueRouting pins which logical queue every enqueue helper
// targets.
//
// task type の接頭辞と積まれる queue がずれても handler は task type 単位で
// 登録されているため処理自体は走ってしまい、テストが無いと気付けない。実際
// `maintenance:reactionFlush` が deliver queue に積まれていた (#2327)。
//
// ここは「あるべき姿」ではなく **現在の実態** を固定する表。意図して配置を
// 変えるときは期待値も更新すること。deliver に載っている
// `note:postScheduled` / `maintenance:deleteAccount` は
// いずれも実行結果が連合配送につながるジョブで、maintenance (worker 2) より
// deliver (worker 16) の方が捌けるという判断による現状維持。
func TestClient_EnqueueRouting(t *testing.T) {
	cases := []struct {
		name     string
		enqueue  func(c *queue.Client) error
		taskType string
		queue    string
	}{
		{"deliver", func(c *queue.Client) error {
			return c.EnqueueDeliver(queue.DeliverPayload{Inbox: "https://a.example/inbox", Body: []byte(`{}`)})
		}, queue.TaskTypeDeliver, queue.QueueName},

		{"inbox", func(c *queue.Client) error {
			return c.EnqueueInbox(t.Context(), queue.InboxPayload{Body: []byte(`{}`)})
		}, queue.TaskTypeInbox, queue.InboxQueueName},

		{"export", func(c *queue.Client) error {
			return c.EnqueueExport(queue.ExportPayload{UserID: "u1"})
		}, queue.TaskTypeExport, queue.ExportQueueName},

		{"import", func(c *queue.Client) error {
			return c.EnqueueImport(queue.ImportPayload{UserID: "u1"})
		}, queue.TaskTypeImport, queue.ExportQueueName},

		{"webPush", func(c *queue.Client) error {
			return c.EnqueueWebPush(t.Context(), queue.WebPushPayload{UserID: "u1"})
		}, queue.TaskTypeWebPush, queue.PushQueueName},

		{"userWebhook", func(c *queue.Client) error {
			return c.EnqueueUserWebhook(t.Context(), queue.WebhookPayload{WebhookID: "w1"})
		}, queue.TaskTypeUserWebhook, queue.WebhookQueueName},

		{"systemWebhook", func(c *queue.Client) error {
			return c.EnqueueSystemWebhook(t.Context(), queue.WebhookPayload{WebhookID: "w1"})
		}, queue.TaskTypeSystemWebhook, queue.WebhookQueueName},

		// #2327: cron 経由 (Scheduler.Register) と同じ maintenance に積む。
		{"cleanRemoteNotes", func(c *queue.Client) error {
			return c.EnqueueCleanRemoteNotes()
		}, queue.TaskTypeCleanRemoteNotes, queue.MaintenanceQueueName},

		{"reactionFlush", func(c *queue.Client) error {
			return c.EnqueueReactionFlush()
		}, queue.TaskTypeReactionFlush, queue.MaintenanceQueueName},

		{"objectStorageDeleteFile", func(c *queue.Client) error {
			return c.EnqueueDeleteObjectStorageFile("k")
		}, queue.TaskTypeObjectStorageDeleteFile, queue.ObjectStorageQueueName},

		{"cleanRemoteFiles", func(c *queue.Client) error {
			return c.EnqueueCleanRemoteFiles()
		}, queue.TaskTypeCleanRemoteFiles, queue.ObjectStorageQueueName},

		// 以下は task type の接頭辞と queue が一致しないが現状維持 (上記 doc 参照)。
		{"postScheduledNote", func(c *queue.Client) error {
			return c.EnqueuePostScheduledNote(queue.PostScheduledNotePayload{NoteDraftID: "d1"})
		}, queue.TaskTypePostScheduledNote, queue.QueueName},

		{"deleteAccount", func(c *queue.Client) error {
			return c.EnqueueDeleteAccount(queue.DeleteAccountPayload{UserID: "u1"})
		}, queue.TaskTypeDeleteAccount, queue.QueueName},

		{"follow", func(c *queue.Client) error {
			return c.EnqueueFollow(queue.FollowPayload{FollowerID: "a", FolloweeID: "b"})
		}, queue.TaskTypeFollow, queue.RelationshipQueueName},

		{"unfollow", func(c *queue.Client) error {
			return c.EnqueueUnfollow(queue.UnfollowPayload{FollowerID: "a", FolloweeID: "b"})
		}, queue.TaskTypeUnfollow, queue.RelationshipQueueName},

		{"block", func(c *queue.Client) error {
			return c.EnqueueBlock(queue.BlockPayload{BlockerID: "a", BlockeeID: "b"})
		}, queue.TaskTypeBlock, queue.RelationshipQueueName},

		{"unblock", func(c *queue.Client) error {
			return c.EnqueueUnblock(queue.UnblockPayload{BlockerID: "a", BlockeeID: "b"})
		}, queue.TaskTypeUnblock, queue.RelationshipQueueName},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &payloadRecordingClient{}
			c := queue.NewClient(&stubDriver{client: rec})
			defer func() { _ = c.Close() }()

			require.NoError(t, tc.enqueue(c))
			assert.Equal(t, tc.taskType, rec.lastTaskType)
			assert.Equal(t, tc.queue, rec.lastOpts.Queue)
		})
	}
}
