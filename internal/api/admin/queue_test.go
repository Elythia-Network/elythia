package admin_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	apiadmin "github.com/elythia-network/elythia/internal/api/admin"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deliverPayloadJSON returns a marshaled queue.DeliverPayload with the given
// recipient inbox URL. Helper for *-delayed aggregation tests.
func deliverPayloadJSON(t *testing.T, inbox string) []byte {
	t.Helper()
	b, err := json.Marshal(queue.DeliverPayload{Inbox: inbox})
	require.NoError(t, err)
	return b
}

// inboxPayloadJSON returns a marshaled queue.InboxPayload whose Headers
// contain a minimal HTTP Signature header pointing at the given keyId URL.
// 他の field (algorithm / headers / signature) は dummy で埋める ―
// activitypub.ParseSignatureHeader はキー順序非依存で keyId だけ拾えれば成立。
func inboxPayloadJSON(t *testing.T, keyID string) []byte {
	t.Helper()
	sig := `keyId="` + keyID + `",algorithm="rsa-sha256",headers="(request-target) date host",signature="abc"`
	b, err := json.Marshal(queue.InboxPayload{
		Body:    []byte(`{}`),
		Headers: map[string]string{"Signature": sig},
	})
	require.NoError(t, err)
	return b
}

// inboxPayloadNoSigJSON returns a marshaled InboxPayload with no signature
// header — used to verify the aggregator silently skips it.
func inboxPayloadNoSigJSON(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(queue.InboxPayload{Body: []byte(`{}`)})
	require.NoError(t, err)
	return b
}

// assertHostCount asserts that got[i] == [host, count]. JSON decode yields
// numbers as float64, so this hides the conversion noise at call sites.
func assertHostCount(t *testing.T, got [][]any, i int, host string, count int) {
	t.Helper()
	require.Greater(t, len(got), i, "got len=%d, want index %d", len(got), i)
	assert.Equal(t, []any{host, float64(count)}, got[i],
		"got[%d] should be [%q, %d]", i, host, count)
}

// stubQueueInspector is a test double for admin.QueueInspector that returns
// configurable canned responses without touching Redis.
type stubQueueInspector struct {
	queues        []string
	info          map[string]*apiadmin.QueueInfoResult
	pending       map[string][]*apiadmin.QueueTaskSummary
	active        map[string][]*apiadmin.QueueTaskSummary
	scheduled     map[string][]*apiadmin.QueueTaskSummary
	retry         map[string][]*apiadmin.QueueTaskSummary
	completed     map[string][]*apiadmin.QueueTaskSummary
	failed        map[string][]*apiadmin.QueueTaskSummary
	task          map[string]*apiadmin.QueueTaskSummary
	logs          map[string][]string
	metrics       map[string]map[string]*apiadmin.QueueMetricsResult // [queue][kind]
	deleted       []string
	runCalls      []string
	deleteAllHits []string
	pauseCalls    []string
	resumeCalls   []string
	pauseErr      error
	queuesErr     error
	runErr        error
	deleteErr     error
	// filteredCalls / delayedCalls count ListScheduled+RetryTasks and
	// ListDelayedTasks calls.
	filteredCalls int
	delayedCalls  int
	// deleteFailIDs makes DeleteTask fail for these ids only (cron repeat
	// jobs that RemoveJob refuses).
	deleteFailIDs map[string]bool
}

func (s *stubQueueInspector) PauseQueue(q string) error {
	if s.pauseErr != nil {
		return s.pauseErr
	}
	s.pauseCalls = append(s.pauseCalls, q)
	return nil
}

func (s *stubQueueInspector) UnpauseQueue(q string) error {
	if s.pauseErr != nil {
		return s.pauseErr
	}
	s.resumeCalls = append(s.resumeCalls, q)
	return nil
}

func (s *stubQueueInspector) Queues() ([]string, error) { return s.queues, s.queuesErr }
func (s *stubQueueInspector) GetQueueInfo(q string) (*apiadmin.QueueInfoResult, error) {
	if info, ok := s.info[q]; ok {
		return info, nil
	}
	return nil, errors.New("not found")
}
func (s *stubQueueInspector) DeleteTask(q, id string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if s.deleteFailIDs[id] {
		return errors.New("cannot remove repeat job")
	}
	s.deleted = append(s.deleted, id)
	// 実 inspector 同様、削除した task を各 state list から除く (clearQueueState
	// の list→delete ループが list の縮小で終了することを担保する)。
	for _, m := range []map[string][]*apiadmin.QueueTaskSummary{
		s.pending, s.active, s.scheduled, s.retry, s.completed, s.failed,
	} {
		removeStubTask(m, q, id)
	}
	delete(s.task, id)
	return nil
}

// removeStubTask drops the task with id from m[q] (test helper).
func removeStubTask(m map[string][]*apiadmin.QueueTaskSummary, q, id string) {
	if m == nil {
		return
	}
	rows := m[q]
	out := rows[:0:0]
	for _, t := range rows {
		if t.ID != id {
			out = append(out, t)
		}
	}
	m[q] = out
}
func (s *stubQueueInspector) DeleteAllPendingTasks(q string) (int, error) {
	s.deleteAllHits = append(s.deleteAllHits, q)
	return 0, nil
}
func (s *stubQueueInspector) RunTask(_, id string) error {
	if s.runErr != nil {
		return s.runErr
	}
	s.runCalls = append(s.runCalls, id)
	return nil
}

// stubPage slices rows like the real inspector does: 1-indexed page, newest
// first as stored. 実 inspector と同じくページで切らないと、先頭ページより
// 後ろを扱う経路 (#3169) を試せない。
func stubPage(rows []*apiadmin.QueueTaskSummary, page, pageSize int) []*apiadmin.QueueTaskSummary {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 30
	}
	start := (page - 1) * pageSize
	if start >= len(rows) {
		return nil
	}
	return rows[start:min(start+pageSize, len(rows))]
}

func (s *stubQueueInspector) ListPendingTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	return stubPage(s.pending[q], page, pageSize), nil
}
func (s *stubQueueInspector) ListActiveTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	return stubPage(s.active[q], page, pageSize), nil
}
func (s *stubQueueInspector) ListScheduledTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	s.filteredCalls++
	return stubPage(s.scheduled[q], page, pageSize), nil
}
func (s *stubQueueInspector) ListRetryTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	s.filteredCalls++
	return stubPage(s.retry[q], page, pageSize), nil
}

// ListDelayedTasks は delayed バケット全体 = scheduled + retry を返す。
func (s *stubQueueInspector) ListDelayedTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	s.delayedCalls++
	all := append(append([]*apiadmin.QueueTaskSummary{}, s.scheduled[q]...), s.retry[q]...)
	return stubPage(all, page, pageSize), nil
}
func (s *stubQueueInspector) ListCompletedTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	return stubPage(s.completed[q], page, pageSize), nil
}
func (s *stubQueueInspector) ListFailedTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	return stubPage(s.failed[q], page, pageSize), nil
}
func (s *stubQueueInspector) GetTaskInfo(_, id string) (*apiadmin.QueueTaskSummary, error) {
	if t, ok := s.task[id]; ok {
		return t, nil
	}
	return nil, errors.New("not found")
}

// GetTaskLogs は既定で空。log を返すケースは個別テストが Logs を差し替える。
func (s *stubQueueInspector) GetTaskLogs(_, taskID string, _, _ int64) ([]string, int64, error) {
	if s.logs == nil {
		return []string{}, 0, nil
	}
	lines := s.logs[taskID]
	return lines, int64(len(lines)), nil
}

func (s *stubQueueInspector) QueueMetrics(q, kind string) (*apiadmin.QueueMetricsResult, error) {
	if perKind, ok := s.metrics[q]; ok {
		if m, ok := perKind[kind]; ok {
			return m, nil
		}
	}
	return &apiadmin.QueueMetricsResult{}, nil
}

// --- Queue --------------------------------------------------------------------

func TestQueueClear_WithInspector(t *testing.T) {
	// queue + state 指定で対象 queue 1 つだけが clear される (#929 A の strict 化後)。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{}
	h.SetQueueInspector(insp)
	// **pending も個別に消す。** 一括削除 (`DeleteAllPendingTasks`) は job type を
	// 見ないので、wait へ昇格した予約投稿やアカウント削除まで巻き込む。
	insp.pending = map[string][]*apiadmin.QueueTaskSummary{
		"deliver": {{ID: "p1", Type: queue.TaskTypeDeliver}},
	}
	rec := doPost(h.QueueClear, `{"queue":"deliver","state":"wait"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"p1"}, insp.deleted)
	assert.Empty(t, insp.deleteAllHits, "一括削除は使わないこと")
}

func TestQueueClear_MissingParams(t *testing.T) {
	// upstream Misskey TS paramDef で queue + state は required (#929 A)。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{}
	h.SetQueueInspector(insp)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueClear, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueClear, `{"queue":"deliver"}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueClear, `{"state":"wait"}`, adminUser).Code)
}

func TestQueueJobs_FilterByQueueAndState(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		queues: []string{"deliver"},
		pending: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{ID: "p1", Queue: "deliver", Type: "x", State: "pending"}},
		},
		active: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{ID: "a1", Queue: "deliver", Type: "x", State: "active"}},
		},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":"active"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "a1", rows[0]["id"])
}

