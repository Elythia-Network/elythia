package queue_test

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/queue/driver/mkqdriver"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testRedis *testutil.TestRedis

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testRedis, err = testutil.SetupRedis(ctx)
	if err != nil {
		log.Fatalf("failed to setup redis: %v", err)
	}
	code := m.Run()
	testRedis.Teardown(ctx)
	os.Exit(code)
}

// newDriver constructs a fresh mkq-backed driver bound to the test
// Redis. Each call returns an independent driver instance so tests can
// Close() one without affecting the others.
//
// **flush はしない。** enqueue した後に inspector 用の driver をもう 1 つ
// 作るテストがあり、ここで流すとその都度データが消える。掃除は各テスト
// 冒頭の flushTestRedis が担う。
func newDriver(t *testing.T) driver.Driver {
	t.Helper()
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:       redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		Concurrency: 2,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// newInspector returns a driver-level inspector bound to the test Redis.
// enqueue 側が実際に Redis へ書いた内容を読む用途で、queue.Inspector を
// 経由しない層 (= driver.TaskSummary そのもの) を見たいときに使う。
func newInspector(t *testing.T) driver.Inspector {
	t.Helper()
	return newDriver(t).Inspector()
}

// listPending fetches the first page of the queue's pending bucket.
// mkq の一覧 API は page / pageSize を取るので、呼び出し側を読みやすく
// 保つための薄いラッパー。
func listPending(t *testing.T, insp driver.Inspector, qname string) []*driver.TaskSummary {
	t.Helper()
	rows, err := insp.ListPendingTasks(qname, 1, 100)
	require.NoError(t, err)
	return rows
}

// jobAttempts reads the BullMQ `attempts` (= 総試行回数) stored on the job.
//
// **mkq driver は driver.TaskSummary.MaxRetry を埋めない。** BullMQ は
// 試行回数を job の opts に持つので、そちらを読む。`WithMaxRetry(n)` は
// mkqdriver で `attempts = n+1` に翻訳される (option.go)。
func jobAttempts(t *testing.T, s *driver.TaskSummary) int {
	t.Helper()
	require.NotEmpty(t, s.Opts, "opts が空だと attempts を検証できない")
	var o struct {
		Attempts *int `json:"attempts"`
	}
	require.NoError(t, json.Unmarshal(s.Opts, &o))
	require.NotNil(t, o.Attempts, "opts に attempts が無い: %s", string(s.Opts))
	return *o.Attempts
}

func TestNewDeliverTask_RoundTrip(t *testing.T) {
	payload := queue.DeliverPayload{
		Inbox:  "https://remote.example/users/alice/inbox",
		Body:   []byte(`{"type":"Create"}`),
		KeyID:  "https://example.com/users/u1#main-key",
		KeyPEM: "PEMDATA",
	}
	task := queue.NewDeliverTask(payload)
	assert.Equal(t, queue.TaskTypeDeliver, task.Type())

	decoded, err := queue.DecodeDeliverPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload, decoded)
}

func TestDecodeDeliverPayload_Invalid(t *testing.T) {
	_, err := queue.DecodeDeliverPayload([]byte(`{not json`))
	assert.Error(t, err)
}

func TestClient_EnqueueDeliver(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	payload := queue.DeliverPayload{
		Inbox:  "https://remote.example/inbox",
		Body:   []byte(`{"type":"Follow"}`),
		KeyID:  "k1",
		KeyPEM: "PEM",
	}
	require.NoError(t, c.EnqueueDeliver(payload, driver.WithMaxRetry(3)))

	// inspector で実際にキューに入っていることを確認
	tasks := listPending(t, newInspector(t), queue.QueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeDeliver, tasks[0].Type)

	var got queue.DeliverPayload
	require.NoError(t, json.Unmarshal(tasks[0].Payload, &got))
	assert.Equal(t, payload, got)
}

