package mkqdriver

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/mkq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

func TestBuildRedisOptions_TCP(t *testing.T) {
	got := BuildRedisOptions(config.RedisOptions{
		Host: "redis.example",
		Port: 16379,
		Pass: "p",
		DB:   3,
	})
	if len(got.Addrs) != 1 || got.Addrs[0] != "redis.example:16379" {
		t.Fatalf("Addrs: got %v", got.Addrs)
	}
	if got.Password != "p" || got.DB != 3 {
		t.Fatalf("Auth fields: got %+v", got)
	}
}

func TestBuildRedisOptions_UnixSocket(t *testing.T) {
	got := BuildRedisOptions(config.RedisOptions{
		Host: "/tmp/redis.sock",
		Pass: "p",
	})
	if len(got.Addrs) != 1 || got.Addrs[0] != "/tmp/redis.sock" {
		t.Fatalf("Addrs: got %v", got.Addrs)
	}
}

func TestToMkqAddOptions_FullSet(t *testing.T) {
	o := driver.ApplyEnqueueOptions([]driver.EnqueueOption{
		driver.WithQueue("deliver"),
		driver.WithMaxRetry(4),
		driver.WithUnique(time.Hour),
		driver.WithProcessIn(2 * time.Second),
	})
	got := toMkqAddOptions(o, "ap:deliver", []byte("body"))
	if len(got) < 4 {
		// 4 options expected: WithJobName, WithAttempts, WithUnique, WithDelay
		t.Fatalf("expected at least 4 options, got %d", len(got))
	}
}

func TestToMkqAddOptions_DefaultsSkipped(t *testing.T) {
	got := toMkqAddOptions(driver.EnqueueOptions{}, "", nil)
	if len(got) != 0 {
		t.Fatalf("expected zero options for default EnqueueOptions, got %d", len(got))
	}
}

func TestToMkqAddOptions_JobNameOnly(t *testing.T) {
	got := toMkqAddOptions(driver.EnqueueOptions{}, "ap:deliver", nil)
	if len(got) != 1 {
		// Just WithJobName.
		t.Fatalf("expected 1 option, got %d", len(got))
	}
}

func TestUniqueKey_DistinctPayloads(t *testing.T) {
	// Two payloads with the same (queue, task type) must produce
	// different unique keys so deleteAccount-for-userA and
	// deleteAccount-for-userB don't collapse within the 24h window.
	a := uniqueKey("deliver", "delete", []byte(`{"userId":"a"}`))
	b := uniqueKey("deliver", "delete", []byte(`{"userId":"b"}`))
	if a == b {
		t.Fatalf("distinct payloads must yield distinct unique keys: %q == %q", a, b)
	}
	// Same triple → same key (idempotency).
	if a != uniqueKey("deliver", "delete", []byte(`{"userId":"a"}`)) {
		t.Fatalf("equal inputs must yield equal keys")
	}
}

func TestUniqueKey_DistinctQueues(t *testing.T) {
	// Same task type + payload routed to different queues must be
	// treated as independent dedup scopes (the dedup tuple is
	// (queue, type, payload) dedup tuple).
	a := uniqueKey("deliver", "delete", []byte("p"))
	b := uniqueKey("maintenance", "delete", []byte("p"))
	if a == b {
		t.Fatalf("different queues must yield different keys: %q == %q", a, b)
	}
}

func TestUniqueKey_NilPayload(t *testing.T) {
	// nil and empty payload should be equivalent (both hash to the
	// SHA-256 of zero bytes), and non-nil payloads must differ.
	a := uniqueKey("cron", "tick", nil)
	b := uniqueKey("cron", "tick", []byte{})
	if a != b {
		t.Fatalf("nil and empty payload must produce the same key")
	}
	c := uniqueKey("cron", "tick", []byte{0})
	if a == c {
		t.Fatalf("non-empty payload must yield a distinct key")
	}
}