func TestQueueJobs_MissingParams(t *testing.T) {
	// upstream Misskey TS paramDef で queue + state は required (#929 A)。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{}
	h.SetQueueInspector(insp)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueJobs, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueJobs, `{"queue":"deliver"}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueJobs, `{"state":"wait"}`, adminUser).Code)
}

func TestQueueShowJob_Found(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"tid1": {ID: "tid1", Queue: "deliver", Type: "x", State: "pending"},
		},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueShowJob, `{"queue":"deliver","jobId":"tid1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "tid1", got["id"])
	// **failedReason / returnValue は成功した job には出さない (#2689)。**
	// golden (upstream の宣言 schema 由来) は required としているが、upstream の
	// 実装は Bull の undefined をそのまま返すので JSON からは消える。frontend は
	// `v-if="job.failedReason != null"` / `job.returnValue != null` で出し分ける
	// ので、空文字や {} を常に出すと**成功した job にも赤い警告アイコン付きの
	// 空の "Failed reason" 行と空の "Return value" タブ**が出る。
	shapetest.AssertExcept(t, "QueueJob", got, "failedReason", "returnValue")
	_, hasFailedReason := got["failedReason"]
	assert.False(t, hasFailedReason, "失敗していない job に failedReason を出さない")
	_, hasReturnValue := got["returnValue"]
	assert.False(t, hasReturnValue, "戻り値が無い job に returnValue を出さない")
	// progress は Bull だと既定 0 (数値)。frontend は数値のときだけ % を出す。
	assert.EqualValues(t, 0, got["progress"])
}

// TestQueueShowJob_ProcessedBy は upstream QueueJob.processedBy (optional) を、
// ProcessedBy が非空のときだけ出し、空のときは省略することを確認する (#1552)。
func TestQueueShowJob_ProcessedBy(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"withpb": {ID: "withpb", Queue: "deliver", Type: "x", State: "completed", ProcessedBy: "worker-host-1"},
			"nopb":   {ID: "nopb", Queue: "deliver", Type: "x", State: "pending"},
		},
	})

	rec := doPost(h.QueueShowJob, `{"queue":"deliver","jobId":"withpb"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	shapetest.AssertExcept(t, "QueueJob", got, "failedReason", "returnValue")
	assert.Equal(t, "worker-host-1", got["processedBy"], "non-empty ProcessedBy must be output")

	rec2 := doPost(h.QueueShowJob, `{"queue":"deliver","jobId":"nopb"}`, adminUser)
	require.Equal(t, http.StatusOK, rec2.Code)
	var got2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &got2))
	_, has := got2["processedBy"]
	assert.False(t, has, "empty ProcessedBy must be omitted (optional field)")
}

func TestQueueShowJob_NotFoundWithInspector(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueShowJob, `{"queue":"deliver","jobId":"ghost"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQueueRemoveJob_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// driver の DeleteTask は不明 id で nil を返しうる (idempotent) ため、existence
	// 確認用に GetTaskInfo を事前 hit する (#929 B)。stub の task map に
	// 入れて GetTaskInfo を pass させる。
	insp := &stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"tid1": {ID: "tid1", Queue: "deliver"},
		},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueRemoveJob, `{"queue":"deliver","jobId":"tid1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"tid1"}, insp.deleted)
}

func TestQueueRemoveJob_NotFound(t *testing.T) {
	// task map が空 = GetTaskInfo が "not found" を返すケース。idempotent な
	// DeleteTask に到達せず precheck で 404 (#929 B)。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueRemoveJob, `{"queue":"deliver","jobId":"x"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQueueRemoveJob_DeleteTaskError(t *testing.T) {
	// precheck は pass するが DeleteTask 自体が失敗する race condition を guard。
	// (例: precheck と DeleteTask の間に他 admin が削除済) 引き続き 404 を返す。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"tid1": {ID: "tid1", Queue: "deliver"},
		},
		deleteErr: errors.New("not found"),
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueRemoveJob, `{"queue":"deliver","jobId":"tid1"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQueueRetryJob_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"tid1": {ID: "tid1", Queue: "deliver"},
		},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueRetryJob, `{"queue":"deliver","jobId":"tid1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"tid1"}, insp.runCalls)
}

func TestQueueRetryJob_NotFound(t *testing.T) {
	// 不明 id は GetTaskInfo precheck で 404。RunTask は idempotent (#929 B)。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueRetryJob, `{"queue":"deliver","jobId":"x"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQueueRetryJob_RunTaskError(t *testing.T) {
	// precheck pass 後に RunTask 自体が失敗する race condition (precheck と
	// RunTask の間に別 admin が削除した等)。RemoveJob と symmetric。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"tid1": {ID: "tid1", Queue: "deliver"},
		},
		runErr: errors.New("not found"),
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueRetryJob, `{"queue":"deliver","jobId":"tid1"}`, adminUser)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestQueuePromoteJobs_RunsScheduledAndRetry(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{ID: "s1"}, {ID: "s2"}},
		},
		retry: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{ID: "r1"}},
		},
	}
	h.SetQueueInspector(insp)

	// queue は paramDef required (#929 A)。
	rec := doPost(h.QueuePromoteJobs, `{"queue":"deliver"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.EqualValues(t, 3, got["promoted"])
	assert.ElementsMatch(t, []string{"s1", "s2", "r1"}, insp.runCalls)
}

func TestQueueQueueStats_WithInspector(t *testing.T) {
	// frontend admin/job-queue.vue が期待する Misskey Bull shape
	// ({name, counts:{active,delayed,waiting,...}, metrics:..., db:...})
	// を返すことを検証する (#929 A の strict 化後は単一 queue のみ)。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		info: map[string]*apiadmin.QueueInfoResult{
			"deliver": {Queue: "deliver", Size: 5, Pending: 3, Active: 1, Completed: 10, Failed: 2, Scheduled: 0, Retry: 1},
		},
		metrics: map[string]map[string]*apiadmin.QueueMetricsResult{
			"deliver": {
				"completed": {Count: 10, Data: []int64{4, 3, 2, 1}},
				"failed":    {Count: 2, Data: []int64{0, 1, 0, 1}},
			},
		},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueQueueStats, `{"queue":"deliver"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "deliver", got["name"])
	counts, ok := got["counts"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1, counts["active"])
	assert.EqualValues(t, 3, counts["waiting"])
	// scheduled(0) + retry(1) がdelayedに集約される。
	assert.EqualValues(t, 1, counts["delayed"])
	shapetest.Assert(t, "QueueCount", counts) // L3 (#1302)

	// metrics shape: data 配列と count が driver から伝播。
	metrics, ok := got["metrics"].(map[string]any)
	require.True(t, ok)
	completed, ok := metrics["completed"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 10, completed["count"])
	completedData, ok := completed["data"].([]any)
	require.True(t, ok)
	assert.Len(t, completedData, 4)
	assert.EqualValues(t, 4, completedData[0]) // newest-first
}

func TestQueueQueueStats_NoMetrics_FallsBackToCumulative(t *testing.T) {
	// driver が QueueMetrics を実装していても Data 空 / Count 0 を
	// 返すケース (mkq で WithJobMetrics 無効、または time-series 非対応 driver)
	// では info.Completed / info.Failed の累積値を count にフォール
	// バックさせ、data は空配列で安定 shape を維持する。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		queues: []string{"deliver"},
		info: map[string]*apiadmin.QueueInfoResult{
			"deliver": {Queue: "deliver", Completed: 7, Failed: 3},
		},
		// metrics map を空にすると stub は zero-valued QueueMetricsResult を返す
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueQueueStats, `{"queue":"deliver"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	metrics, ok := got["metrics"].(map[string]any)
	require.True(t, ok)
	completed, ok := metrics["completed"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 7, completed["count"])
	completedData, ok := completed["data"].([]any)
	require.True(t, ok)
	assert.Empty(t, completedData)
	failed, ok := metrics["failed"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 3, failed["count"])
}

func TestQueueQueueStats_SingleQueueQuery(t *testing.T) {
	// frontend の fetchCurrentQueue は {queue: "deliver"} を投げて
	// 単一 queue の shape を受ける。req.Queue が与えられた場合はその
	// queue 1 つ分を返す挙動を検証する。
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		queues: []string{"deliver", "push"},
		info: map[string]*apiadmin.QueueInfoResult{
			"deliver": {Queue: "deliver", Size: 5, Pending: 3, Active: 1},
			"push":    {Queue: "push"},
		},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueQueueStats, `{"queue":"deliver"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "deliver", got["name"])
	counts, ok := got["counts"].(map[string]any)
	require.True(t, ok)
	assert.EqualValues(t, 1, counts["active"])
}

// TestQueueDeliverDelayed_AggregatesByHost verifies the upstream Misskey TS
// contract: [[host, count], ...] tuples sorted by count desc. Mixed
// scheduled+retry tasks targeting the same host must be merged into one
// bucket. Tasks whose payload cannot yield a host (invalid JSON / unparsable
// URL / empty Inbox) must be skipped silently. (#1179)
func TestQueueDeliverDelayed_AggregatesByHost(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {
				{ID: "s1", Payload: deliverPayloadJSON(t, "https://a.example/inbox")},
				{ID: "s2", Payload: deliverPayloadJSON(t, "https://b.example/users/foo/inbox")},
				{ID: "s3", Payload: deliverPayloadJSON(t, "https://a.example/users/x/inbox")},
				// 不正 payload は skip
				{ID: "bad1", Payload: []byte("not json")},
				{ID: "bad2", Payload: deliverPayloadJSON(t, "::not a url::")},
				{ID: "bad3", Payload: deliverPayloadJSON(t, "")},
			},
		},
		retry: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {
				{ID: "r1", Payload: deliverPayloadJSON(t, "https://b.example/inbox")},
				{ID: "r2", Payload: deliverPayloadJSON(t, "https://c.example/inbox")},
				{ID: "r3", Payload: deliverPayloadJSON(t, "https://a.example/u2/inbox")},
			},
		},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueDeliverDelayed, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got [][]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// a.example: 3 (s1+s3+r3), b.example: 2 (s2+r1), c.example: 1 (r2)
	// count desc, tie 無し
	require.Len(t, got, 3)
	assertHostCount(t, got, 0, "a.example", 3)
	assertHostCount(t, got, 1, "b.example", 2)
	assertHostCount(t, got, 2, "c.example", 1)
}

