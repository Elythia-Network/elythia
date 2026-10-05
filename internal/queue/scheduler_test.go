package queue_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/redis/go-redis/v9"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
	"github.com/elythia-network/elythia/internal/testutil"
)

// newSchedulerForTest constructs a Scheduler against the mkq driver bound
// to the test Redis.
//
// **実 Redis が要る。** mkq の Register は cron 式を Go 側で解析したあと
// BullMQ の repeat ZSET へ upsert するので、driver の構築にも登録にも接続が
// いる (旧 asynq driver は Scheduler を遅延構築していたので接続不要だった)。
func newSchedulerForTest(t *testing.T) *queue.Scheduler {
	t.Helper()
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)
	s := queue.NewScheduler(newDriver(t))
	t.Cleanup(s.Shutdown)
	return s
}

// TestNewScheduler_RegistersWithoutError is a smoke test that the
// Scheduler wrapper builds and accepts a registration.
func TestNewScheduler_RegistersWithoutError(t *testing.T) {
	s := newSchedulerForTest(t)
	require.NotNil(t, s)
	require.NoError(t, s.RegisterChartJobs())
}

// TestScheduler_RegisterChartJobs_NoErr verifies the cron syntax + queue
// options are accepted.
func TestScheduler_RegisterChartJobs_NoErr(t *testing.T) {
	s := newSchedulerForTest(t)
	require.NoError(t, s.RegisterChartJobs())
}

// TestScheduler_RegisterMaintenanceJobs_NoErr verifies the #1563 cron jobs
// register without error (cron syntax + queue options accepted).
func TestScheduler_RegisterMaintenanceJobs_NoErr(t *testing.T) {
	s := newSchedulerForTest(t)
	require.NoError(t, s.RegisterCheckExpiredMutingsJob())
	require.NoError(t, s.RegisterCleanJob())
	require.NoError(t, s.RegisterCleanRemoteNotesJob())
	require.NoError(t, s.RegisterCheckModeratorsActivityJob())
	require.NoError(t, s.RegisterResendQuoteRequestsJob())
}

// TestScheduler_RegisterOrphanCleanupJobs_NoErr は孤児掃除 2 本の cron 式と
// queue オプションが driver に受理されることを見る (#2340 / #2722)。
//
// **登録失敗は起動時に warn ログを出すだけ** (`server.go` の jobs ループ) なので、
// cron 式のタイポは静かに「そのジョブだけ走らない」状態になる。ここで弾く。
func TestScheduler_RegisterOrphanCleanupJobs_NoErr(t *testing.T) {
	s := newSchedulerForTest(t)
	require.NoError(t, s.RegisterOrphanUserCleanupJob())
	require.NoError(t, s.RegisterOrphanAttachmentCleanupJob())
}

func TestMaintenanceQueueName_Const(t *testing.T) {
	// 既存定数と被らないこと (ベタ書きの同期ミスを防ぐ smoke)
	assert.Equal(t, "maintenance", queue.MaintenanceQueueName)
}

// **全ての cron 登録に 7 日の retention が付く (mkq#109)。** 付けないと定期
// ジョブの completed / failed が無期限に溜まる (本番の maintenance で
// 完了 57,614 件・失敗 9,319 件)。upstream の system queue と同じく
// completed / failed とも `{ age: 7 日 }`。
//
// **登録メソッドを手で並べない。** 引数を取らない `Register*` を reflect で
// 全部呼ぶので、新しい cron を足しても自動で対象に入る (共通の register を
// 通さずに `s.inner.Register` を直接呼ぶ形に戻すと、ここで落ちる)。
// 見るのは scheduler HASH の `opts` (BullMQ TS の worker が次の iteration を
// 積むときに読む template)。
func TestScheduler_EveryCronCarriesRetention(t *testing.T) {
	testutil.SkipIfNoDocker(t)
	flushTestRedis(t)
	// プラグインの cron (`RegisterPluginJob`) は引数を取るので reflect の対象外。
	// 専用キューを持つ driver を作って明示的に呼ぶ。
	d, err := mkqdriver.New(context.Background(), mkqdriver.Config{
		Redis:      redis.UniversalOptions{Addrs: []string{testRedis.Addr}},
		QueueNames: append(append([]string{}, mkqdriver.QueueNames...), queue.PluginQueueName("demo")),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	s := queue.NewScheduler(d)
	t.Cleanup(s.Shutdown)
	require.NoError(t, s.RegisterPluginJob("*/5 * * * *", "demo", "tick", nil))

	v := reflect.ValueOf(s)
	called := 0
	for i := range v.NumMethod() {
		m := v.Type().Method(i)
		if !strings.HasPrefix(m.Name, "Register") || m.Type.NumIn() != 1 {
			continue
		}
		out := v.Method(i).Call(nil)
		if err, _ := out[0].Interface().(error); err != nil {
			t.Fatalf("%s: %v", m.Name, err)
		}
		called++
	}
	require.Greater(t, called, 5, "Register* を拾えていない")

	ctx := context.Background()
	keys, err := testRedis.Client.Keys(ctx, "bull:*:repeat:*").Result()
	require.NoError(t, err)
	require.NotEmpty(t, keys, "scheduler HASH が 1 つも無い")
	require.Contains(t, keys, "bull:"+queue.PluginQueueName("demo")+":repeat:"+queue.PluginTaskType("demo", "tick"),
		"プラグインの cron も対象に入っていること")

	week := float64(queue.ScheduledJobRetention.Seconds())
	for _, key := range keys {
		raw, err := testRedis.Client.HGet(ctx, key, "opts").Result()
		require.NoError(t, err, "%s に opts template が無い", key)
		var tmpl map[string]any
		require.NoError(t, json.Unmarshal([]byte(raw), &tmpl), key)
		assert.Equal(t, map[string]any{"age": week}, tmpl["removeOnComplete"], key)
		assert.Equal(t, map[string]any{"age": week}, tmpl["removeOnFail"], key)
	}
}