// TestToMkqAddOptions_KeepFailedZeroSkipped: driver-neutral semantic で
// `WithKeepFailed(0)` は「retention 無し = unlimited 蓄積」だが、mkq
// native の `WithKeepFailed(0)` は「即時削除」で意味が真逆。translation
// 層で 0 を skip して semantic を揃えていることを pin する (#1184 review)。
func TestToMkqAddOptions_KeepFailedZeroSkipped(t *testing.T) {
	o := driver.EnqueueOptions{KeepFailed: 0, KeepFailedSet: true}
	got := toMkqAddOptions(o, "task", nil)
	// WithJobName 1 個のみ (= KeepFailed=0 は mkq に渡されない)。
	if len(got) != 1 {
		t.Fatalf("KeepFailed=0 must not emit mkq.WithKeepFailed, got %d options", len(got))
	}
}

// TestToMkqAddOptions_KeepFailedPositive: value>0 のときは
// mkq.WithKeepFailed として直訳される。
func TestToMkqAddOptions_KeepFailedPositive(t *testing.T) {
	o := driver.EnqueueOptions{KeepFailed: 500, KeepFailedSet: true}
	got := toMkqAddOptions(o, "task", nil)
	// WithJobName + WithKeepFailed の 2 個。
	if len(got) != 2 {
		t.Fatalf("KeepFailed>0 should emit mkq.WithKeepFailed, got %d options", len(got))
	}
}

// TestToMkqAddOptions_KeepCompletedZeroSkipped: KeepFailed と同じ 0-skip
// semantic を KeepCompleted 側でも維持する (mkq native の WithKeepCompleted(0)
// は「即時削除」で driver-neutral semantic と真逆、#1193 review)。
func TestToMkqAddOptions_KeepCompletedZeroSkipped(t *testing.T) {
	o := driver.EnqueueOptions{KeepCompleted: 0, KeepCompletedSet: true}
	got := toMkqAddOptions(o, "task", nil)
	if len(got) != 1 {
		t.Fatalf("KeepCompleted=0 must not emit mkq.WithKeepCompleted, got %d options", len(got))
	}
}

// TestToMkqAddOptions_KeepCompletedPositive: value>0 のときは
// mkq.WithKeepCompleted として直訳される。
func TestToMkqAddOptions_KeepCompletedPositive(t *testing.T) {
	o := driver.EnqueueOptions{KeepCompleted: 30, KeepCompletedSet: true}
	got := toMkqAddOptions(o, "task", nil)
	// WithJobName + WithKeepCompleted の 2 個。
	if len(got) != 2 {
		t.Fatalf("KeepCompleted>0 should emit mkq.WithKeepCompleted, got %d options", len(got))
	}
}

// TestToMkqAddOptions_AgePositive: KeepCompletedAge / KeepFailedAge の
// 両方が正値なら 2 個追加で計 3 個。0 は emit しない。
func TestToMkqAddOptions_AgePositive(t *testing.T) {
	o := driver.EnqueueOptions{
		KeepCompletedAge: 7 * 24 * time.Hour,
		KeepFailedAge:    7 * 24 * time.Hour,
	}
	got := toMkqAddOptions(o, "task", nil)
	// WithJobName + WithKeepCompletedAge + WithKeepFailedAge の 3 個。
	if len(got) != 3 {
		t.Fatalf("both ages should emit 2 options + WithJobName = 3 total, got %d", len(got))
	}
}

// TestToMkqAddOptions_AgeZeroSkipped: age=0 は「未指定」扱いで emit しない
// (BullMQ 仕様で age=0 を意味的に使う場面が無く、Set sentinel 不要)。
func TestToMkqAddOptions_AgeZeroSkipped(t *testing.T) {
	o := driver.EnqueueOptions{KeepCompletedAge: 0, KeepFailedAge: 0}
	got := toMkqAddOptions(o, "task", nil)
	if len(got) != 1 {
		t.Fatalf("age=0 must not emit options, got %d", len(got))
	}
}