// TestQueueDeliverDelayed_EmptyReturnsEmptyArray ensures the response is
// `[]` (not `null`) when no tasks exist, so frontend.reduce() yields 0.
func TestQueueDeliverDelayed_EmptyReturnsEmptyArray(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{})
	rec := doPost(h.QueueDeliverDelayed, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// TestQueueDeliverDelayed_NoInspectorReturnsEmpty: queueInspector が無い
// (queue 機能無効構成) でも 200 + `[]` を返す。frontend が render crash しない
// よう defensive に動く。
func TestQueueDeliverDelayed_NoInspectorReturnsEmpty(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.QueueDeliverDelayed, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

// TestQueueInboxDelayed_AggregatesByHostFromSignature verifies inbox tasks
// resolve host via HTTP Signature header (keyId URL), matching upstream
// Misskey TS の `new URL(job.data.signature.keyId).host`. (#1179)
func TestQueueInboxDelayed_AggregatesByHostFromSignature(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{
			"inbox": {
				{ID: "s1", Payload: inboxPayloadJSON(t, "https://a.example/users/alice#main-key")},
				{ID: "s2", Payload: inboxPayloadJSON(t, "https://a.example/users/bob#main-key")},
				// signature 無 → skip
				{ID: "noSig", Payload: inboxPayloadNoSigJSON(t)},
				// 不正 JSON → skip
				{ID: "bad", Payload: []byte("not json")},
			},
		},
		retry: map[string][]*apiadmin.QueueTaskSummary{
			"inbox": {
				{ID: "r1", Payload: inboxPayloadJSON(t, "https://b.example/users/eve#main-key")},
			},
		},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueInboxDelayed, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	var got [][]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// a.example: 2, b.example: 1
	require.Len(t, got, 2)
	assertHostCount(t, got, 0, "a.example", 2)
	assertHostCount(t, got, 1, "b.example", 1)
}

// pagedInspector is a QueueInspector stub that always returns full pages,
// emulating an inspector that has so many tasks the cursor never
// reaches the end (or, worse, a misbehaving inspector that ignores empty
// state). Used to verify the page cap defense in fetchAllDelayedTasks.
//
// 他の QueueInspector method は embedded stubQueueInspector の zero-value に
// fall through する (空 map / 空 slice 返却)。cap test では fetchAllDelayedTasks
// 経由の delayed list call しか走らないので問題なし。
type pagedInspector struct {
	stubQueueInspector
	payload []byte
	calls   int
}

func (p *pagedInspector) ListDelayedTasks(_ string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	p.calls++
	rows := make([]*apiadmin.QueueTaskSummary, 0, pageSize)
	for i := 0; i < pageSize; i++ {
		// ID は重複除去に掛からないよう、ページごとに変える。
		rows = append(rows, &apiadmin.QueueTaskSummary{ID: fmt.Sprintf("%d-%d", page, i), Payload: p.payload})
	}
	return rows, nil
}

// TestQueueDeliverDelayed_PageCapBoundsRunaway: inspector が常に full page を
// 返してくる misbehavior / pathological case で、handler が無限ループせず
// `delayedTasksMaxPages` で打ち切ることを担保する。defense-in-depth の
// 動作 test (#1179 review #1 + #2)。
func TestQueueDeliverDelayed_PageCapBoundsRunaway(t *testing.T) {
	// 本物の cap (1000 pages) で test すると 100K item 走査で 1 unit test に
	// しては重い。test 中だけ cap を小さくして cap の挙動だけを検証する。
	// `pageCap` という名前で builtin `cap()` の shadow を避ける。
	const pageCap = 3
	prev := apiadmin.SetDelayedTasksMaxPages(pageCap)
	t.Cleanup(func() { apiadmin.SetDelayedTasksMaxPages(prev) })

	h, _, _, _ := newTestHandler(t)
	insp := &pagedInspector{payload: deliverPayloadJSON(t, "https://always.example/inbox")}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueDeliverDelayed, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var got [][]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	// 1 host × pageCap (3) × pageSize = 3 × DelayedTasksFetchPageSize 件で
	// 打ち切られる。
	require.Len(t, got, 1)
	assertHostCount(t, got, 0, "always.example", pageCap*apiadmin.DelayedTasksFetchPageSize)
	// inspector への ListDelayedTasks 呼び出しは cap 回数で打ち切られる。
	assert.Equal(t, pageCap, insp.calls,
		"page iteration must stop at delayedTasksMaxPages")
}

// TestQueueDeliverDelayed_StableSortOnTie: 同 count の host は host 名 asc で
// 安定 sort される (frontend で順序が flicker しないように)。
func TestQueueDeliverDelayed_StableSortOnTie(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {
				{ID: "1", Payload: deliverPayloadJSON(t, "https://c.example/inbox")},
				{ID: "2", Payload: deliverPayloadJSON(t, "https://a.example/inbox")},
				{ID: "3", Payload: deliverPayloadJSON(t, "https://b.example/inbox")},
			},
		},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueDeliverDelayed, `{}`, adminUser)
	var got [][]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 3)
	// all count = 1, host 名 asc で並ぶ
	assert.Equal(t, "a.example", got[0][0])
	assert.Equal(t, "b.example", got[1][0])
	assert.Equal(t, "c.example", got[2][0])
}

// QueueJobs は単一 queue でも limit を超えない件数しか返さないことを確認する。
func TestQueueJobs_SingleQueueCappedAtLimit(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		pending: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{ID: "d1"}, {ID: "d2"}, {ID: "d3"}},
		},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":"wait","limit":2}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2, "single-queue output must respect limit")
}

// pagingQueueInspector serves completed / failed / delayed with real
// 1-indexed paging, newest first as the driver returns them.
type pagingQueueInspector struct {
	*stubQueueInspector
	completed, failed, delayed []*apiadmin.QueueTaskSummary
}

func pageOf(rows []*apiadmin.QueueTaskSummary, page, size int) []*apiadmin.QueueTaskSummary {
	start := (page - 1) * size
	if start >= len(rows) {
		return nil
	}
	// コピーして返す。呼び出し側がループ中に削除すると、別名のままでは要素がずれる。
	return append([]*apiadmin.QueueTaskSummary(nil), rows[start:min(start+size, len(rows))]...)
}

func (p *pagingQueueInspector) ListCompletedTasks(_ string, page, size int) ([]*apiadmin.QueueTaskSummary, error) {
	return pageOf(p.completed, page, size), nil
}
func (p *pagingQueueInspector) ListFailedTasks(_ string, page, size int) ([]*apiadmin.QueueTaskSummary, error) {
	return pageOf(p.failed, page, size), nil
}
func (p *pagingQueueInspector) ListDelayedTasks(_ string, page, size int) ([]*apiadmin.QueueTaskSummary, error) {
	return pageOf(p.delayed, page, size), nil
}

func summaries(prefix string, n int) []*apiadmin.QueueTaskSummary {
	out := make([]*apiadmin.QueueTaskSummary, 0, n)
	for i := range n {
		out = append(out, &apiadmin.QueueTaskSummary{ID: prefix + strconv.Itoa(i), Queue: "deliver"})
	}
	return out
}

func jobIDs(t *testing.T, body []byte) []string {
	t.Helper()
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(body, &rows))
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r["id"].(string))
	}
	return ids
}

// **limit 未指定なら upstream と同じ形で返す (#3167)。** upstream は
// `getJobs(types, 0, 100)` = state ごとに新しい順で最大 101 件を、state の指定順に
// 連結する。以前は既定の 30 件が合計に効き、「すべて」タブ (completed が先頭) が
// 完了済みだけで埋まって失敗が 1 件も見えなかった。
func TestQueueJobs_NoLimitReturnsPerStateLikeUpstream(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&pagingQueueInspector{
		stubQueueInspector: &stubQueueInspector{},
		completed:          summaries("c", 150),
		failed:             summaries("f", 5),
	})

	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":["completed","failed"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	ids := jobIDs(t, rec.Body.Bytes())

	require.Len(t, ids, 101+5, "state ごとに最大 101 件 (合計では切らない)")
	assert.Equal(t, "c0", ids[0], "driver が返した順 (新しい順) を保つ")
	assert.Equal(t, "c100", ids[100], "101 件目まで取る (end を含む)")
	assert.Equal(t, []string{"f0", "f1", "f2", "f3", "f4"}, ids[101:], "後ろの state も欠けない")
}

// delayed は Scheduled / Retry を後から混ぜず、バケット全体の並びをそのまま使う。
func TestQueueJobs_DelayedUsesWholeBucket(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stub := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "s0"}}},
		retry:     map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "r0"}}},
	}
	h.SetQueueInspector(&pagingQueueInspector{
		stubQueueInspector: stub,
		// retry で delayed に戻った job の方が発火予定が遅い、という並び。
		delayed: []*apiadmin.QueueTaskSummary{{ID: "r0"}, {ID: "s0"}},
	})

	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":["delayed"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"r0", "s0"}, jobIDs(t, rec.Body.Bytes()))
}

// --- thin nil-inspector smoke tests ---
//
// nil queueInspector 経路 (newTestHandler は wire しない) で expected status
// を返すことを担保。詳細テストは inspector を wire して実挙動を検証するので、
// 本群は nil 分岐の coverage 補完。

func TestQueueClear(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// queue/state 欠落は 400、required 揃いの正規 bind は nil inspector で 204
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueClear, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusNoContent, doPost(h.QueueClear, `{"queue":"deliver","state":"wait"}`, adminUser).Code)
}

func TestQueueDeliverDelayed(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.QueueDeliverDelayed, `{}`, adminUser).Code)
}

func TestQueueInboxDelayed(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.QueueInboxDelayed, `{}`, adminUser).Code)
}

func TestQueueJobs(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// queue/state 欠落は 400、required 揃いの正規 bind は nil inspector で 200 (空配列)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueJobs, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusOK, doPost(h.QueueJobs, `{"queue":"deliver","state":"wait"}`, adminUser).Code)
}

func TestQueuePromoteJobs(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueuePromoteJobs, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusNoContent, doPost(h.QueuePromoteJobs, `{"queue":"deliver"}`, adminUser).Code)
}

func TestQueueQueueStats(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueQueueStats, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusOK, doPost(h.QueueQueueStats, `{"queue":"deliver"}`, adminUser).Code)
}

func TestQueueQueues(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusOK, doPost(h.QueueQueues, `{}`, adminUser).Code)
}

func TestQueueRemoveJob(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// queue/id 欠落は 400
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueRemoveJob, `{}`, adminUser).Code)
	// inspector 未注入は 204
	assert.Equal(t, http.StatusNoContent, doPost(h.QueueRemoveJob, `{"queue":"deliver","jobId":"x"}`, adminUser).Code)
}

func TestQueueRetryJob(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueRetryJob, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusNoContent, doPost(h.QueueRetryJob, `{"queue":"deliver","jobId":"x"}`, adminUser).Code)
}

func TestQueueShowJob(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// queue/id 欠落は 400、inspector 未注入 + 正規 bind は 404
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueShowJob, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusNotFound, doPost(h.QueueShowJob, `{"queue":"deliver","jobId":"x"}`, adminUser).Code)
}

func TestQueueShowJobLogs(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	// queue/jobId 欠落は 400 (upstream required)。
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueShowJobLogs, `{}`, adminUser).Code)
	// 正規 bind では常に空配列を返す (mkq は per-task ログ非保持)。
	rec := doPost(h.QueueShowJobLogs, `{"queue":"deliver","jobId":"x"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]\n", rec.Body.String())
}

func TestQueueStatsAdmin(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.QueueStats, `{}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "deliver")
}

// TestQueueStats_DelayedIncludesScheduledAndRetry guards #654: Misskey
// frontend の WidgetJobQueue は Bull 用語の delayed をグラフ化するが、
// driver では Scheduled (未来実行予定) と Retry (失敗後再試行待ち) の
// 2 つに分かれる。delayed = Scheduled + Retry を返さないと再試行待ちが
// dashboard に出ないため、この合算が REST API でも維持されることを guard。
func TestQueueStats_DelayedIncludesScheduledAndRetry(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		info: map[string]*apiadmin.QueueInfoResult{
			"deliver": {Queue: "deliver", Active: 1, Pending: 2, Scheduled: 3, Retry: 4},
			"inbox":   {Queue: "inbox", Active: 0, Pending: 0, Scheduled: 5, Retry: 6},
		},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueStats, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp map[string]map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// deliver: Scheduled 3 + Retry 4 = 7
	assert.EqualValues(t, 1, resp["deliver"]["active"])
	assert.EqualValues(t, 2, resp["deliver"]["waiting"])
	assert.EqualValues(t, 7, resp["deliver"]["delayed"], "delayed must be Scheduled+Retry")

	// inbox: Scheduled 5 + Retry 6 = 11
	assert.EqualValues(t, 11, resp["inbox"]["delayed"], "delayed must be Scheduled+Retry")
}

