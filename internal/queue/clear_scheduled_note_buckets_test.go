package queue_test

import (
	"encoding/json"
	"errors"
	"fmt"
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

// ListDelayedTasks は delayed 全体 (scheduled + retry) を、実物と同じく添字の
// 範囲で切って返す。ListScheduledTasks / ListRetryTasks は実装しない。あちらは
// ページごとに delayed の全件を読み直すので、走査に使ったら panic で気付く
// (#3169)。
func (i *bucketInspector) ListDelayedTasks(_ string, page, pageSize int) ([]*driver.TaskSummary, error) {
	all := append(append([]*driver.TaskSummary{}, i.scheduled...), i.retry...)
	start := (page - 1) * pageSize
	if start >= len(all) {
		return nil, nil
	}
	return all[start:min(start+pageSize, len(all))], nil
}

// DeleteTask は実物と同じく一覧から取り除く。たどりながら消すと、後ろの
// task が前へ詰まって次のページで読み飛ばされる。
func (i *bucketInspector) DeleteTask(_ string, taskID string) error {
	i.deleted = append(i.deleted, taskID)
	drop := func(rows []*driver.TaskSummary) []*driver.TaskSummary {
		out := rows[:0:0]
		for _, t := range rows {
			if t.ID != taskID {
				out = append(out, t)
			}
		}
		return out
	}
	i.scheduled = drop(i.scheduled)
	i.retry = drop(i.retry)
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

// **たどり終えてから消すこと (#3169)。** 一覧は添字の範囲で切るので、1 ページ
// 目で消すと 2 ページ目の先頭が 1 ページ目へ詰まり、読み飛ばされる。同じ
// 下書きの job が 2 つ (古い予定と backoff 待ちなど) あり、ページをまたいで
// いると、片方が残って予定と違う時刻に公開される。
func TestClearScheduledNote_DeletesAfterWalking(t *testing.T) {
	scheduled := []*driver.TaskSummary{scheduledNoteSummary(t, "target-1", "target")}
	for i := 0; i < 99; i++ {
		scheduled = append(scheduled, scheduledNoteSummary(t, fmt.Sprintf("other-%d", i), "other"))
	}
	insp := &bucketInspector{
		scheduled: scheduled,
		retry:     []*driver.TaskSummary{scheduledNoteSummary(t, "target-2", "target")},
	}
	c := queue.NewClient(&bucketDriver{insp: insp})

	require.NoError(t, c.ClearScheduledNote("target"))
	assert.Equal(t, []string{"target-1", "target-2"}, insp.deleted)
}

// **途中のページが短くても止まらないこと (#3169)。** mkq は HASH が既に無い
// job を飛ばすので、途中のページが 100 件に満たないことがある。
func TestClearScheduledNote_ContinuesPastShortPage(t *testing.T) {
	var scheduled []*driver.TaskSummary
	for i := 0; i < 150; i++ {
		scheduled = append(scheduled, scheduledNoteSummary(t, fmt.Sprintf("other-%d", i), "other"))
	}
	scheduled = append(scheduled, scheduledNoteSummary(t, "target-1", "target"))
	insp := &shortPageInspector{bucketInspector: bucketInspector{scheduled: scheduled}}
	c := queue.NewClient(&bucketDriver{insp: insp})

	require.NoError(t, c.ClearScheduledNote("target"))
	assert.Equal(t, []string{"target-1"}, insp.deleted)
}

// shortPageInspector drops one entry from the first delayed page, like mkq
// does when a job's HASH vanished between the range read and HGETALL.
type shortPageInspector struct {
	bucketInspector
}

func (i *shortPageInspector) ListDelayedTasks(q string, page, pageSize int) ([]*driver.TaskSummary, error) {
	rows, err := i.bucketInspector.ListDelayedTasks(q, page, pageSize)
	if page == 1 && len(rows) > 0 {
		rows = rows[1:]
	}
	return rows, err
}

// **一覧の取得に途中で失敗しても、それまでに見つけた job は消すこと (#3169)。**
// 残すと、時刻を変えた予約投稿が古い時刻で公開される。
func TestClearScheduledNote_DeletesFoundTasksOnListError(t *testing.T) {
	scheduled := []*driver.TaskSummary{scheduledNoteSummary(t, "target-1", "target")}
	for i := 0; i < 99; i++ {
		scheduled = append(scheduled, scheduledNoteSummary(t, fmt.Sprintf("other-%d", i), "other"))
	}
	insp := &failingPageInspector{bucketInspector: bucketInspector{scheduled: scheduled}}
	c := queue.NewClient(&bucketDriver{insp: insp})

	require.Error(t, c.ClearScheduledNote("target"))
	assert.Equal(t, []string{"target-1"}, insp.deleted)
}

// failingPageInspector fails the second delayed page.
type failingPageInspector struct {
	bucketInspector
}

func (i *failingPageInspector) ListDelayedTasks(q string, page, pageSize int) ([]*driver.TaskSummary, error) {
	if page >= 2 {
		return nil, errors.New("redis down")
	}
	return i.bucketInspector.ListDelayedTasks(q, page, pageSize)
}

// **同じ job を二度消さないこと (#3169)。** 走査中に新しい job が先頭に入ると、
// 読んだ job が後ろへずれて次のページにもう一度出る。二度目の削除は、既に
// 無い job として失敗を返すので、消せたのに失敗と報告することになる。
func TestClearScheduledNote_DeletesDuplicateOnce(t *testing.T) {
	// 1 ページ目を読む直前に 1 件、2 ページ目の直前にもう 1 件が先頭に入る。
	// target-1 は 1 ページ目の末尾 (添字 99) で読まれ、2 ページ目の先頭
	// (添字 100) にもう一度出る。
	insp := &headPushInspector{}
	for i := 0; i < 98; i++ {
		insp.scheduled = append(insp.scheduled, scheduledNoteSummary(t, fmt.Sprintf("other-%d", i), "other"))
	}
	insp.scheduled = append(insp.scheduled,
		scheduledNoteSummary(t, "target-1", "target"),
		scheduledNoteSummary(t, "other-last", "other"))
	c := queue.NewClient(&bucketDriver{insp: insp})

	require.NoError(t, c.ClearScheduledNote("target"))
	assert.Equal(t, []string{"target-1"}, insp.deleted)
}

// headPushInspector pushes a new unrelated job to the head before each
// delayed page, and fails DeleteTask for jobs that are already gone.
type headPushInspector struct {
	bucketInspector
	pushed int
}

func (i *headPushInspector) ListDelayedTasks(q string, page, pageSize int) ([]*driver.TaskSummary, error) {
	i.pushed++
	head := &driver.TaskSummary{ID: fmt.Sprintf("new-%d", i.pushed), Type: queue.TaskTypeDeliver}
	i.scheduled = append([]*driver.TaskSummary{head}, i.scheduled...)
	return i.bucketInspector.ListDelayedTasks(q, page, pageSize)
}

func (i *headPushInspector) DeleteTask(q, taskID string) error {
	for _, t := range i.scheduled {
		if t.ID == taskID {
			return i.bucketInspector.DeleteTask(q, taskID)
		}
	}
	return errors.New("job not found")
}