// #495 / #531 review: deliverJobMaxAttempts を Client.Policy に流すと
// EnqueueDeliver で caller が WithMaxRetry を渡さないときに default と
// して適用される。Policy.MaxAttempts は BullMQ semantics の総試行回数
// なので driver.WithMaxRetry には N-1 で渡り、mkq が attempts=N に戻す
// (TS YAML 互換)。
func TestClient_EnqueueDeliver_PolicyMaxAttemptsApplied(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	// MaxAttempts=8 は TS YAML の `deliverJobMaxAttempts: 8` 相当。
	c.SetPolicy(queue.QueueName, queue.Policy{MaxAttempts: 8})

	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)}))

	tasks := listPending(t, newInspector(t), queue.QueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, 8, jobAttempts(t, tasks[0]),
		"MaxAttempts=8 (BullMQ の総試行回数) はそのまま BullMQ attempts=8 として保存される")
}

// MaxAttempts=1 (= no retry, only initial try) は WithMaxRetry(0) に
// マップされる。境界条件として BullMQ attempts=1 (= 初回のみ) になる
// ことを Inspector で確認する。
func TestClient_EnqueueDeliver_PolicyMaxAttemptsOne(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	c.SetPolicy(queue.QueueName, queue.Policy{MaxAttempts: 1})

	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)}))

	tasks := listPending(t, newInspector(t), queue.QueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, 1, jobAttempts(t, tasks[0]), "MaxAttempts=1 は attempts=1 (リトライ無し)")
}

// caller の WithMaxRetry は policy default を上書きする (last-write-wins)。
// caller の WithMaxRetry は driver semantics 直渡しなので変換しない。
func TestClient_EnqueueDeliver_CallerOptsOverridePolicy(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	c.SetPolicy(queue.QueueName, queue.Policy{MaxAttempts: 8})

	require.NoError(t, c.EnqueueDeliver(
		queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)},
		driver.WithMaxRetry(2),
	))

	tasks := listPending(t, newInspector(t), queue.QueueName)
	require.Len(t, tasks, 1)
	// caller の WithMaxRetry(2) は driver semantics 直渡し (= 2 回リトライ)
	// なので、BullMQ attempts は初回を足した 3 になる。
	assert.Equal(t, 3, jobAttempts(t, tasks[0]), "caller WithMaxRetry should override policy default")
}

func TestPolicyMap_PolicyFor(t *testing.T) {
	var nilMap queue.PolicyMap
	assert.Equal(t, queue.Policy{}, nilMap.PolicyFor("deliver"), "nil PolicyMap should return zero Policy")

	m := queue.PolicyMap{"deliver": {MaxAttempts: 5}}
	assert.Equal(t, 5, m.PolicyFor("deliver").MaxAttempts)
	assert.Equal(t, queue.Policy{}, m.PolicyFor("missing"))
}

// **閉じるのは driver であって Client ではない。** mkq の Client.Close は
// no-op で、接続を持っているのは driver 側 (server.go も driver.Close 一本に
// 統一している)。Client.Close を呼んだだけでは enqueue は通るので、ここで
// 閉じる対象を間違えると「閉じた後も投げられる」ことを検査しないまま緑に
// なる。
func TestClient_EnqueueDeliver_ClosedDriverFails(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	d := newDriver(t)
	c := queue.NewClient(d)
	require.NoError(t, d.Close())

	err := c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)})
	assert.Error(t, err)
}

func TestServer_HandleAndProcess(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	srv := queue.NewServer(newDriver(t))

	var (
		wg         sync.WaitGroup
		received   int32
		gotPayload queue.DeliverPayload
		mu         sync.Mutex
	)
	wg.Add(1)
	srv.Handle(queue.TaskTypeDeliver, func(_ context.Context, t driver.Task) error {
		defer wg.Done()
		atomic.AddInt32(&received, 1)
		p, err := queue.DecodeDeliverPayload(t.Payload())
		if err != nil {
			return err
		}
		mu.Lock()
		gotPayload = p
		mu.Unlock()
		return nil
	})
	require.NoError(t, srv.Start())
	defer srv.Shutdown()

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	payload := queue.DeliverPayload{
		Inbox:  "https://remote.example/inbox",
		Body:   []byte(`{"hello":"world"}`),
		KeyID:  "k",
		KeyPEM: "PEM",
	}
	require.NoError(t, c.EnqueueDeliver(payload))

	if !waitGroupTimeout(&wg, 5*time.Second) {
		t.Fatal("handler was not invoked within timeout")
	}

	assert.Equal(t, int32(1), atomic.LoadInt32(&received))
	mu.Lock()
	assert.Equal(t, payload, gotPayload)
	mu.Unlock()
}

