package queue_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/testutil"
)

// 期限の時刻のジョブ (#3994) は maintenance に、期限まで遅らせて積む。dedup は
// 走る時刻までにする (完了しても印が消えないので、長くすると積み直せなくなる)。
func TestScheduleAnnouncementArchive_Options(t *testing.T) {
	c, rc := newRecordingClient(nil)
	at := time.Now().Add(time.Hour)
	before := time.Now()
	require.NoError(t, c.ScheduleAnnouncementArchive("a1", at))
	require.Len(t, rc.enqueued, 1)
	got := rc.enqueued[0]
	assert.Equal(t, queue.TaskTypeArchiveAnnouncements, got.taskType)
	assert.Equal(t, queue.MaintenanceQueueName, got.opts.Queue)

	var p queue.ArchiveAnnouncementPayload
	require.NoError(t, json.Unmarshal(got.payload, &p))
	assert.Equal(t, queue.ArchiveAnnouncementPayload{AnnouncementID: "a1", AutoArchiveAt: at.UnixMilli()}, p)

	// 期限より前には走らない (遅延は「期限 - 積んだ時刻」以上)。遅れは 1 秒と少し。
	assert.GreaterOrEqual(t, got.opts.ProcessIn, at.Sub(before))
	assert.LessOrEqual(t, got.opts.ProcessIn, at.Sub(before)+2*time.Second)
	assert.Equal(t, got.opts.ProcessIn, got.opts.UniqueTTL)

	assert.True(t, got.opts.MaxRetrySet)
	assert.Equal(t, 3, got.opts.MaxRetry)
	assert.Equal(t, driver.BackoffExponential, got.opts.BackoffType)
	assert.Equal(t, time.Minute, got.opts.BackoffDelay)
	assert.Equal(t, queue.ScheduledJobRetention, got.opts.KeepCompletedAge)
	assert.Equal(t, queue.ScheduledJobRetention, got.opts.KeepFailedAge)
}

// 期限を過ぎた予定は、すぐ走らせて dedup しない。
func TestScheduleAnnouncementArchive_PastRunsNow(t *testing.T) {
	c, rc := newRecordingClient(nil)
	require.NoError(t, c.ScheduleAnnouncementArchive("a1", time.Now().Add(-time.Minute)))
	require.Len(t, rc.enqueued, 1)
	assert.Zero(t, rc.enqueued[0].opts.ProcessIn)
	assert.Zero(t, rc.enqueued[0].opts.UniqueTTL)
}

func TestScheduleQuoteRequestResend_Options(t *testing.T) {
	c, rc := newRecordingClient(nil)
	at := time.Now().Add(time.Minute)
	before := time.Now()
	require.NoError(t, c.ScheduleQuoteRequestResend("n1", at))
	require.Len(t, rc.enqueued, 1)
	got := rc.enqueued[0]
	assert.Equal(t, queue.TaskTypeResendQuoteRequests, got.taskType)
	assert.Equal(t, queue.MaintenanceQueueName, got.opts.Queue)

	var p queue.ResendQuoteRequestPayload
	require.NoError(t, json.Unmarshal(got.payload, &p))
	assert.Equal(t, queue.ResendQuoteRequestPayload{NoteID: "n1", At: at.UnixMilli()}, p)

	assert.GreaterOrEqual(t, got.opts.ProcessIn, at.Sub(before))
	assert.LessOrEqual(t, got.opts.ProcessIn, at.Sub(before)+2*time.Second)
	assert.Equal(t, got.opts.ProcessIn, got.opts.UniqueTTL)
	assert.Equal(t, queue.ResendSlotRetries, got.opts.MaxRetry)
	assert.Equal(t, 5, queue.ResendSlotRetries, "retries until the 3-minute tolerance")
	assert.Equal(t, driver.BackoffFixed, got.opts.BackoffType)
	assert.Equal(t, queue.ResendSlotRetryInterval, got.opts.BackoffDelay)
	assert.Equal(t, queue.ScheduledJobRetention, got.opts.KeepCompletedAge)
}