// TestToMkqAddOptions_FullRetention: TS upstream の `{age: 7d, count: 30}`
// /  `{age: 7d, count: 100}` 互換を 1 ジョブで両側設定したケース。
// WithJobName + WithKeepFailed + WithKeepCompleted + WithKeepCompletedAge +
// WithKeepFailedAge の 5 options が emit される。
func TestToMkqAddOptions_FullRetention(t *testing.T) {
	o := driver.EnqueueOptions{
		KeepFailed:       100,
		KeepFailedSet:    true,
		KeepCompleted:    30,
		KeepCompletedSet: true,
		KeepCompletedAge: 7 * 24 * time.Hour,
		KeepFailedAge:    7 * 24 * time.Hour,
	}
	got := toMkqAddOptions(o, "task", nil)
	if len(got) != 5 {
		t.Fatalf("full retention should emit 4 retention options + WithJobName = 5 total, got %d", len(got))
	}
}

func TestToMkqAddOptions_MaxRetryRequiresExplicit(t *testing.T) {
	// MaxRetrySet=false → default attempts; no option emitted.
	got := toMkqAddOptions(driver.EnqueueOptions{MaxRetry: 3}, "task", nil)
	// 1 option only (WithJobName); MaxRetry must NOT be added.
	if len(got) != 1 {
		t.Fatalf("MaxRetry without MaxRetrySet must be skipped, got %d", len(got))
	}
}

func TestMkqTask_Wrapper(t *testing.T) {
	w := mkqTask{taskType: "x", payload: []byte("body")}
	if w.Type() != "x" {
		t.Fatalf("Type: got %q", w.Type())
	}
	if string(w.Payload()) != "body" {
		t.Fatalf("Payload: got %q", string(w.Payload()))
	}
}

func TestBuildRedisOptions_PoolSize(t *testing.T) {
	pool := 64
	got := BuildRedisOptions(config.RedisOptions{
		Host:     "redis.example",
		Port:     6379,
		PoolSize: &pool,
	})
	if got.PoolSize != 64 {
		t.Fatalf("PoolSize: want 64, got %d", got.PoolSize)
	}
}

func TestBuildRedisOptions_PoolSizeNilKeepsDefault(t *testing.T) {
	got := BuildRedisOptions(config.RedisOptions{
		Host: "redis.example",
		Port: 6379,
	})
	if got.PoolSize != 0 {
		// 0 = "use go-redis default"
		t.Fatalf("PoolSize: want 0 (default), got %d", got.PoolSize)
	}
}

func TestBuildRedisOptions_PoolSizeZeroIgnored(t *testing.T) {
	pool := 0
	got := BuildRedisOptions(config.RedisOptions{
		Host:     "redis.example",
		Port:     6379,
		PoolSize: &pool,
	})
	if got.PoolSize != 0 {
		t.Fatalf("PoolSize: want 0, got %d", got.PoolSize)
	}
}

func TestJobToSummary_Nil(t *testing.T) {
	if got := jobToSummary("q", "wait", nil, nil); got != nil {
		t.Fatalf("nil job must yield nil summary, got %#v", got)
	}
}