// TestServer_ShutdownBeforeStartIsSafe pins that the facade tolerates
// Shutdown without a preceding Start. RoleServer は worker を起動しないまま
// Server.Shutdown を通るので (#2459)、「Start 済み」を前提にした driver へ
// 差し替わるとここで落ちる。
//
// **Concurrency の既定値は見ない。** 以前この test は「Concurrency<=0 なら
// 内部デフォルト 16 に落ちる」を確認する体だったが、mkqdriver の
// resolveQueueConcurrency が登録済みの全 queue を埋めるので
// Server.concurrency は実際には参照されない (変異検証: 16 を 1 にしても
// internal/queue 配下は全 package ok のまま)。空虚なアサーションを残さず、
// 実際に効く性質だけを見る。
func TestServer_ShutdownBeforeStartIsSafe(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	srv := queue.NewServer(newDriver(t))
	srv.Handle(queue.TaskTypeDeliver, func(_ context.Context, _ driver.Task) error {
		return nil
	})
	require.NotPanics(t, srv.Shutdown)
}

func flushTestRedis(t *testing.T) {
	t.Helper()
	if err := testRedis.Client.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("failed to flush redis: %v", err)
	}
}

// waitGroupTimeout waits for wg with a deadline. Returns true if completed.
func waitGroupTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// #534: EnqueueInbox は inbox queue に積まれる + Policy.MaxAttempts が
// 適用される。BullMQ semantics で MaxAttempts=N → MaxRetry=N-1。
func TestClient_EnqueueInbox(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueInbox(context.Background(), queue.InboxPayload{
		Body: []byte(`{"type":"Follow"}`),
		Host: "remote.example",
	}))

	tasks := listPending(t, newInspector(t), queue.InboxQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeInbox, tasks[0].Type)

	got, err := queue.DecodeInboxPayload(tasks[0].Payload)
	require.NoError(t, err)
	assert.Equal(t, "remote.example", got.Host)
	assert.JSONEq(t, `{"type":"Follow"}`, string(got.Body))
}

func TestClient_EnqueueInbox_PolicyMaxAttemptsApplied(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	c.SetPolicy(queue.InboxQueueName, queue.Policy{MaxAttempts: 8})

	require.NoError(t, c.EnqueueInbox(context.Background(), queue.InboxPayload{Body: []byte(`{}`)}))

	tasks := listPending(t, newInspector(t), queue.InboxQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, 8, jobAttempts(t, tasks[0]), "MaxAttempts=8 (BullMQ の総試行回数) がそのまま attempts になる")
}

func TestNewInboxTask_RoundTrip(t *testing.T) {
	payload := queue.InboxPayload{Body: []byte(`{"type":"Create"}`), Host: "h.example"}
	task := queue.NewInboxTask(payload)
	assert.Equal(t, queue.TaskTypeInbox, task.Type())
	got, err := queue.DecodeInboxPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload.Host, got.Host)
	assert.JSONEq(t, string(payload.Body), string(got.Body))
}

func TestDecodeInboxPayload_Invalid(t *testing.T) {
	_, err := queue.DecodeInboxPayload([]byte(`{not json`))
	assert.Error(t, err)
}

func TestNewExportTask_RoundTrip(t *testing.T) {
	payload := queue.ExportPayload{UserID: "u1", Type: "notes"}
	task := queue.NewExportTask(payload)
	assert.Equal(t, queue.TaskTypeExport, task.Type())

	decoded, err := queue.DecodeExportPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload, decoded)
}

func TestDecodeExportPayload_Invalid(t *testing.T) {
	_, err := queue.DecodeExportPayload([]byte(`{bad`))
	assert.Error(t, err)
}

func TestNewImportTask_RoundTrip(t *testing.T) {
	payload := queue.ImportPayload{UserID: "u1", Type: "following", FileID: "f1"}
	task := queue.NewImportTask(payload)
	assert.Equal(t, queue.TaskTypeImport, task.Type())

	decoded, err := queue.DecodeImportPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload, decoded)
}