// QueueJobs は mkq の finished-job 保持から completed / failed を一覧する (#1396)。
// 旧実装は completed/failed を nil 固定にしており、busy queue でも
// All / Completed タブが空になっていた。
func TestQueueJobs_ListsCompletedAndFailed(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		completed: map[string][]*apiadmin.QueueTaskSummary{
			"inbox": {{ID: "c1", Queue: "inbox", Type: "inbox:process", State: "completed"}},
		},
		failed: map[string][]*apiadmin.QueueTaskSummary{
			"inbox": {{ID: "f1", Queue: "inbox", Type: "inbox:process", State: "failed"}},
		},
	}
	h.SetQueueInspector(insp)

	// state=completed → 完了ジョブが返る。
	rec := doPost(h.QueueJobs, `{"queue":"inbox","state":["completed"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "c1", got[0]["id"])

	// state=all (frontend の All タブ) → completed + failed が両方含まれる。
	rec2 := doPost(h.QueueJobs, `{"queue":"inbox","state":["completed","failed","active","delayed","wait"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec2.Code)
	var got2 []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &got2))
	ids := map[string]bool{}
	for _, j := range got2 {
		ids[j["id"].(string)] = true
	}
	assert.True(t, ids["c1"], "completed job present in all-tab")
	assert.True(t, ids["f1"], "failed job present in all-tab")
}

// ジョブ詳細の timestamp は job 作成/処理/完了の実時刻から出す (#1398)。
// 旧実装は timestamp に NextProcessAt、processedOn に LastFailedAt を使い、
// completed/failed ジョブで 0 (= 1970/1/1) になっていた。
func TestQueueJobs_TimestampsFromJobState(t *testing.T) {
	created := time.UnixMilli(1_700_000_000_000)
	processed := time.UnixMilli(1_700_000_001_000)
	finished := time.UnixMilli(1_700_000_002_000)
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		completed: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{
				ID: "c1", Queue: "deliver", Type: "x", State: "completed",
				EnqueuedAt: created, ProcessedAt: processed, CompletedAt: finished,
			}},
		},
		pending: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{ID: "p1", Queue: "deliver", Type: "x", State: "pending", EnqueuedAt: created}},
		},
		failed: map[string][]*apiadmin.QueueTaskSummary{
			// 失敗ジョブは CompletedAt 無し。finishedOn は LastFailedAt から出す。
			"deliver": {{
				ID: "x1", Queue: "deliver", Type: "x", State: "failed",
				EnqueuedAt: created, ProcessedAt: processed, LastFailedAt: finished, LastErr: "boom",
			}},
		},
	}
	h.SetQueueInspector(insp)

	// completed: 3 つの時刻が実 ms で出る。
	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":["completed"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.EqualValues(t, created.UnixMilli(), got[0]["timestamp"])
	assert.EqualValues(t, processed.UnixMilli(), got[0]["processedOn"])
	assert.EqualValues(t, finished.UnixMilli(), got[0]["finishedOn"])

	// pending: processedOn / finishedOn は省略され 1970 にならない。timestamp は present。
	rec2 := doPost(h.QueueJobs, `{"queue":"deliver","state":["wait"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec2.Code)
	var got2 []map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &got2))
	require.Len(t, got2, 1)
	assert.EqualValues(t, created.UnixMilli(), got2[0]["timestamp"])
	_, hasProcessed := got2[0]["processedOn"]
	assert.False(t, hasProcessed, "未処理ジョブは processedOn を省略する")
	_, hasFinished := got2[0]["finishedOn"]
	assert.False(t, hasFinished, "未完了ジョブは finishedOn を省略する")

	// failed: CompletedAt 無しでも finishedOn は LastFailedAt から出る。
	rec3 := doPost(h.QueueJobs, `{"queue":"deliver","state":["failed"]}`, adminUser)
	require.Equal(t, http.StatusOK, rec3.Code)
	var got3 []map[string]any
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &got3))
	require.Len(t, got3, 1)
	assert.EqualValues(t, processed.UnixMilli(), got3[0]["processedOn"])
	assert.EqualValues(t, finished.UnixMilli(), got3[0]["finishedOn"])
}

// --- #H-PR8e: queue parity additions ---

// remove/retry/show-job は upstream の jobId キーを受け付ける (旧 id も互換)。
func TestQueueJobHandlers_AcceptJobId(t *testing.T) {
	mkHandler := func() (*apiadmin.Handler, *stubQueueInspector) {
		h, _, _, _ := newTestHandler(t)
		insp := &stubQueueInspector{task: map[string]*apiadmin.QueueTaskSummary{
			"j1": {ID: "j1", Queue: "deliver", Type: "x"},
		}}
		h.SetQueueInspector(insp)
		return h, insp
	}

	h, insp := mkHandler()
	rec := doPost(h.QueueRemoveJob, `{"queue":"deliver","jobId":"j1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Contains(t, insp.deleted, "j1")

	h, insp = mkHandler()
	rec = doPost(h.QueueRetryJob, `{"queue":"deliver","jobId":"j1"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Contains(t, insp.runCalls, "j1")

	h, _ = mkHandler()
	rec = doPost(h.QueueShowJob, `{"queue":"deliver","jobId":"j1"}`, adminUser)
	assert.Equal(t, http.StatusOK, rec.Code)

	// jobId 欠落 (queue だけ) は 400。
	h, _ = mkHandler()
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueRemoveJob, `{"queue":"deliver"}`, adminUser).Code)
}

// queue/stats は deliver/inbox/db/objectStorage の 4 キーで、各値が
// waiting/active/completed/failed/delayed を持つ (upstream QueueCount)。
func TestQueueStats_FullShape(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{info: map[string]*apiadmin.QueueInfoResult{
		"deliver": {Queue: "deliver", Active: 1, Pending: 2, Completed: 3, Failed: 4, Scheduled: 5, Retry: 6},
		"inbox":   {Queue: "inbox", Active: 7, Pending: 8, Completed: 9, Failed: 10, Scheduled: 1, Retry: 1},
	}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueStats, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	for _, k := range []string{"deliver", "inbox", "db", "objectStorage"} {
		require.Contains(t, got, k)
		for _, f := range []string{"waiting", "active", "completed", "failed", "delayed"} {
			_, ok := got[k][f]
			assert.True(t, ok, k+"."+f+" present")
		}
	}
	assert.Equal(t, float64(3), got["deliver"]["completed"])
	assert.Equal(t, float64(4), got["deliver"]["failed"])
	assert.Equal(t, float64(11), got["deliver"]["delayed"]) // scheduled(5)+retry(6)
	// db/objectStorage はゼロ埋め。
	assert.Equal(t, float64(0), got["db"]["completed"])
}

// queue/clear は state を尊重する。failed 指定は pending を drain せず failed
// task を DeleteTask で消す。
func TestQueueClear_ByFailedState(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{failed: map[string][]*apiadmin.QueueTaskSummary{
		"deliver": {{ID: "f1"}, {ID: "f2"}},
	}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueClear, `{"queue":"deliver","state":"failed"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.ElementsMatch(t, []string{"f1", "f2"}, insp.deleted)
	assert.Empty(t, insp.deleteAllHits, "failed 指定では pending を drain しない")
}

// state='*' は全 state を消す。**pending も個別に消す** (一括削除は job type を
// 見ないので、利用者の予定を巻き込む)。
func TestQueueClear_AllStates(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		pending:   map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "p1", Type: queue.TaskTypeDeliver}}},
		failed:    map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "f1"}}},
		completed: map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "c1"}}},
		scheduled: map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "s1"}}},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueClear, `{"queue":"deliver","state":"*"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, insp.deleteAllHits, "一括削除は使わないこと")
	assert.ElementsMatch(t, []string{"p1", "f1", "c1", "s1"}, insp.deleted)
}

// queue-stats の metrics.completed/failed は meta オブジェクトを持つ。
func TestQueueQueueStats_MetricsHaveMeta(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{info: map[string]*apiadmin.QueueInfoResult{
		"deliver": {Queue: "deliver", Completed: 5, Failed: 2},
	}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueQueueStats, `{"queue":"deliver"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	metrics := got["metrics"].(map[string]any)
	completed := metrics["completed"].(map[string]any)
	meta, ok := completed["meta"].(map[string]any)
	require.True(t, ok, "metrics.completed.meta が存在する")
	for _, f := range []string{"count", "prevTS", "prevCount"} {
		_, ok := meta[f]
		assert.True(t, ok, "meta."+f)
	}
}

// queue/jobs は search で job を絞り込む (JSON 表現に全 term を含むもの)。
func TestQueueJobs_SearchFilter(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{pending: map[string][]*apiadmin.QueueTaskSummary{
		"deliver": {
			{ID: "p1", Queue: "deliver", Type: "deliverNote", Payload: []byte(`{"host":"alpha.example"}`)},
			{ID: "p2", Queue: "deliver", Type: "deliverNote", Payload: []byte(`{"host":"beta.example"}`)},
		},
	}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":"wait","search":"alpha"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "p1", rows[0]["id"])
}

// jobId の代わりの id は読まない (#3865)。本家の paramDef と同じく jobId が要る。
func TestQueueJobOps_IDIsNotRead(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{task: map[string]*apiadmin.QueueTaskSummary{
		"j1": {ID: "j1", Queue: "deliver"},
	}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueRemoveJob, `{"queue":"deliver","id":"j1"}`, adminUser)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.NotContains(t, insp.deleted, "j1")
}

// 空の jobId は、本家と同じく INVALID_PARAM にせず、見つからない job として扱う
// (#3865。本家は getJob("") で見つからない)。見つからない job への応答 (404) は
// 本家 (remove-job・retry-job は 204、show-job は 500) と違う
// (docs/divergence/approximations.md)。
func TestQueueJobOps_EmptyJobIDIsNotFound(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{task: map[string]*apiadmin.QueueTaskSummary{
		"": {ID: "", Queue: "deliver"},
	}}
	h.SetQueueInspector(insp)
	body := `{"queue":"deliver","jobId":""}`
	assert.Equal(t, http.StatusNotFound, doPost(h.QueueRemoveJob, body, adminUser).Code)
	assert.Equal(t, http.StatusNotFound, doPost(h.QueueRetryJob, body, adminUser).Code)
	assert.Equal(t, http.StatusNotFound, doPost(h.QueueShowJob, body, adminUser).Code)
	rec := doPost(h.QueueShowJobLogs, body, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `[]`, rec.Body.String())
	assert.Empty(t, insp.deleted)
}

// clear state=delayed は scheduled + retry の両方を消す。
func TestQueueClear_ByDelayedState(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "s1"}}},
		retry:     map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "r1"}}},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueClear, `{"queue":"deliver","state":"delayed"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.ElementsMatch(t, []string{"s1", "r1"}, insp.deleted)
	assert.Empty(t, insp.deleteAllHits)
}

// clear state=active 等は no-op (pending drain も DeleteTask もしない)。
func TestQueueClear_NoOpStates(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		failed: map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "f1"}}},
	}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueClear, `{"queue":"deliver","state":"active"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, insp.deleted, "active は何も消さない")
	assert.Empty(t, insp.deleteAllHits)
}