func TestJobToSummary_FramedPayload(t *testing.T) {
	job := &mkq.Job[framedPayload]{
		ID:           "1",
		Name:         "ap:deliver",
		Data:         framedPayload{Type: "ap:deliver", Body: []byte("body")},
		Timestamp:    time.Unix(1700000000, 0),
		AttemptsMade: 2,
	}
	state := &mkq.JobState{
		ProcessedOn:  time.Unix(1700000010, 0),
		FinishedOn:   time.Unix(1700000020, 0),
		FailedReason: "boom",
	}
	got := jobToSummary("deliver", "wait", job, state)
	if got == nil {
		t.Fatal("got nil")
	}
	if got.Type != "ap:deliver" {
		t.Fatalf("Type: got %q", got.Type)
	}
	if got.Retried != 2 {
		t.Fatalf("Retried: got %d", got.Retried)
	}
	if string(got.Payload) != "body" {
		t.Fatalf("Payload: got %q", got.Payload)
	}
	if got.LastErr != "boom" {
		t.Fatalf("LastErr: got %q", got.LastErr)
	}
	if got.LastFailedAt != state.FinishedOn {
		t.Fatalf("LastFailedAt: got %v", got.LastFailedAt)
	}
	// 失敗ジョブの場合、CompletedAt は zero のまま。
	// 失敗時刻は LastFailedAt 側に出すべきで、CompletedAt 列に出すと
	// admin UI が「失敗なのに完了した」風に表示されてしまう。
	if !got.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt must stay zero for failed jobs, got %v", got.CompletedAt)
	}
	// NextProcessAt は「次に処理される時刻」(将来) のセマンティクスに合わせる
	// ため意図的に未設定にしている (mkq には next-retry timestamp が
	// 存在しない)。past-time の ProcessedOn を入れると意味的に逆になる
	// ので zero のままを保証する。
	if !got.NextProcessAt.IsZero() {
		t.Fatalf("NextProcessAt must stay zero, got %v", got.NextProcessAt)
	}
}

func TestJobToSummary_CompletedSuccess(t *testing.T) {
	// 成功完了したジョブだけ CompletedAt が埋まる。
	job := &mkq.Job[framedPayload]{
		ID:   "1",
		Name: "ap:deliver",
		Data: framedPayload{Type: "ap:deliver"},
	}
	finished := time.Unix(1700000020, 0)
	state := &mkq.JobState{FinishedOn: finished}
	got := jobToSummary("deliver", "completed", job, state)
	if got.CompletedAt != finished {
		t.Fatalf("CompletedAt: got %v want %v", got.CompletedAt, finished)
	}
	if !got.LastFailedAt.IsZero() {
		t.Fatalf("LastFailedAt must stay zero on success, got %v", got.LastFailedAt)
	}
}

func TestJobToSummary_ProcessedBy(t *testing.T) {
	// mkq `pb` (BullMQ job.processedBy = 処理 worker 名) を summary に通す (#1552)。
	job := &mkq.Job[framedPayload]{
		ID:          "1",
		Name:        "ap:deliver",
		Data:        framedPayload{Type: "ap:deliver"},
		ProcessedBy: "worker-host-7",
	}
	got := jobToSummary("deliver", "completed", job, nil)
	if got.ProcessedBy != "worker-host-7" {
		t.Fatalf("ProcessedBy: got %q want %q", got.ProcessedBy, "worker-host-7")
	}

	// 未処理 job は空のまま。
	job2 := &mkq.Job[framedPayload]{ID: "2", Data: framedPayload{Type: "x"}}
	if got2 := jobToSummary("deliver", "wait", job2, nil); got2.ProcessedBy != "" {
		t.Fatalf("never-run job ProcessedBy must be empty, got %q", got2.ProcessedBy)
	}
}

func TestJobToSummary_TypeFallbackToJobName(t *testing.T) {
	// Foreign jobs (created by BullMQ TS without our framing) have an
	// empty Data.Type. The summary must fall back to Job.Name so admin
	// UIs at least see a non-blank label.
	job := &mkq.Job[framedPayload]{
		ID:   "1",
		Name: "foreign:type",
		Data: framedPayload{}, // no Type
	}
	got := jobToSummary("deliver", "wait", job, nil)
	if got.Type != "foreign:type" {
		t.Fatalf("Type fallback: got %q", got.Type)
	}
}

func TestJobToSummary_NilState(t *testing.T) {
	job := &mkq.Job[framedPayload]{ID: "1", Data: framedPayload{Type: "x"}}
	got := jobToSummary("q", "wait", job, nil)
	if got == nil {
		t.Fatal("got nil")
	}
	if !got.LastFailedAt.IsZero() {
		t.Fatalf("LastFailedAt should be zero, got %v", got.LastFailedAt)
	}
}