// 実 Redis: 同じ予定を 2 回積んでも (起動時の積み直しが重なっても) 遅延ジョブは 1 つ。
// 予定が違えば別のジョブになる。
func TestScheduleAnnouncementArchive_DedupOnRedis(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)
	c := queue.NewClient(newDriver(t))
	at := time.Now().Add(time.Hour)
	require.NoError(t, c.ScheduleAnnouncementArchive("a1", at))
	require.NoError(t, c.ScheduleAnnouncementArchive("a1", at))
	require.NoError(t, c.ScheduleAnnouncementArchive("a1", at.Add(time.Minute)))
	require.NoError(t, c.ScheduleQuoteRequestResend("n1", at))
	require.NoError(t, c.ScheduleQuoteRequestResend("n1", at))

	rows, err := newInspector(t).ListScheduledTasks(queue.MaintenanceQueueName, 1, 100)
	require.NoError(t, err)
	byType := map[string]int{}
	for _, r := range rows {
		byType[r.Type]++
	}
	// 予定の時刻より前に走る位置に置かない。delayed の ZSET の score は BullMQ の
	// 形 (走る時刻のミリ秒 * 0x1000 + 連番)。
	zs, err := testRedis.Client.ZRangeWithScores(context.Background(), "bull:"+queue.MaintenanceQueueName+":delayed", 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, zs, 3)
	for _, z := range zs {
		fire := int64(z.Score) / 0x1000
		assert.GreaterOrEqual(t, fire, at.UnixMilli(), "job %v fires before its due time", z.Member)
	}
	assert.Equal(t, map[string]int{
		queue.TaskTypeArchiveAnnouncements: 2,
		queue.TaskTypeResendQuoteRequests:  1,
	}, byType)
}

// 起動時のジョブ (#3994) は、2 種類をすぐ走らせる形で積む。dedup しない (複数の
// プロセスが積んでも、処理は一度しか効かない)。
func TestEnqueueDueJobsAtStartup(t *testing.T) {
	c, rc := newRecordingClient(nil)
	require.NoError(t, c.EnqueueDueJobsAtStartup())
	require.Len(t, rc.enqueued, 2)
	assert.Equal(t, queue.TaskTypeArchiveAnnouncements, rc.enqueued[0].taskType)
	assert.Equal(t, queue.TaskTypeResendQuoteRequests, rc.enqueued[1].taskType)
	for _, got := range rc.enqueued {
		assert.JSONEq(t, `{"atStartup":true}`, string(got.payload), got.taskType)
		assert.Equal(t, queue.MaintenanceQueueName, got.opts.Queue)
		assert.Zero(t, got.opts.ProcessIn)
		assert.Zero(t, got.opts.UniqueTTL)
		assert.True(t, got.opts.MaxRetrySet)
	}
	// 起動の直後は DB の準備中でありうるので、1・2・4・8 分後に試し直す。
	for _, got := range rc.enqueued {
		assert.Equal(t, 4, got.opts.MaxRetry, got.taskType)
		assert.Equal(t, driver.BackoffExponential, got.opts.BackoffType, got.taskType)
		assert.Equal(t, time.Minute, got.opts.BackoffDelay, got.taskType)
	}

	// 1 つ積めなくても、もう 1 つは積み、失敗を返す。
	c, rc = newRecordingClient(func(taskType string, _ []byte) error {
		if taskType == queue.TaskTypeArchiveAnnouncements {
			return assert.AnError
		}
		return nil
	})
	assert.Error(t, c.EnqueueDueJobsAtStartup())
	require.Len(t, rc.enqueued, 1)
	assert.Equal(t, queue.TaskTypeResendQuoteRequests, rc.enqueued[0].taskType)
}

// 期限の前に走った枠のジョブ (管理画面の promote) は、payload を変えて dedup せずに
// 期限の時刻へ積み直す (#3994)。同じ予定の dedup の印に弾かれない。
func TestRequeueQuoteRequestResend(t *testing.T) {
	c, rc := newRecordingClient(nil)
	at := time.Now().Add(time.Minute)
	before := time.Now()
	require.NoError(t, c.RequeueQuoteRequestResend("n1", at))
	require.Len(t, rc.enqueued, 1)
	got := rc.enqueued[0]
	assert.Equal(t, queue.TaskTypeResendQuoteRequests, got.taskType)
	assert.Equal(t, queue.MaintenanceQueueName, got.opts.Queue)
	assert.JSONEq(t, fmt.Sprintf(`{"noteId":"n1","at":%d,"requeued":true}`, at.UnixMilli()), string(got.payload))
	assert.GreaterOrEqual(t, got.opts.ProcessIn, at.Sub(before))
	assert.Zero(t, got.opts.UniqueTTL, "no dedup")
	assert.Equal(t, queue.ResendSlotRetries, got.opts.MaxRetry)
}

// 実 Redis: 同じ予定のジョブが dedup の印を残したままでも、積み直しは入る。
func TestRequeueQuoteRequestResend_NotBlockedByDedupOnRedis(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)
	c := queue.NewClient(newDriver(t))
	at := time.Now().Add(time.Hour)
	require.NoError(t, c.ScheduleQuoteRequestResend("n1", at))
	require.NoError(t, c.RequeueQuoteRequestResend("n1", at))
	n, err := testRedis.Client.ZCard(context.Background(), "bull:"+queue.MaintenanceQueueName+":delayed").Result()
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)
}