// clearQueueState の deleteAll は DeleteTask が失敗 (list 縮小しない) しても
// progressed=false で 1 反復で抜け、無限ループしない。
func TestQueueClear_StopsWhenDeleteFails(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		failed:    map[string][]*apiadmin.QueueTaskSummary{"deliver": {{ID: "f1"}}},
		deleteErr: errors.New("locked"),
	}
	h.SetQueueInspector(insp)
	done := make(chan struct{})
	go func() {
		doPost(h.QueueClear, `{"queue":"deliver","state":"failed"}`, adminUser)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("clearQueueState did not terminate (infinite loop)")
	}
}

// search は複数 term の AND マッチで、どれも含まない job は落とす。
func TestQueueJobs_SearchMultiTermAndZeroMatch(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{pending: map[string][]*apiadmin.QueueTaskSummary{
		"deliver": {
			{ID: "p1", Queue: "deliver", Type: "x", Payload: []byte(`{"host":"alpha.example","kind":"urgent"}`)},
			{ID: "p2", Queue: "deliver", Type: "x", Payload: []byte(`{"host":"alpha.example","kind":"calm"}`)},
		},
	}}
	h.SetQueueInspector(insp)

	// "alpha urgent" は両 term を含む p1 のみ (p2 は urgent を含まない)。
	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":"wait","search":"alpha urgent"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	assert.Equal(t, "p1", rows[0]["id"])

	// マッチ無しは空配列。
	rec = doPost(h.QueueJobs, `{"queue":"deliver","state":"wait","search":"zzznomatch"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Empty(t, rows)
}

// search は最大 100 件返す (upstream RETURN_LIMIT、非 search の既定 limit 30 で
// 頭打ちにならない)。
func TestQueueJobs_SearchReturnsUpTo100(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	tasks := make([]*apiadmin.QueueTaskSummary, 0, 150)
	for i := 0; i < 150; i++ {
		tasks = append(tasks, &apiadmin.QueueTaskSummary{
			ID: "p" + strconv.Itoa(i), Queue: "deliver", Type: "deliverNote",
			Payload: []byte(`{"host":"match.example"}`),
		})
	}
	insp := &stubQueueInspector{pending: map[string][]*apiadmin.QueueTaskSummary{"deliver": tasks}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":"wait","search":"match"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 100, "search は最大 100 件 (RETURN_LIMIT)")
}

// #2069 (upstream #17436): admin/queue/pause・resume。
func TestQueuePause_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{queues: []string{"deliver", "inbox"}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueuePause, `{"queue":"deliver"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"deliver"}, insp.pauseCalls)
	assert.Empty(t, insp.resumeCalls)
}

func TestQueueResume_Success(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{queues: []string{"deliver", "inbox"}}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueueResume, `{"queue":"inbox"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"inbox"}, insp.resumeCalls)
	assert.Empty(t, insp.pauseCalls)
}

func TestQueuePauseResume_Validation(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{queues: []string{"deliver"}}
	h.SetQueueInspector(insp)
	// queue 欠落 → 400。
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueuePause, `{}`, adminUser).Code)
	// QUEUE_TYPES 外 → 400。
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueuePause, `{"queue":"bogus"}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueueResume, `{"queue":"bogus"}`, adminUser).Code)
	assert.Empty(t, insp.pauseCalls)
	assert.Empty(t, insp.resumeCalls)
}

// #2069 H1: QUEUE_TYPES enum だが mk-go が運用しない queue (Queues() に無い) は
// no-op 204 (frontend が全 QUEUE_TYPES タブを出すため 500 にしない)。
func TestQueuePause_UnmanagedQueueNoOp(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{queues: []string{"deliver", "inbox"}}
	h.SetQueueInspector(insp)
	// "system" は enum だが managed でない → 204、PauseQueue は呼ばれない。
	rec := doPost(h.QueuePause, `{"queue":"system"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, insp.pauseCalls)
}

func TestQueuePause_InspectorError(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{queues: []string{"deliver"}, pauseErr: assert.AnError}
	h.SetQueueInspector(insp)
	rec := doPost(h.QueuePause, `{"queue":"deliver"}`, adminUser)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestQueuePause_NoInspector(t *testing.T) {
	// queueInspector 未配線時は no-op 204 (QueueClear と同パターン)。
	h, _, _, _ := newTestHandler(t)
	rec := doPost(h.QueuePause, `{"queue":"deliver"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
}

// stubQueueRuntime は mk-go 独自 runtime block (#2277) の provider stub。
type stubQueueRuntime struct {
	known map[string]apiadmin.QueueRuntime
}

func (s *stubQueueRuntime) QueueRuntime(q string) (apiadmin.QueueRuntime, bool) {
	rt, ok := s.known[q]
	return rt, ok
}

// runtime provider を配線すると admin/queue/queues の各 queue に
// worker 数 / auto-scale 範囲 / 遅延 / scale 履歴が additive に載ること。
func TestQueueQueues_IncludesRuntimeBlock(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		queues: []string{"deliver"},
		info: map[string]*apiadmin.QueueInfoResult{
			"deliver": {Queue: "deliver", Active: 2, Pending: 3},
		},
	}
	h.SetQueueInspector(insp)
	h.SetQueueRuntimeProvider(&stubQueueRuntime{known: map[string]apiadmin.QueueRuntime{
		"deliver": {
			Workers: 6, MinWorkers: 4, MaxWorkers: 32, AutoScale: true,
			DispatchWaitMs: apiadmin.Quantiles{Count: 10, P50: 1.5, P95: 9, Max: 12},
			ProcessingMs:   apiadmin.Quantiles{Count: 10, P50: 40, P95: 220, Max: 900},
			RecentFailures: 1,
			ScaleEvents: []apiadmin.QueueScaleEvent{
				{At: "2026-08-02T12:00:00Z", Direction: "up", From: 4, To: 6},
			},
		},
	}})

	rec := doPost(h.QueueQueues, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)

	rt, ok := rows[0]["runtime"].(map[string]any)
	require.True(t, ok, "runtime block should be present")
	assert.EqualValues(t, 6, rt["workers"])
	assert.EqualValues(t, 4, rt["minWorkers"])
	assert.EqualValues(t, 32, rt["maxWorkers"])
	assert.Equal(t, true, rt["autoScale"])
	assert.EqualValues(t, 1, rt["recentFailures"])

	events, ok := rt["scaleEvents"].([]any)
	require.True(t, ok)
	require.Len(t, events, 1)
	assert.Equal(t, "up", events[0].(map[string]any)["direction"])
}

// provider 未配線 (= 純正互換ビルド / driver 未対応) では runtime block を
// 一切出さない。upstream shape を汚さないため。
func TestQueueQueues_OmitsRuntimeWhenUnwired(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		queues: []string{"deliver"},
		info:   map[string]*apiadmin.QueueInfoResult{"deliver": {Queue: "deliver"}},
	})

	rec := doPost(h.QueueQueues, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	require.Len(t, rows, 1)
	_, hasRuntime := rows[0]["runtime"]
	assert.False(t, hasRuntime, "runtime must be omitted when the provider is unwired")
}

// provider が知らない queue も block を出さない (部分配線でも壊れない)。
func TestQueueQueues_OmitsRuntimeForUnknownQueue(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		queues: []string{"deliver"},
		info:   map[string]*apiadmin.QueueInfoResult{"deliver": {Queue: "deliver"}},
	})
	h.SetQueueRuntimeProvider(&stubQueueRuntime{known: map[string]apiadmin.QueueRuntime{}})

	rec := doPost(h.QueueQueues, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	_, hasRuntime := rows[0]["runtime"]
	assert.False(t, hasRuntime)
}

// job 詳細が BullMQ の保存値をそのまま返すこと (#2689)。
//
// **判定は upstream の JSON schema ではなく frontend の条件に合わせる。**
// 以前は schema (`optional: false`) に寄せて failedReason に空文字、
// opts / progress / returnValue に組み立てた値を常に出していた。その結果:
//
//   - 成功した job にも赤い警告アイコン付きの空の "Failed reason" 行が出る
//     (`v-if="job.failedReason != null"` は空文字でも真)
//   - Options タブが `{attempts:0,delay:0,repeat:null}` になり、実際の
//     backoff / removeOnComplete / removeOnFail が消える
//   - `opts.attempts` が 0 なので Attempts 行もヘッダの n/m バッジも出ない
//   - 戻り値の無い job に空の "Return value" タブが出る
func TestQueueShowJob_PassesThroughBullFields(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	opts := `{"attempts":8,"backoff":{"type":"custom","delay":0},"removeOnComplete":{"count":30,"age":604800}}`
	h.SetQueueInspector(&stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"failed": {
				ID: "failed", Queue: "inbox", Type: "ap:inbox", State: "failed",
				LastErr:     "process inbox activity: unexpected status: 500",
				Stacktrace:  []string{"first attempt", "", "second attempt"},
				Opts:        json.RawMessage(opts),
				Delay:       1500,
				Progress:    json.RawMessage(`42`),
				ReturnValue: json.RawMessage(`{"ok":true}`),
			},
		},
	})

	rec := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"failed"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	shapetest.Assert(t, "QueueJob", got)

	// opts は保存された JSON がそのまま出ること。組み立て直すと
	// mk-go が知らない key が黙って消える。
	gotOpts, ok := got["opts"].(map[string]any)
	require.True(t, ok, "opts は object であること")
	assert.EqualValues(t, 8, gotOpts["attempts"], "Attempts 行はこの値で出し分けられる")
	assert.NotNil(t, gotOpts["backoff"], "backoff が消えないこと")
	assert.NotNil(t, gotOpts["removeOnComplete"], "removeOnComplete が消えないこと")

	assert.EqualValues(t, 1500, got["delay"], "delay は保存値")
	assert.EqualValues(t, 42, got["progress"], "progress は保存値")
	assert.Equal(t, map[string]any{"ok": true}, got["returnValue"])
	assert.Equal(t, "process inbox activity: unexpected status: 500", got["failedReason"])

	// upstream packJobData は空要素を落として逆順にする (新しい試行が先)。
	assert.Equal(t, []any{"second attempt", "first attempt"}, got["stacktrace"])
	assert.Equal(t, true, got["isFailed"])
}