func TestClient_CloseIsNoop(t *testing.T) {
	// Driver-level Close handles the underlying *mkq.Client; per the
	// driver split, queue.Client.Close from the layered API forwards
	// to mkqdriver.Client.Close which must be a clean no-op.
	c := &Client{driver: nil}
	if err := c.Close(); err != nil {
		t.Fatalf("Client.Close: %v", err)
	}
}

func TestInspector_CloseIsNoop(t *testing.T) {
	// Same contract as Client.Close — the underlying client is owned
	// by Driver.
	i := &Inspector{driver: nil}
	if err := i.Close(); err != nil {
		t.Fatalf("Inspector.Close: %v", err)
	}
}

func TestDeriveJobState(t *testing.T) {
	finished := time.Unix(1700000020, 0)
	processed := time.Unix(1700000010, 0)
	tests := []struct {
		name string
		st   *mkq.JobState
		want string
	}{
		{"nil → wait", nil, string(mkq.JobBucketWait)},
		{"empty → wait", &mkq.JobState{}, string(mkq.JobBucketWait)},
		{"processed only → active", &mkq.JobState{ProcessedOn: processed}, string(mkq.JobBucketActive)},
		{"finished + no error → completed", &mkq.JobState{ProcessedOn: processed, FinishedOn: finished}, string(mkq.JobBucketCompleted)},
		{"finished + error → failed", &mkq.JobState{ProcessedOn: processed, FinishedOn: finished, FailedReason: "boom"}, string(mkq.JobBucketFailed)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveJobState(tt.st); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewDispatchHandler_NoHandler(t *testing.T) {
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{}, "deliver", nil, -1, nil)
	job := &mkq.Job[framedPayload]{Data: framedPayload{Type: "missing"}}
	_, err := dispatch(context.Background(), job)
	if err == nil || !errIs(err, mkq.ErrUnrecoverable) {
		t.Fatalf("missing handler must wrap mkq.ErrUnrecoverable, got %v", err)
	}
}

func TestNewDispatchHandler_SkipRetryConverts(t *testing.T) {
	called := 0
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"x": func(_ context.Context, _ driver.Task) error {
			called++
			return driver.ErrSkipRetry
		},
	}, "deliver", nil, -1, nil)
	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{Data: framedPayload{Type: "x"}})
	if err == nil || !errIs(err, mkq.ErrUnrecoverable) || !errIs(err, driver.ErrSkipRetry) {
		t.Fatalf("ErrSkipRetry must wrap both driver.ErrSkipRetry and mkq.ErrUnrecoverable, got %v", err)
	}
	if called != 1 {
		t.Fatalf("handler called %d times, want 1", called)
	}
}

func TestNewDispatchHandler_NormalReturn(t *testing.T) {
	called := 0
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"x": func(_ context.Context, task driver.Task) error {
			called++
			if task.Type() != "x" {
				t.Errorf("Type: %q", task.Type())
			}
			if string(task.Payload()) != "body" {
				t.Errorf("Payload: %q", task.Payload())
			}
			return nil
		},
	}, "deliver", nil, -1, nil)
	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{Data: framedPayload{Type: "x", Body: []byte("body")}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if called != 1 {
		t.Fatalf("handler called %d times", called)
	}
}

// errIs aliases errors.Is so the dispatch tests read consistently.
func errIs(err, target error) bool { return errors.Is(err, target) }

func TestQueueDefaultConcurrency(t *testing.T) {
	cases := map[string]int{
		"inbox":       16,
		"deliver":     16,
		"webhook":     4,
		"push":        4,
		"export":      2,
		"maintenance": 2,
		"custom":      unknownQueueConcurrency, // unknown queue falls back to the modest default
	}
	for name, want := range cases {
		if got := queueDefaultConcurrency(name); got != want {
			t.Errorf("queueDefaultConcurrency(%q) = %d, want %d", name, got, want)
		}
	}
}

