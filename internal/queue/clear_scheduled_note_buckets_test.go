package queue_test

import (
	"encoding/json"
	"testing"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bucketInspector は ClearScheduledNote が走査する bucket だけを差し替える。
// 未実装のメソッドを呼べば nil の埋め込みで panic するので、走査対象が増えた
// ことに気付ける。
type bucketInspector struct {
	driver.Inspector
	scheduled []*driver.TaskSummary
	retry     []*driver.TaskSummary
	// pending / active も走査対象 (upstream は delayed / waiting / active の
	// 3 バケットを見る)。
	pending []*driver.TaskSummary
	active  []*driver.TaskSummary
	deleted []string
}

func (i *bucketInspector) ListPendingTasks(_ string, page, _ int) ([]*driver.TaskSummary, error) {
	if page > 1 {
		return nil, nil
	}
	return i.pending, nil
}

func (i *bucketInspector) ListActiveTasks(_ string, page, _ int) ([]*driver.TaskSummary, error) {
	if page > 1 {
		return nil, nil
	}
	return i.active, nil
}

func (i *bucketInspector) ListScheduledTasks(_ string, page, _ int) ([]*driver.TaskSummary, error) {
	if page > 1 {
		return nil, nil
	}
	return i.scheduled, nil
}

func (i *bucketInspector) ListRetryTasks(_ string, page, _ int) ([]*driver.TaskSummary, error) {
	if page > 1 {
		return nil, nil
	}
	return i.retry, nil
}

func (i *bucketInspector) ListDelayedTasks(_ string, page, _ int) ([]*driver.TaskSummary, error) {
	if page > 1 {
		return nil, nil
	}
	return append(append([]*driver.TaskSummary{}, i.scheduled...), i.retry...), nil
}

func (i *bucketInspector) DeleteTask(_ string, taskID string) error {
	i.deleted = append(i.deleted, taskID)
	return nil
}

type bucketDriver struct {
	insp driver.Inspector
}

func (d *bucketDriver) Client() driver.Client       { return nil }
func (d *bucketDriver) Inspector() driver.Inspector { return d.insp }
func (d *bucketDriver) Server() driver.Server       { return nil }
func (d *bucketDriver) Scheduler() driver.Scheduler { return nil }
func (d *bucketDriver) Close() error                { return nil }
func (d *bucketDriver) WorkerCount(_ string) int    { return 0 }
func (d *bucketDriver) Resize(_ string, _ int) error {
	return driver.ErrResizeNotSupported
}

func scheduledNoteSummary(t *testing.T, taskID, draftID string) *driver.TaskSummary {
	t.Helper()
	body, err := json.Marshal(queue.PostScheduledNotePayload{NoteDraftID: draftID})
	require.NoError(t, err)
	return &driver.TaskSummary{ID: taskID, Type: queue.TaskTypePostScheduledNote, Payload: body}
}

// #3121: attempts を積んだので、予約投稿の job は「backoff 待ち」の状態を
// 取りうる。そちらは retry bucket にしか出ないので、delayed だけ見ていると
// **再スケジュールが古い job に届かず、backoff 明けに予定と違う時刻で公開
// される**。
func TestClearScheduledNote_ScansRetryBucket(t *testing.T) {
	insp := &bucketInspector{
		scheduled: []*driver.TaskSummary{scheduledNoteSummary(t, "sched-1", "target")},
		retry: []*driver.TaskSummary{
			scheduledNoteSummary(t, "retry-1", "target"),
			scheduledNoteSummary(t, "retry-2", "other"),
		},
	}
	c := queue.NewClient(&bucketDriver{insp: insp})

	require.NoError(t, c.ClearScheduledNote("target"))
	assert.Equal(t, []string{"sched-1", "retry-1"}, insp.deleted,
		"retry 待ちの job が消し残っている")
}

// **wait へ昇格した予約投稿も取り消せること。**
//
// delayed と retry だけだと、予約時刻が来て job が wait へ昇格した後に利用者が
// 時刻を後ろへ変更したとき、古い job が消されずそのまま発火し、**取り消したはず
// の時刻で公開される**。upstream は `getJobs(['delayed','waiting','active'])`。
func TestClearScheduledNote_ScansPendingAndActiveBuckets(t *testing.T) {
	for _, bucket := range []string{"pending", "active"} {
		t.Run(bucket, func(t *testing.T) {
			insp := &bucketInspector{}
			task := scheduledNoteSummary(t, "t1", "draft1")
			if bucket == "pending" {
				insp.pending = []*driver.TaskSummary{task}
			} else {
				insp.active = []*driver.TaskSummary{task}
			}
			c := queue.NewClient(&bucketDriver{insp: insp})
			require.NoError(t, c.ClearScheduledNote("draft1"))
			require.Equal(t, []string{"t1"}, insp.deleted,
				"%s バケットの job も消すこと", bucket)
		})
	}
}