// stacktrace があれば failedReason が無くても失敗扱いにすること。
// upstream: isFailed = !!failedReason || stacktrace.length > 0。
func TestQueueShowJob_IsFailedFromStacktraceAlone(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"st": {ID: "st", Queue: "inbox", Type: "ap:inbox", State: "failed", Stacktrace: []string{"boom"}},
		},
	})
	rec := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"st"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, true, got["isFailed"])
	_, hasFailedReason := got["failedReason"]
	assert.False(t, hasFailedReason, "failedReason が無いなら出さない")
}

// Data タブに framing の type が残ること (#2689)。以前は body だけを返して
// いたので、どの task type の job かが詳細画面から分からなかった。
func TestQueueShowJob_DataKeepsTaskType(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"d": {ID: "d", Queue: "inbox", Type: "ap:inbox", State: "wait", Payload: []byte(`{"path":"/inbox"}`)},
		},
	})
	rec := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"d"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	data, ok := got["data"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "ap:inbox", data["type"])
	assert.Equal(t, map[string]any{"path": "/inbox"}, data["body"])
}

// show-job-logs が driver の実データを返すこと (#2689)。以前は常に空配列。
func TestQueueShowJobLogs_ReturnsDriverLogs(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		logs: map[string][]string{"j1": {"line one", "line two"}},
	})
	rec := doPost(h.QueueShowJobLogs, `{"queue":"inbox","jobId":"j1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got []string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, []string{"line one", "line two"}, got)

	// log の無い job は空配列 (null にしない — 配列を期待する UI が壊れる)。
	rec2 := doPost(h.QueueShowJobLogs, `{"queue":"inbox","jobId":"none"}`, adminUser)
	require.Equal(t, http.StatusOK, rec2.Code)
	assert.Equal(t, "[]", strings.TrimSpace(rec2.Body.String()))
}

// **本番で実際に通るのは「JSON の null」の経路** (#2689 review MED-1)。
// mk-go の processor は戻り値を持たないので、mkq は完了した job すべての
// `returnvalue` に**リテラル `null`** を書く。field が空 (未完了) の経路だけ
// テストしても、Return value タブを隠している判定は検証できない。
func TestQueueShowJob_ReturnValueNullIsOmitted(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			// 完了済みで returnvalue = null (mk-go の processor の既定)。
			"done": {ID: "done", Queue: "inbox", Type: "ap:inbox", State: "completed",
				ReturnValue: json.RawMessage(`null`), Progress: json.RawMessage(`null`)},
			// 壊れた JSON は fallback へ倒す (admin 画面を壊さない)。
			"broken": {ID: "broken", Queue: "inbox", Type: "ap:inbox", State: "completed",
				ReturnValue: json.RawMessage(`{not json`), Opts: json.RawMessage(`{not json`)},
		},
	})

	rec := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"done"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	_, has := got["returnValue"]
	assert.False(t, has, "returnvalue が null の job に Return value タブを出さない")
	assert.EqualValues(t, 0, got["progress"], "progress が null なら Bull 既定の 0 に倒す")

	rec2 := doPost(h.QueueShowJobLogs, `{"queue":"inbox","jobId":"broken"}`, adminUser)
	require.Equal(t, http.StatusOK, rec2.Code)
	rec3 := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"broken"}`, adminUser)
	require.Equal(t, http.StatusOK, rec3.Code)
	var got3 map[string]any
	require.NoError(t, json.Unmarshal(rec3.Body.Bytes(), &got3))
	_, hasRV := got3["returnValue"]
	assert.False(t, hasRV, "壊れた JSON も出さない")
	// opts は object 必須なので、壊れていても {} に倒して frontend の
	// `job.opts.attempts` 参照が TypeError にならないようにする。
	assert.Equal(t, map[string]any{}, got3["opts"])
}

// maxRetry を opts.attempts から拾うこと (#2689 review LOW-4)。mkq driver は
// TaskSummary.MaxRetry を埋めないので、0 のまま出すと「リトライしない設定」に見える。
func TestQueueShowJob_MaxRetryFromOpts(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"a": {ID: "a", Queue: "inbox", Type: "ap:inbox", State: "wait", Opts: json.RawMessage(`{"attempts":8}`)},
			// MaxRetry を埋める driver ではそちらを優先する。
			"b": {ID: "b", Queue: "inbox", Type: "ap:inbox", State: "wait", MaxRetry: 3},
		},
	})
	for id, want := range map[string]int{"a": 8, "b": 3} {
		rec := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"`+id+`"}`, adminUser)
		require.Equal(t, http.StatusOK, rec.Code)
		var got map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.EqualValues(t, want, got["maxRetry"], "job %s", id)
	}
}

// qualifiedName は driver が組んだ BullMQ の `<prefix>:<name>` を出すこと
// (#2689 review MED-2)。上位で "bull" を決め打ちすると prefix を変えた構成で
// 嘘を表示する。
func TestQueueQueueStats_QualifiedName(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		info: map[string]*apiadmin.QueueInfoResult{
			"inbox": {Queue: "inbox", QualifiedName: "mkprefix:inbox"},
		},
	})
	rec := doPost(h.QueueQueueStats, `{"queue":"inbox"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "mkprefix:inbox", got["qualifiedName"])
	assert.Equal(t, "inbox", got["name"])
}

// driver が報告しない場合は queue 名で代替すること。
func TestQueueQueueStats_QualifiedNameFallback(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		info: map[string]*apiadmin.QueueInfoResult{"inbox": {Queue: "inbox"}},
	})
	rec := doPost(h.QueueQueueStats, `{"queue":"inbox"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "inbox", got["qualifiedName"])
}

// mk-go が実際に持つ全 queue で pause / resume が通ること (#2690)。
//
// 以前は upstream の QUEUE_TYPES を手書きで持っていただけだったので、重なるのは
// deliver / inbox / objectStorage / relationship の 4 つだけ。fork frontend は
// mk-go の 8 種のタブを出すため、**push / export / webhook / maintenance では
// 一時停止・再開が 400 で失敗していた**。
func TestQueuePauseResume_AcceptsEveryManagedQueue(t *testing.T) {
	all := queue.AllQueueNames()
	require.NotEmpty(t, all)

	info := map[string]*apiadmin.QueueInfoResult{}
	for _, q := range all {
		info[q] = &apiadmin.QueueInfoResult{Queue: q}
	}
	for _, qname := range all {
		t.Run(qname, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			h.SetQueueInspector(&stubQueueInspector{queues: all, info: info})
			body := `{"queue":"` + qname + `"}`
			assert.Equal(t, http.StatusNoContent, doPost(h.QueuePause, body, adminUser).Code,
				"pause が通ること")
			assert.Equal(t, http.StatusNoContent, doPost(h.QueueResume, body, adminUser).Code,
				"resume が通ること")
		})
	}
}

// 純正 frontend が出す upstream の queue 名は、mk-go に無くても 400 にしない
// (drop-in 互換。運用していない queue は no-op 204)。知らない名前は 400。
func TestQueuePauseResume_UpstreamNamesAndUnknown(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{queues: queue.AllQueueNames()})

	// mk-go に無い upstream の名前。
	assert.Equal(t, http.StatusNoContent,
		doPost(h.QueuePause, `{"queue":"endedPollNotification"}`, adminUser).Code,
		"upstream の enum は 400 にしない (drop-in)")
	// どちらにも無い名前。
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.QueuePause, `{"queue":"nosuchqueue"}`, adminUser).Code)
	assert.Equal(t, http.StatusBadRequest,
		doPost(h.QueueResume, `{"queue":"nosuchqueue"}`, adminUser).Code)
}

// 試行ごとの開始時刻を応答に載せること (#2692)。BullMQ は per-attempt の時刻を
// 残さないので、mkq の拡張として記録したものをそのまま出す。Timeline はこれで
// 再試行を実時刻に並べる。
func TestQueueShowJob_AttemptsAt(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	h.SetQueueInspector(&stubQueueInspector{
		task: map[string]*apiadmin.QueueTaskSummary{
			"retried": {ID: "retried", Queue: "inbox", Type: "ap:inbox", State: "failed",
				AttemptsAt: []int64{1000, 2000, 3000}},
			// mkq v1.0.8 より前に失敗した job / TS が書いた job。
			"legacy": {ID: "legacy", Queue: "inbox", Type: "ap:inbox", State: "failed"},
		},
	})

	rec := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"retried"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, []any{float64(1000), float64(2000), float64(3000)}, got["attemptsAt"])

	// **記録が無くても null にしない。** 配列を期待する frontend が
	// `job.attemptsAt.length` で落ちる。
	rec2 := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"legacy"}`, adminUser)
	require.Equal(t, http.StatusOK, rec2.Code)
	var got2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &got2))
	assert.Equal(t, []any{}, got2["attemptsAt"])
}

// プラグイン専用のキュー (#2818) は名前が動的なので、静的な一覧では判定
// できない。**接頭辞で通したうえで、実在するかは Inspector に確かめさせる。**
func TestQueuePauseResume_PluginQueue(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{queues: []string{"deliver", "plugin:demo"}}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueuePause, `{"queue":"plugin:demo"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"plugin:demo"}, insp.pauseCalls, "静的な一覧に無くても通ること")

	rec = doPost(h.QueueResume, `{"queue":"plugin:demo"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, []string{"plugin:demo"}, insp.resumeCalls)

	// 入れていないプラグインの名前は、接頭辞は通るが managed でないので
	// no-op 204 (未運用 queue と同じ扱い)。
	insp.pauseCalls = nil
	rec = doPost(h.QueuePause, `{"queue":"plugin:notinstalled"}`, adminUser)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, insp.pauseCalls)

	// 接頭辞が無ければ従来どおり 400。
	assert.Equal(t, http.StatusBadRequest, doPost(h.QueuePause, `{"queue":"demo"}`, adminUser).Code)
}

// **予約投稿とアカウント削除を促進 / 破棄しないこと。**
//
// deliver キューにはこの 2 つが同居している。促進すると全利用者の未公開の
// 予約投稿が即時公開され (不可逆)、クリアすると二度と発火しない (失敗通知も
// 出ない)。アカウント削除を消すと、削除フラグだけ立って中身が残るアカウントが
// 生まれ、dedup キーのせいで 24 時間は再実行も効かない。
func TestQueuePromoteJobs_SkipsUserScheduledWork(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {
				{ID: "t_sched", Type: queue.TaskTypePostScheduledNote},
				{ID: "t_del", Type: queue.TaskTypeDeleteAccount},
				{ID: "t_deliver", Type: queue.TaskTypeDeliver},
			},
		},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueuePromoteJobs, `{"queue":"deliver"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []string{"t_deliver"}, insp.runCalls,
		"配送ジョブだけを促進すること (利用者の予定は触らない)")
}

func TestQueueClear_SkipsUserScheduledWork(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		pending: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {
				{ID: "c_sched", Type: queue.TaskTypePostScheduledNote},
				{ID: "c_deliver", Type: queue.TaskTypeDeliver},
			},
		},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueClear, `{"queue":"deliver","state":"wait"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.NotContains(t, insp.deleted, "c_sched", "予約投稿は消さないこと")
}