func TestResolveQueueConcurrency_DefaultsAndOverrides(t *testing.T) {
	queues := []string{"inbox", "deliver", "push", "export", "webhook", "maintenance"}
	override := map[string]int{
		"inbox":   32, // override wins over the default 16
		"deliver": 0,  // <=0 is ignored, leaving the default 16
	}
	got := resolveQueueConcurrency(queues, override)
	want := map[string]int{
		"inbox":       32,
		"deliver":     16,
		"push":        4,
		"export":      2,
		"webhook":     4,
		"maintenance": 2,
	}
	if len(got) != len(want) {
		t.Fatalf("resolveQueueConcurrency len = %d, want %d (%v)", len(got), len(want), got)
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("queue %q concurrency = %d, want %d", name, got[name], w)
		}
	}
}

// #1374: 旧均等割り (total 16 / 6 queue = 2) では inbox / deliver が starve して
// いた。default 状態で hot queue が 2 より十分大きいことを保証する回帰テスト。
func TestResolveQueueConcurrency_HotQueuesNotStarved(t *testing.T) {
	queues := []string{"inbox", "deliver", "push", "export", "webhook", "maintenance"}
	got := resolveQueueConcurrency(queues, nil)
	if got["inbox"] <= 2 {
		t.Errorf("inbox default concurrency = %d, want > 2 (旧均等割りの starve 回帰)", got["inbox"])
	}
	if got["deliver"] <= 2 {
		t.Errorf("deliver default concurrency = %d, want > 2 (旧均等割りの starve 回帰)", got["deliver"])
	}
}

func TestWorkerPoolSize(t *testing.T) {
	queues := []string{"inbox", "deliver", "push", "export", "webhook", "maintenance"}
	goDefault := 10 * runtime.GOMAXPROCS(0)

	// worker 合計: 16+16+4+4+2+2 = 44。
	// これに liveness を追跡するキュー (inbox / deliver / push / webhook) の
	// 隔離許容ぶんが乗る: max(16,4)+max(16,4)+max(4,4)+max(4,4) = 40。
	// export / maintenance は追跡対象外なので乗らない (#2657)。
	const workers = 44
	const quarantineAllowance = 16 + 16 + 4 + 4
	if got, want := workerPoolSize(queues, nil, 0), max(workers+quarantineAllowance+poolHeadroom, goDefault); got != want {
		t.Errorf("workerPoolSize(default) = %d, want %d", got, want)
	}

	// override は合計に反映され、pool もそれに追従する。
	override := map[string]int{"deliver": 128}
	wantOverride := max((16+128+4+4+2+2)+(16+128+4+4)+poolHeadroom, goDefault)
	if got := workerPoolSize(queues, override, 0); got != wantOverride {
		t.Errorf("workerPoolSize(deliver=128) = %d, want %d", got, wantOverride)
	}

	// liveness を切ると隔離ぶんは不要になり、以前の計算に戻る。
	if got, want := workerPoolSize(queues, nil, -1), max(workers+poolHeadroom, goDefault); got != want {
		t.Errorf("workerPoolSize(tracking off) = %d, want %d", got, want)
	}

	// 明示値は全キューに効くので、export / maintenance のぶんも乗る。
	allTracked := max(workers+(16+16+4+4+4+4)+poolHeadroom, goDefault)
	if got := workerPoolSize(queues, nil, time.Minute); got != allTracked {
		t.Errorf("workerPoolSize(all tracked) = %d, want %d", got, allTracked)
	}

	// pool は全 worker を同時に賄える (= 合計 worker 数以上) ことを保証する。
	conc := resolveQueueConcurrency(queues, nil)
	sum := 0
	for _, c := range conc {
		sum += c
	}
	if got := workerPoolSize(queues, nil, 0); got < sum {
		t.Errorf("workerPoolSize = %d, must be >= total workers %d", got, sum)
	}

	// go-redis default を下回らない (multi-core での pool 縮小退行を防ぐ)。
	if got := workerPoolSize(queues, nil, 0); got < goDefault {
		t.Errorf("workerPoolSize = %d, must be >= go-redis default %d", got, goDefault)
	}
}