func TestDecodeImportPayload_Invalid(t *testing.T) {
	_, err := queue.DecodeImportPayload([]byte(`{bad`))
	assert.Error(t, err)
}

func TestNewImportCustomEmojisTask_RoundTrip(t *testing.T) {
	payload := queue.ImportCustomEmojisPayload{UserID: "admin1", FileID: "f1"}
	task := queue.NewImportCustomEmojisTask(payload)
	assert.Equal(t, queue.TaskTypeImportCustomEmojis, task.Type())

	decoded, err := queue.DecodeImportCustomEmojisPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload, decoded)
}

func TestDecodeImportCustomEmojisPayload_Invalid(t *testing.T) {
	_, err := queue.DecodeImportCustomEmojisPayload([]byte(`{bad`))
	assert.Error(t, err)
}

func TestClient_EnqueueExport(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueExport(queue.ExportPayload{UserID: "u1", Type: "notes"}))

	tasks := listPending(t, newInspector(t), queue.ExportQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeExport, tasks[0].Type)
}

func TestClient_EnqueuePostScheduledNote(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	// caller は WithProcessIn で delay を渡す想定 (= scheduledAt - now)。
	require.NoError(t, c.EnqueuePostScheduledNote(
		queue.PostScheduledNotePayload{NoteDraftID: "d1"},
		driver.WithProcessIn(time.Hour),
	))

	// delayed task は scheduled state で観測される
	tasks, err := newInspector(t).ListScheduledTasks(queue.QueueName, 1, 100)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypePostScheduledNote, tasks[0].Type)
}

// #3121: 予約投稿の enqueue も policy の attempts を積む。積まないと mkq の
// `attempts <= 0` で **初回失敗がそのまま failed** になり、publish に到達する前の
// 一時的な DB 障害だけで予約投稿が失われる (publish そのものの失敗は processor が
// ack するので、ここで retry されるのは publish 前の失敗だけ)。
func TestClient_EnqueuePostScheduledNote_PolicyMaxAttemptsApplied(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	c.SetPolicy(queue.QueueName, queue.Policy{MaxAttempts: 8})

	require.NoError(t, c.EnqueuePostScheduledNote(queue.PostScheduledNotePayload{NoteDraftID: "d1"}))

	tasks := listPending(t, newInspector(t), queue.QueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, 8, jobAttempts(t, tasks[0]), "MaxAttempts=8 (BullMQ の総試行回数) がそのまま attempts になる")
}

func TestClient_ClearScheduledNote_RemovesMatchingTasks(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)
	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueuePostScheduledNote(
		queue.PostScheduledNotePayload{NoteDraftID: "target"},
		driver.WithProcessIn(time.Hour),
	))
	require.NoError(t, c.EnqueuePostScheduledNote(
		queue.PostScheduledNotePayload{NoteDraftID: "other"},
		driver.WithProcessIn(time.Hour),
	))

	require.NoError(t, c.ClearScheduledNote("target"))

	tasks, err := newInspector(t).ListScheduledTasks(queue.QueueName, 1, 100)
	require.NoError(t, err)
	require.Len(t, tasks, 1, "target だけ消えて other は残る")
	payload, err := queue.DecodePostScheduledNotePayload(tasks[0].Payload)
	require.NoError(t, err)
	assert.Equal(t, "other", payload.NoteDraftID)
}

// inspector 未配線 (= test 用 fake driver や zero-value Client) でも
// ClearScheduledNote は no-op で error しない。
func TestClient_ClearScheduledNote_NilInspectorIsNoop(t *testing.T) {
	// queue.NewClient 経由ではなく直接 zero-value Client を組んで
	// inspector=nil の状態を作る (= production では起こらないが defensive)。
	var c queue.Client
	assert.NoError(t, c.ClearScheduledNote("x"))
}

func TestClient_EnqueueImport(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueImport(queue.ImportPayload{UserID: "u1", Type: "following", FileID: "f1"}))

	tasks := listPending(t, newInspector(t), queue.ExportQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeImport, tasks[0].Type)
}