// **終了済みのバケットでは保護を掛けないこと。**
//
// completed / failed は既に終わった記録なので、消せなくすると**終了済みの
// 予約投稿 / アカウント削除が永久に残る** (掃除の目的そのものが果たせない)。
// 保護が要るのは「これから動く」バケット (wait / delayed / retry) だけ。
func TestQueueClear_ProtectionOnlyAppliesToPendingBuckets(t *testing.T) {
	for _, tc := range []struct {
		state   string
		bucket  string
		protect bool
	}{
		{"wait", "pending", true},
		{"delayed", "scheduled", true},
		{"failed", "failed", false},
		{"completed", "completed", false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			insp := &stubQueueInspector{}
			h.SetQueueInspector(insp)
			rows := map[string][]*apiadmin.QueueTaskSummary{
				"deliver": {{ID: "s1", Type: queue.TaskTypePostScheduledNote}},
			}
			switch tc.bucket {
			case "pending":
				insp.pending = rows
			case "scheduled":
				insp.scheduled = rows
			case "failed":
				insp.failed = rows
			case "completed":
				insp.completed = rows
			}

			rec := doPost(h.QueueClear, `{"queue":"deliver","state":"`+tc.state+`"}`, adminUser)
			require.Equal(t, http.StatusNoContent, rec.Code)
			if tc.protect {
				assert.Empty(t, insp.deleted, "%s では予約投稿を守ること", tc.state)
			} else {
				assert.Equal(t, []string{"s1"}, insp.deleted,
					"%s は終了済みの記録なので消せること", tc.state)
			}
		})
	}
}

// stubTasks returns n tasks of typ with ids prefix-0 .. prefix-(n-1).
func stubTasks(prefix, typ string, n int) []*apiadmin.QueueTaskSummary {
	out := make([]*apiadmin.QueueTaskSummary, n)
	for i := range out {
		out[i] = &apiadmin.QueueTaskSummary{ID: fmt.Sprintf("%s-%d", prefix, i), Type: typ}
	}
	return out
}

func idsOf(tasks []*apiadmin.QueueTaskSummary) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.ID
	}
	return out
}

// **先頭ページが保護対象で埋まっていても、その後ろを消すこと (#3169)。**
//
// 以前は先頭ページ (100 件) を引いては消し、全部が保護対象なら打ち切ったので、
// 予約投稿が 100 件を超えて先頭にあると、後ろの配送を 1 件も消さなかった。
func TestQueueClear_ReachesPastProtectedFirstPage(t *testing.T) {
	for _, tc := range []struct {
		state string
		fill  func(*stubQueueInspector, []*apiadmin.QueueTaskSummary)
	}{
		{"delayed", func(s *stubQueueInspector, rows []*apiadmin.QueueTaskSummary) {
			s.scheduled = map[string][]*apiadmin.QueueTaskSummary{"deliver": rows}
		}},
		{"wait", func(s *stubQueueInspector, rows []*apiadmin.QueueTaskSummary) {
			s.pending = map[string][]*apiadmin.QueueTaskSummary{"deliver": rows}
		}},
		{"*", func(s *stubQueueInspector, rows []*apiadmin.QueueTaskSummary) {
			s.retry = map[string][]*apiadmin.QueueTaskSummary{"deliver": rows}
		}},
	} {
		t.Run(tc.state, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			protected := stubTasks("note", queue.TaskTypePostScheduledNote, 150)
			deliver := stubTasks("deliver", queue.TaskTypeDeliver, 30)
			insp := &stubQueueInspector{}
			tc.fill(insp, append(append([]*apiadmin.QueueTaskSummary{}, protected...), deliver...))
			h.SetQueueInspector(insp)

			rec := doPost(h.QueueClear, `{"queue":"deliver","state":"`+tc.state+`"}`, adminUser)
			require.Equal(t, http.StatusNoContent, rec.Code)
			assert.ElementsMatch(t, idsOf(deliver), insp.deleted,
				"保護対象の後ろにある配送を消し、保護対象は消さないこと")
		})
	}
}

// **消せない task があっても、その後ろを消し続けること (#3169)。** cron 由来の
// repeat job は RemoveJob が拒否する。先頭がそれで埋まっていても止めない。
func TestQueueClear_ContinuesPastUndeletableTasks(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	stuck := stubTasks("repeat", queue.TaskTypeDeliver, 150)
	deliver := stubTasks("deliver", queue.TaskTypeDeliver, 30)
	insp := &stubQueueInspector{
		scheduled:     map[string][]*apiadmin.QueueTaskSummary{"deliver": append(append([]*apiadmin.QueueTaskSummary{}, stuck...), deliver...)},
		deleteFailIDs: map[string]bool{},
	}
	for _, t := range stuck {
		insp.deleteFailIDs[t.ID] = true
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueClear, `{"queue":"deliver","state":"delayed"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.ElementsMatch(t, idsOf(deliver), insp.deleted)
}

// **promote は先頭の 100 件だけでなく全件を対象にすること (#3169)。** upstream の
// `queue.promoteJobs()` は全件が対象。
func TestQueuePromoteJobs_PromotesBeyondFirstPage(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	protected := stubTasks("note", queue.TaskTypePostScheduledNote, 150)
	retry := stubTasks("retry", queue.TaskTypeDeliver, 120)
	insp := &stubQueueInspector{
		scheduled: map[string][]*apiadmin.QueueTaskSummary{"deliver": protected},
		retry:     map[string][]*apiadmin.QueueTaskSummary{"deliver": retry},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueuePromoteJobs, `{"queue":"deliver"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.EqualValues(t, 120, got["promoted"])
	assert.ElementsMatch(t, idsOf(retry), insp.runCalls)
}

// **delayed は振り分けた一覧ではなく、delayed 全体の一覧でたどること (#3169)。**
//
// mkq の scheduled / retry の一覧は、atm で振り分けてからページを切るので、
// 1 ページ読むたびに delayed の全件を Redis から読み直す。それでページを
// 進めると、retry が 5 万件あるときに約 2,500 万回の読み出しになる。delayed
// 全体の一覧は添字の範囲だけを読むので、たどり切っても N 回で済む
// (mkqdriver の TestInspector_ListDelayedTasks_WalkReadsEachJobOnce)。
func TestQueueBulkOps_WalkDelayedOnce(t *testing.T) {
	const n = 5000
	for _, tc := range []struct {
		name string
		call func(h *apiadmin.Handler) int
	}{
		{"clear delayed", func(h *apiadmin.Handler) int {
			return doPost(h.QueueClear, `{"queue":"deliver","state":"delayed"}`, adminUser).Code
		}},
		{"clear all", func(h *apiadmin.Handler) int {
			return doPost(h.QueueClear, `{"queue":"deliver","state":"*"}`, adminUser).Code
		}},
		{"promote", func(h *apiadmin.Handler) int {
			return doPost(h.QueuePromoteJobs, `{"queue":"deliver"}`, adminUser).Code
		}},
		{"deliver-delayed", func(h *apiadmin.Handler) int {
			return doPost(h.QueueDeliverDelayed, `{}`, adminUser).Code
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			insp := &stubQueueInspector{
				retry: map[string][]*apiadmin.QueueTaskSummary{"deliver": stubTasks("retry", queue.TaskTypeDeliver, n)},
			}
			h.SetQueueInspector(insp)

			code := tc.call(h)
			require.Less(t, code, 300)
			assert.Zero(t, insp.filteredCalls, "振り分けた一覧を使わないこと")
			// 5000 件 = 満杯の 50 ページ + 空の 1 ページ。
			assert.Equal(t, n/apiadmin.DelayedTasksFetchPageSize+1, insp.delayedCalls)
		})
	}
}

// churnInspector wraps stubQueueInspector and simulates a delayed bucket that
// changes while it is walked: each ListDelayedTasks call first pushes a new
// job to the head (newest first), and the first page loses one entry whose
// HASH vanished (mkq skips it).
type churnInspector struct {
	stubQueueInspector
	pushed int
}

func (c *churnInspector) ListDelayedTasks(q string, page, pageSize int) ([]*apiadmin.QueueTaskSummary, error) {
	c.pushed++
	head := &apiadmin.QueueTaskSummary{ID: fmt.Sprintf("new-%d", c.pushed), Type: queue.TaskTypeDeliver}
	c.retry[q] = append([]*apiadmin.QueueTaskSummary{head}, c.retry[q]...)
	rows, err := c.stubQueueInspector.ListDelayedTasks(q, page, pageSize)
	if page == 1 && len(rows) > 0 {
		rows = rows[1:]
	}
	return rows, err
}

// **走査中に一覧が動いても、読み飛ばさず、同じ job を二度扱わないこと (#3169)。**
//
// 新しい job が先頭に入ると、既に読んだ job が後ろへずれて次のページにもう一度
// 出る。重複を除かないと promote が同じ job を二度促進し、件数も水増しされる。
// また mkq は HASH が既に無い job を飛ばすので、途中のページが 100 件に満たない
// ことがある。そこで止まると後ろを取りこぼす。
func TestQueuePromoteJobs_WalkSurvivesChurn(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &churnInspector{}
	insp.retry = map[string][]*apiadmin.QueueTaskSummary{"deliver": stubTasks("retry", queue.TaskTypeDeliver, 250)}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueuePromoteJobs, `{"queue":"deliver"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	counts := map[string]int{}
	for _, id := range insp.runCalls {
		counts[id]++
	}
	for id, n := range counts {
		assert.Equal(t, 1, n, "%s を二度促進しないこと", id)
	}
	// 1 ページ目で欠けた 1 件 (走査の最初に先頭へ入った new-1) 以外は、
	// 元からある 250 件すべてに届くこと。
	for i := 0; i < 250; i++ {
		assert.Contains(t, counts, fmt.Sprintf("retry-%d", i))
	}
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.EqualValues(t, len(insp.runCalls), got["promoted"])
}

// legacyActivityPayload marshals a deliver / inbox payload the way it was
// stored before #3614: the activity as a base64 string under "body".
func legacyActivityPayload(t *testing.T, fields map[string]any, body []byte) []byte {
	t.Helper()
	m := map[string]any{"body": body}
	for k, v := range fields {
		m[k] = v
	}
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return out
}

// activityPayloadEncodings returns the two stored shapes of an inbox payload
// carrying body: the pre-#3614 base64 one ("legacy", also what this release
// writes) and the #3614 one written once driver.CompactJobPayloads is
// enabled ("compact").
func activityPayloadEncodings(t *testing.T, body []byte) map[string][]byte {
	t.Helper()
	compact, err := queue.InboxPayload{Body: body, Path: "/inbox"}.MarshalCompactJSON()
	require.NoError(t, err)
	return map[string][]byte{
		"legacy":  legacyActivityPayload(t, map[string]any{"path": "/inbox"}, body),
		"compact": compact,
	}
}

// **配送と受信の本文を、base64 の 1 行ではなくオブジェクトで返すこと (#3550)。**
//
// payload は本文を []byte で持つので、#3614 より前の job では Data タブに base64 の
// 長い 1 行が出て、ハイライトで画面が数秒固まる。#3614 以降の job は本文を
// オブジェクトか文字列で持つ。どちらの形の job も同じ見え方にすること。
func TestQueueShowJob_DataExpandsActivityBody(t *testing.T) {
	activity := map[string]any{"type": "Create", "id": "https://local.example/notes/1/activity"}
	rawActivity, err := json.Marshal(activity)
	require.NoError(t, err)
	// Misskey の JSON.stringify は < をエスケープしないので、受信の本文には生の <
	// が入る。#3614 の形では、< などをエスケープしたオブジェクト (bodyEscaped)
	// で載る。
	htmlContent := "<p>" + strings.Repeat("今日はいい天気でした。", 10) + "</p>"
	rawHTML := []byte(`{"type":"Create","object":{"content":"` + htmlContent + `"}}`)
	deliverCompact, err := queue.DeliverPayload{Inbox: "https://remote.example/inbox", Body: rawActivity, KeyPEM: "secret"}.MarshalCompactJSON()
	require.NoError(t, err)
	deliverLegacy := legacyActivityPayload(t, map[string]any{"inbox": "https://remote.example/inbox", "keyPem": "secret"}, rawActivity)

	type tcase struct {
		name    string
		task    *apiadmin.QueueTaskSummary
		want    any
		checkFn func(t *testing.T, body map[string]any)
	}
	noSecret := func(t *testing.T, body map[string]any) {
		assert.NotEqual(t, "secret", body["keyPem"], "鍵は伏せたままにすること")
		assert.NotContains(t, body, "bodyText")
	}
	cases := []tcase{
		{name: "deliver/legacy", task: &apiadmin.QueueTaskSummary{ID: "d", Type: queue.TaskTypeDeliver, Payload: deliverLegacy}, want: activity, checkFn: noSecret},
		{name: "deliver/compact", task: &apiadmin.QueueTaskSummary{ID: "d", Type: queue.TaskTypeDeliver, Payload: deliverCompact}, want: activity, checkFn: noSecret},
		// 配送と受信以外の job は触らない。
		{name: "other type", task: &apiadmin.QueueTaskSummary{ID: "d", Type: "ap:other", Payload: legacyActivityPayload(t, nil, rawActivity)},
			want: base64.StdEncoding.EncodeToString(rawActivity)},
	}
	for enc, payload := range activityPayloadEncodings(t, rawActivity) {
		cases = append(cases, tcase{name: "inbox/" + enc, task: &apiadmin.QueueTaskSummary{ID: "d", Type: queue.TaskTypeInbox, Payload: payload}, want: activity})
	}
	htmlEncodings := activityPayloadEncodings(t, rawHTML)
	require.Contains(t, string(htmlEncodings["compact"]), `"bodyEscaped":{`)
	for enc, payload := range htmlEncodings {
		cases = append(cases, tcase{name: "inbox html/" + enc, task: &apiadmin.QueueTaskSummary{ID: "d", Type: queue.TaskTypeInbox, Payload: payload},
			want:    map[string]any{"type": "Create", "object": map[string]any{"content": htmlContent}},
			checkFn: func(t *testing.T, body map[string]any) { assert.NotContains(t, body, "bodyEscaped") }})
	}
	// JSON として読めない本文と、JSON の文字列 (オブジェクトでも配列でもない) は
	// 埋め込まない。以前の形は base64 のまま、#3614 の形 (書く側を有効にした
	// 後) は文字列のまま返す。
	for _, raw := range []string{"not json", `"abc"`} {
		encs := activityPayloadEncodings(t, []byte(raw))
		cases = append(cases,
			tcase{name: raw + "/legacy", task: &apiadmin.QueueTaskSummary{ID: "d", Type: queue.TaskTypeInbox, Payload: encs["legacy"]},
				want: base64.StdEncoding.EncodeToString([]byte(raw))},
			tcase{name: raw + "/compact", task: &apiadmin.QueueTaskSummary{ID: "d", Type: queue.TaskTypeInbox, Payload: encs["compact"]},
				want: raw})
	}
	// bodyEscaped と bodyText が両方あるときは、worker と同じく bodyEscaped を
	// 見る (書く側は 1 つしか出さないが、優先の順を揃えておく)。
	cases = append(cases, tcase{name: "bodyEscaped wins over bodyText",
		task: &apiadmin.QueueTaskSummary{ID: "d", Type: queue.TaskTypeInbox, Payload: []byte(`{"path":"/inbox","bodyText":"not json","bodyEscaped":{"type":"Create"}}`)},
		want: map[string]any{"type": "Create"},
		checkFn: func(t *testing.T, body map[string]any) {
			assert.NotContains(t, body, "bodyText")
			assert.NotContains(t, body, "bodyEscaped")
		}})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			h.SetQueueInspector(&stubQueueInspector{task: map[string]*apiadmin.QueueTaskSummary{"d": tc.task}})
			rec := doPost(h.QueueShowJob, `{"queue":"deliver","jobId":"d"}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			var got map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			data, ok := got["data"].(map[string]any)
			require.True(t, ok)
			body, ok := data["body"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tc.want, body["body"])
			if tc.checkFn != nil {
				tc.checkFn(t, body)
			}
		})
	}
}