func TestStuckAfterForQueue(t *testing.T) {
	if got := stuckAfterForQueue("inbox", 0); got != defaultStuckAfter {
		t.Errorf("stuckAfterForQueue(inbox, 0) = %v, want %v", got, defaultStuckAfter)
	}
	if got := stuckAfterForQueue("export", 0); got != 0 {
		t.Errorf("stuckAfterForQueue(export, 0) = %v, want 0", got)
	}
	if got := stuckAfterForQueue("export", time.Minute); got != time.Minute {
		t.Errorf("stuckAfterForQueue(export, 1m) = %v, want 1m", got)
	}
	if got := stuckAfterForQueue("inbox", -1); got != 0 {
		t.Errorf("stuckAfterForQueue(inbox, -1) = %v, want 0", got)
	}
}

// TestPoolSizeForWorkers は floor / headroom ロジックを GOMAXPROCS 非依存で
// 検証する。workerPoolSize は floor に 10×GOMAXPROCS を渡すためマシンの core
// 数で floor-taken 分岐が実行されたりされなかったりするが、floor を引数化した
// 本ヘルパーなら両分岐を決定的に covered にできる。
func TestPoolSizeForWorkers(t *testing.T) {
	cases := []struct {
		name             string
		workerSum, floor int
		want             int
	}{
		{"floor applies when workers below floor", 44, 80, 80}, // max(44+8, 80)
		{"workers+headroom above floor", 160, 80, 168},         // max(160+8, 80)
		{"small floor ignored", 44, 20, 52},                    // max(44+8, 20)
		{"equal to floor", 72, 80, 80},                         // max(72+8, 80)
	}
	for _, tc := range cases {
		if got := poolSizeForWorkers(tc.workerSum, tc.floor); got != tc.want {
			t.Errorf("%s: poolSizeForWorkers(%d, %d) = %d, want %d",
				tc.name, tc.workerSum, tc.floor, got, tc.want)
		}
	}
}

// Inspector.Queues は mk-go が管理する (Define 済みの) queue 名だけを順序通りに
// 返し、mkq の共有 Redis registry (drop-in 時に旧 Misskey TS の foreign queue を
// 含みうる) は参照しない (#1393)。driver に client/rdb を持たせず構築できることが、
// registry を引かない (= nil client で panic しない) ことの証左でもある。
func TestInspector_Queues_ReturnsManagedNamesOnly(t *testing.T) {
	d := &Driver{queueNames: []string{"deliver", "inbox", "export"}}
	ins := &Inspector{driver: d}

	got, err := ins.Queues()
	if err != nil {
		t.Fatalf("Queues: %v", err)
	}
	want := []string{"deliver", "inbox", "export"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Queues = %v, want %v", got, want)
	}

	// 返り値は copy で、mutate しても driver の内部状態に影響しない。
	got[0] = "mutated"
	got2, err := ins.Queues()
	if err != nil {
		t.Fatalf("Queues (2): %v", err)
	}
	if got2[0] != "deliver" {
		t.Fatalf("driver state mutated via returned slice: got2[0]=%q", got2[0])
	}
}

// recordingObserver captures driver.Observer callbacks for assertions.
type recordingObserver struct {
	mu       sync.Mutex
	waits    []time.Duration
	procs    []time.Duration
	failures []bool
	queues   []string
}

func (o *recordingObserver) ObserveDispatchWait(queue string, d time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.queues = append(o.queues, queue)
	o.waits = append(o.waits, d)
}

func (o *recordingObserver) ObserveProcessing(queue string, d time.Duration, failed bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.procs = append(o.procs, d)
	o.failures = append(o.failures, failed)
}