func TestClient_EnqueueImportCustomEmojis(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueImportCustomEmojis(queue.ImportCustomEmojisPayload{UserID: "admin1", FileID: "f1"}))

	tasks := listPending(t, newInspector(t), queue.ExportQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeImportCustomEmojis, tasks[0].Type)
}

func TestInspector_QueuesAndInfo(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	// エンキューしてキューを作成
	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`), KeyID: "k", KeyPEM: "p"}))

	insp := queue.NewInspector(newDriver(t))
	defer func() { _ = insp.Close() }()

	queues, err := insp.Queues()
	require.NoError(t, err)
	assert.Contains(t, queues, queue.QueueName)

	info, err := insp.GetQueueInfo(queue.QueueName)
	require.NoError(t, err)
	assert.Equal(t, queue.QueueName, info.Queue)
	assert.GreaterOrEqual(t, info.Size, 0)
}

func TestInspector_GetQueueInfo_NotFound(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	insp := queue.NewInspector(newDriver(t))
	defer func() { _ = insp.Close() }()

	_, err := insp.GetQueueInfo("nonexistent")
	assert.Error(t, err)
}

func TestInspector_ListPendingTasksAndGetTaskInfo(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{
		Inbox: "https://remote.example/inbox", Body: []byte(`{}`), KeyID: "k", KeyPEM: "p",
	}))

	insp := queue.NewInspector(newDriver(t))
	defer func() { _ = insp.Close() }()

	pending, err := insp.ListPendingTasks(queue.QueueName, 1, 30)
	require.NoError(t, err)
	require.NotEmpty(t, pending, "at least one pending task expected")
	taskID := pending[0].ID
	assert.Equal(t, queue.QueueName, pending[0].Queue)
	assert.NotZero(t, pending[0].Type)

	// GetTaskInfo で同じ ID を引けること
	info, err := insp.GetTaskInfo(queue.QueueName, taskID)
	require.NoError(t, err)
	assert.Equal(t, taskID, info.ID)
}

func TestInspector_ListActiveScheduledRetry_EmptyByDefault(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	// enqueue 後即 list。active/scheduled/retry は空でも err にならない。
	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{
		Inbox: "https://remote.example/inbox", Body: []byte(`{}`), KeyID: "k", KeyPEM: "p",
	}))

	insp := queue.NewInspector(newDriver(t))
	defer func() { _ = insp.Close() }()

	active, err := insp.ListActiveTasks(queue.QueueName, 1, 30)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(active), 0)

	scheduled, err := insp.ListScheduledTasks(queue.QueueName, 1, 30)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(scheduled), 0)

	retry, err := insp.ListRetryTasks(queue.QueueName, 1, 30)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(retry), 0)
}

func TestInspector_ListTasks_DefaultsClamped(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	insp := queue.NewInspector(newDriver(t))
	defer func() { _ = insp.Close() }()

	// page/pageSize が 0 以下でも default に丸められてエラーにならない。
	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()
	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{
		Inbox: "https://remote.example/inbox", Body: []byte(`{}`), KeyID: "k", KeyPEM: "p",
	}))

	rows, err := insp.ListPendingTasks(queue.QueueName, 0, 0)
	require.NoError(t, err)
	assert.NotNil(t, rows)

	rows, err = insp.ListPendingTasks(queue.QueueName, -5, 500)
	require.NoError(t, err)
	assert.NotNil(t, rows)
}

func TestInspector_GetTaskInfo_NotFound(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	insp := queue.NewInspector(newDriver(t))
	defer func() { _ = insp.Close() }()

	_, err := insp.GetTaskInfo(queue.QueueName, "nonexistent-id")
	assert.Error(t, err)
}

func TestNewWebPushTask_RoundTrip(t *testing.T) {
	payload := queue.WebPushPayload{
		UserID: "u1",
		Type:   "notification",
		Body:   []byte(`{"type":"mention"}`),
	}
	task := queue.NewWebPushTask(payload)
	assert.Equal(t, queue.TaskTypeWebPush, task.Type())

	decoded, err := queue.DecodeWebPushPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload.UserID, decoded.UserID)
	assert.Equal(t, payload.Type, decoded.Type)
	assert.JSONEq(t, string(payload.Body), string(decoded.Body))
}

func TestDecodeWebPushPayload_Invalid(t *testing.T) {
	_, err := queue.DecodeWebPushPayload([]byte(`{bad`))
	assert.Error(t, err)
}

func TestClient_EnqueueWebPush(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueWebPush(context.Background(), queue.WebPushPayload{
		UserID: "u1", Type: "notification", Body: []byte(`{"type":"mention"}`),
	}))

	tasks := listPending(t, newInspector(t), queue.PushQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeWebPush, tasks[0].Type)
}

func TestClient_EnqueueWebPush_ClosedDriverFails(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	d := newDriver(t)
	c := queue.NewClient(d)
	require.NoError(t, d.Close())
	err := c.EnqueueWebPush(context.Background(), queue.WebPushPayload{UserID: "u1", Type: "notification"})
	assert.Error(t, err)
}

func TestNewUserWebhookTask_RoundTrip(t *testing.T) {
	payload := queue.WebhookPayload{
		WebhookID: "h1",
		UserID:    "alice",
		EventType: "note",
		Body:      []byte(`{"type":"note"}`),
	}
	task := queue.NewUserWebhookTask(payload)
	assert.Equal(t, queue.TaskTypeUserWebhook, task.Type())

	decoded, err := queue.DecodeWebhookPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload.WebhookID, decoded.WebhookID)
	assert.Equal(t, payload.UserID, decoded.UserID)
	assert.Equal(t, payload.EventType, decoded.EventType)
	assert.JSONEq(t, string(payload.Body), string(decoded.Body))
}

func TestNewSystemWebhookTask_RoundTrip(t *testing.T) {
	payload := queue.WebhookPayload{
		WebhookID: "sh1",
		EventType: "userCreated",
		Body:      []byte(`{"user":"bob"}`),
	}
	task := queue.NewSystemWebhookTask(payload)
	assert.Equal(t, queue.TaskTypeSystemWebhook, task.Type())

	decoded, err := queue.DecodeWebhookPayload(task.Payload())
	require.NoError(t, err)
	assert.Equal(t, payload.WebhookID, decoded.WebhookID)
	assert.Empty(t, decoded.UserID)
}

func TestDecodeWebhookPayload_Invalid(t *testing.T) {
	_, err := queue.DecodeWebhookPayload([]byte(`{bad`))
	assert.Error(t, err)
}

func TestClient_EnqueueUserWebhook(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueUserWebhook(context.Background(), queue.WebhookPayload{
		WebhookID: "h1", UserID: "alice", EventType: "note", Body: []byte(`{}`),
	}))

	tasks := listPending(t, newInspector(t), queue.WebhookQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeUserWebhook, tasks[0].Type)
	// #2106 L59: upstream の総試行 4 回 (= WithMaxRetry(3) → BullMQ attempts=4)。
	assert.Equal(t, 4, jobAttempts(t, tasks[0]), "webhook は総試行 4 回")
}

func TestClient_EnqueueUserWebhook_ClosedDriverFails(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)
	d := newDriver(t)
	c := queue.NewClient(d)
	require.NoError(t, d.Close())
	err := c.EnqueueUserWebhook(context.Background(), queue.WebhookPayload{WebhookID: "h1"})
	assert.Error(t, err)
}

func TestClient_EnqueueSystemWebhook(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueSystemWebhook(context.Background(), queue.WebhookPayload{
		WebhookID: "sh1", EventType: "userCreated", Body: []byte(`{}`),
	}))

	tasks := listPending(t, newInspector(t), queue.WebhookQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, queue.TaskTypeSystemWebhook, tasks[0].Type)
	assert.Equal(t, 4, jobAttempts(t, tasks[0]), "本配送は本家と同じ総試行 4 回")
}

// 利用者の webhook も、テスト送信 (SingleAttempt) は 1 回だけ (#3278)。本配送は 4 回。
func TestClient_EnqueueUserWebhook_Attempts(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	for _, tt := range []struct {
		single bool
		want   int
	}{{false, 4}, {true, 1}} {
		flushTestRedis(t)
		c := queue.NewClient(newDriver(t))
		require.NoError(t, c.EnqueueUserWebhook(context.Background(), queue.WebhookPayload{
			WebhookID: "w1", UserID: "u1", EventType: "note", Body: []byte(`{}`), SingleAttempt: tt.single,
		}))
		tasks := listPending(t, newInspector(t), queue.WebhookQueueName)
		require.Len(t, tasks, 1)
		assert.Equal(t, tt.want, jobAttempts(t, tasks[0]), "SingleAttempt=%v", tt.single)
		_ = c.Close()
	}
}

// テスト送信 (SingleAttempt) は本家の attempts: 1 と同じく 1 回だけ (#3262)。
func TestClient_EnqueueSystemWebhook_SingleAttempt(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueSystemWebhook(context.Background(), queue.WebhookPayload{
		WebhookID: "sh1", EventType: "userCreated", Body: []byte(`{}`), SingleAttempt: true,
	}))

	tasks := listPending(t, newInspector(t), queue.WebhookQueueName)
	require.Len(t, tasks, 1)
	assert.Equal(t, 1, jobAttempts(t, tasks[0]))
}

func TestClient_EnqueueSystemWebhook_ClosedDriverFails(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)
	d := newDriver(t)
	c := queue.NewClient(d)
	require.NoError(t, d.Close())
	err := c.EnqueueSystemWebhook(context.Background(), queue.WebhookPayload{WebhookID: "sh1"})
	assert.Error(t, err)
}

func TestClient_EnqueueCleanRemoteNotes(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueCleanRemoteNotes())
}

func TestClient_EnqueueReactionFlush(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueReactionFlush())
}

func TestClient_EnqueueDeleteAccount(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueDeleteAccount(queue.DeleteAccountPayload{UserID: "u1"}))
}

func TestNewDeleteAccountTask_RoundTrip(t *testing.T) {
	payload := queue.DeleteAccountPayload{UserID: "u-delete"}
	task := queue.NewDeleteAccountTask(payload)
	require.Equal(t, queue.TaskTypeDeleteAccount, task.Type())
	got, err := queue.DecodeDeleteAccountPayload(task.Payload())
	require.NoError(t, err)
	require.Equal(t, "u-delete", got.UserID)
}

func TestDecodeDeleteAccountPayload_MalformedReturnsError(t *testing.T) {
	_, err := queue.DecodeDeleteAccountPayload([]byte(`not-json`))
	require.Error(t, err)
}

func TestClient_EnqueueUnfollow(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)

	c := queue.NewClient(newDriver(t))
	defer func() { _ = c.Close() }()

	require.NoError(t, c.EnqueueUnfollow(queue.UnfollowPayload{
		FollowerID: "rA", FolloweeID: "localA",
	}))
}

func TestNewUnfollowTask_RoundTrip(t *testing.T) {
	payload := queue.UnfollowPayload{FollowerID: "rA", FolloweeID: "localA"}
	task := queue.NewUnfollowTask(payload)
	require.Equal(t, queue.TaskTypeUnfollow, task.Type())
	got, err := queue.DecodeUnfollowPayload(task.Payload())
	require.NoError(t, err)
	require.Equal(t, "rA", got.FollowerID)
	require.Equal(t, "localA", got.FolloweeID)
}

func TestDecodeUnfollowPayload_MalformedReturnsError(t *testing.T) {
	_, err := queue.DecodeUnfollowPayload([]byte(`not-json`))
	require.Error(t, err)
}

func TestNewPostScheduledNoteTask_RoundTrip(t *testing.T) {
	payload := queue.PostScheduledNotePayload{NoteDraftID: "d1"}
	task := queue.NewPostScheduledNoteTask(payload)
	require.Equal(t, queue.TaskTypePostScheduledNote, task.Type())
	got, err := queue.DecodePostScheduledNotePayload(task.Payload())
	require.NoError(t, err)
	require.Equal(t, "d1", got.NoteDraftID)
}

func TestDecodePostScheduledNotePayload_MalformedReturnsError(t *testing.T) {
	_, err := queue.DecodePostScheduledNotePayload([]byte(`not-json`))
	require.Error(t, err)
}

// ensure errors package referenced for completeness in CI builds.
var _ = errors.New