// **入れ子の深い本文を埋め込まないこと (#3550)。** encoding/json は埋め込んだ
// 本文を書き出すときに、外側の入れ子と合わせて深さを数える。深い受信の本文が
// 1 件あるだけで、一覧の書き出しが失敗し、ほかの job も含めて空のレスポンスに
// なった。受信の本文は署名を検証する前に積まれるので、外から送り込める。
func TestQueueJobs_DeepActivityBodyDoesNotBreakList(t *testing.T) {
	deep := `{"actor":"https://remote.example/u","a":` + strings.Repeat("[", 9997) + strings.Repeat("]", 9997) + `}`
	// 文字列の中の括弧 (エスケープした引用符の後ろ) は深さに数えない。
	inString := `\"` + strings.Repeat("[", 100)
	shallowBody := []byte(`{"a":[[{"b":"` + inString + `"}]]}`)
	for enc, want := range map[string]string{
		"legacy":  base64.StdEncoding.EncodeToString([]byte(deep)),
		"compact": deep,
	} {
		t.Run(enc, func(t *testing.T) {
			h, _, _, _ := newTestHandler(t)
			h.SetQueueInspector(&stubQueueInspector{
				failed: map[string][]*apiadmin.QueueTaskSummary{"inbox": {
					{ID: "deep", Type: queue.TaskTypeInbox, Payload: activityPayloadEncodings(t, []byte(deep))[enc]},
					{ID: "shallow", Type: queue.TaskTypeInbox, Payload: activityPayloadEncodings(t, shallowBody)[enc]},
				}},
			})
			rec := doPost(h.QueueJobs, `{"queue":"inbox","state":["failed"]}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			var got []map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "一覧が壊れないこと")
			require.Len(t, got, 2)
			bodies := map[string]any{}
			for _, job := range got {
				bodies[job["id"].(string)] = job["data"].(map[string]any)["body"].(map[string]any)["body"]
			}
			assert.Equal(t, want, bodies["deep"], "深い本文は埋め込まずに文字列で返す")
			assert.Equal(t, map[string]any{"a": []any{[]any{map[string]any{"b": `"` + strings.Repeat("[", 100)}}}}, bodies["shallow"])
		})
	}
}

// 深さの上限は 64 段で、数えるのは入れ子の深さであって括弧の総数ではない。
// JSON として読めない本文と不正な UTF-8 の本文は、埋め込むと一覧の書き出しを
// 壊すので埋め込まない (#3550)。以前の形の job は base64 のまま、#3614 の形の
// job は文字列のまま返す (不正な UTF-8 は #3614 の形でも base64 で載る)。
func TestQueueShowJob_ActivityBodyEmbeddingLimits(t *testing.T) {
	nested := func(n int) string { return strings.Repeat("[", n) + strings.Repeat("]", n) }
	manyTags := `{"tag":[` + strings.TrimSuffix(strings.Repeat(`{"type":"Emoji"},`, 70), ",") + `]}`
	for _, tc := range []struct {
		name        string
		body        string
		embedded    bool
		compactText bool
	}{
		{"depth 64", nested(64), true, false},
		{"depth 65", nested(65), false, true},
		// 浅いが括弧が多い (絵文字の多いノート)。
		{"many brackets", manyTags, true, false},
		{"truncated object", `{"a":`, false, true},
		{"invalid utf-8", "{\"a\":\"\xff\"}", false, false},
	} {
		for enc, payload := range activityPayloadEncodings(t, []byte(tc.body)) {
			t.Run(tc.name+"/"+enc, func(t *testing.T) {
				h, _, _, _ := newTestHandler(t)
				h.SetQueueInspector(&stubQueueInspector{task: map[string]*apiadmin.QueueTaskSummary{
					"d": {ID: "d", Type: queue.TaskTypeInbox, Payload: payload},
				}})
				rec := doPost(h.QueueShowJob, `{"queue":"inbox","jobId":"d"}`, adminUser)
				require.Equal(t, http.StatusOK, rec.Code)
				var got map[string]any
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
				body := got["data"].(map[string]any)["body"].(map[string]any)["body"]
				switch {
				case tc.embedded:
					_, isString := body.(string)
					assert.False(t, isString, "文字列ではなく JSON として埋め込むこと")
				case enc == "compact" && tc.compactText:
					assert.Equal(t, tc.body, body, "読める文字列のまま返すこと")
				default:
					assert.Equal(t, base64.StdEncoding.EncodeToString([]byte(tc.body)), body, "base64 のまま返すこと")
				}
			})
		}
	}
}

// admin/queue/jobs の独自の limit・page は integer として宣言した (#3867)。
// 本家の ajv と同じく、整数を小数の形で書いた値も受け、その整数として読む。
func TestQueueJobs_LimitAndPageAcceptFractionForms(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	insp := &stubQueueInspector{
		pending: map[string][]*apiadmin.QueueTaskSummary{
			"deliver": {{ID: "d1"}, {ID: "d2"}, {ID: "d3"}},
		},
	}
	h.SetQueueInspector(insp)

	rec := doPost(h.QueueJobs, `{"queue":"deliver","state":"wait","limit":2.0,"page":1e0}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rows))
	assert.Len(t, rows, 2)
}