// observer を配線すると dispatch wait と処理時間の両方が観測されること (#2277)。
// dispatch wait は job.Timestamp (BullMQ の作成時刻) からの経過。
func TestDispatchHandler_ObservesTimings(t *testing.T) {
	obs := &recordingObserver{}
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"t": func(ctx context.Context, task driver.Task) error {
			time.Sleep(2 * time.Millisecond)
			return nil
		},
	}, "deliver", obs, -1, nil)

	job := &mkq.Job[framedPayload]{
		Data:      framedPayload{Type: "t"},
		Timestamp: time.Now().Add(-50 * time.Millisecond),
	}
	_, err := dispatch(context.Background(), job)
	require.NoError(t, err)

	require.Len(t, obs.waits, 1)
	assert.Equal(t, []string{"deliver"}, obs.queues)
	assert.GreaterOrEqual(t, obs.waits[0], 50*time.Millisecond)
	require.Len(t, obs.procs, 1)
	assert.GreaterOrEqual(t, obs.procs[0], time.Millisecond)
	assert.Equal(t, []bool{false}, obs.failures)
}

// handler が error を返したら failed=true で観測されること。
func TestDispatchHandler_ObservesFailure(t *testing.T) {
	obs := &recordingObserver{}
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"t": func(ctx context.Context, task driver.Task) error { return errors.New("boom") },
	}, "inbox", obs, -1, nil)

	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{
		Data: framedPayload{Type: "t"}, Timestamp: time.Now(),
	})
	require.Error(t, err)
	assert.Equal(t, []bool{true}, obs.failures)
}

// retry (AttemptsMade > 0) では dispatch wait を観測しない。job.Timestamp は
// 作成時刻のままなので、含めると backoff 待ちが「詰まり」として混ざる。
// 処理時間は毎試行観測する。
func TestDispatchHandler_SkipsWaitOnRetry(t *testing.T) {
	obs := &recordingObserver{}
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"t": func(ctx context.Context, task driver.Task) error { return nil },
	}, "webhook", obs, -1, nil)

	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{
		Data:         framedPayload{Type: "t"},
		Timestamp:    time.Now().Add(-90 * time.Second),
		AttemptsMade: 2,
	})
	require.NoError(t, err)
	assert.Empty(t, obs.waits, "backoff 待ちを dispatch wait として数えない")
	assert.Len(t, obs.procs, 1)
}

// Timestamp が zero の job では dispatch wait を観測しない (誤った巨大値を
// 出さない)。処理時間は観測する。
func TestDispatchHandler_SkipsWaitWithoutTimestamp(t *testing.T) {
	obs := &recordingObserver{}
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"t": func(ctx context.Context, task driver.Task) error { return nil },
	}, "deliver", obs, -1, nil)

	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{Data: framedPayload{Type: "t"}})
	require.NoError(t, err)
	assert.Empty(t, obs.waits)
	assert.Len(t, obs.procs, 1)
}

// observer 未配線でも従来どおり動くこと (hot path の nil guard)。
func TestDispatchHandler_NilObserver(t *testing.T) {
	called := false
	dispatch := newDispatchHandler(map[string]driver.HandlerFunc{
		"t": func(ctx context.Context, task driver.Task) error { called = true; return nil },
	}, "deliver", nil, -1, nil)

	_, err := dispatch(context.Background(), &mkq.Job[framedPayload]{
		Data: framedPayload{Type: "t"}, Timestamp: time.Now(),
	})
	require.NoError(t, err)
	assert.True(t, called)
}

// SetObserver は Start が closure に snapshot するので Start 前に呼ぶ契約。
// ここでは設定した observer が Server に保持されることだけを確認する
// (実際の dispatch 経路は TestDispatchHandler_* が直接検証する)。
func TestServer_SetObserver(t *testing.T) {
	s := &Server{handlers: map[string]driver.HandlerFunc{}}
	assert.Nil(t, s.observer)

	obs := &recordingObserver{}
	s.SetObserver(obs)
	assert.Same(t, obs, s.observer)

	// 上書きできること (最後の caller が勝つ)。
	s.SetObserver(nil)
	assert.Nil(t, s.observer)
}
