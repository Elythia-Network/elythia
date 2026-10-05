package server

import (
	"context"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/queue/driver/mkqdriver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intp(v int) *int { return &v }

// buildQueueDriver は config 側の検証を通り抜けた driver 名でも mkq を
// 黙って起動しない (#2985)。未知の名前は Redis へ触る前に落ちるので、
// この 2 ケースは実接続を要らない。
//
// **config 側と二重に持つのは意図的。** `resolveJobQueueDriver` を通らない
// 経路 (テストが *config.Config を直接組む等) でも、driver 名が実態と違えば
// 起動しないことを固定する。
func TestBuildQueueDriver_RejectsUnknownDriver(t *testing.T) {
	for _, name := range []string{"asynq", "mkqq"} {
		t.Run(name, func(t *testing.T) {
			d, err := buildQueueDriver(context.Background(), &config.Config{JobQueueDriver: name}, nil)
			require.Error(t, err)
			assert.Nil(t, d)
			assert.Contains(t, err.Error(), name)
		})
	}
}

// #495 / #534 / #2403: cfg の per-queue concurrency がそのまま queue 名 →
// worker 数の map に積まれる。relationship は #2403 で専用 queue を持つように
// なったので、それ以前の「config だけ受けて no-op」ではなく forward される。
func TestPerQueueConcurrencyFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want map[string]int
	}{
		{"empty", &config.Config{}, nil},
		{"deliverOnly", &config.Config{DeliverJobConcurrency: intp(8)}, map[string]int{"deliver": 8}},
		{"zeroIgnored", &config.Config{DeliverJobConcurrency: intp(0)}, nil},
		{"negativeIgnored", &config.Config{DeliverJobConcurrency: intp(-1)}, nil},
		{"relationshipOnly", &config.Config{RelationshipJobConcurrency: intp(6)}, map[string]int{"relationship": 6}},
		{"relationshipZeroIgnored", &config.Config{RelationshipJobConcurrency: intp(0)}, nil},
		{"relationshipNegativeIgnored", &config.Config{RelationshipJobConcurrency: intp(-1)}, nil},
		{"allThree", &config.Config{
			DeliverJobConcurrency:      intp(8),
			InboxJobConcurrency:        intp(12),
			RelationshipJobConcurrency: intp(6),
		}, map[string]int{"deliver": 8, "inbox": 12, "relationship": 6}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, perQueueConcurrencyFromConfig(tc.cfg))
		})
	}
}

func TestPerQueueRatesFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want map[string]int
	}{
		{"empty", &config.Config{}, nil},
		{"deliverOnly", &config.Config{DeliverJobPerSec: intp(50)}, map[string]int{"deliver": 50}},
		{"zeroIgnored", &config.Config{DeliverJobPerSec: intp(0)}, nil},
		{"relationshipOnly", &config.Config{RelationshipJobPerSec: intp(20)}, map[string]int{"relationship": 20}},
		{"relationshipZeroIgnored", &config.Config{RelationshipJobPerSec: intp(0)}, nil},
		{"allThree", &config.Config{
			DeliverJobPerSec:      intp(50),
			InboxJobPerSec:        intp(30),
			RelationshipJobPerSec: intp(20),
		}, map[string]int{"deliver": 50, "inbox": 30, "relationship": 20}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, perQueueRatesFromConfig(tc.cfg))
		})
	}
}

// applyClientPolicies はその後の EnqueueDeliver で WithMaxRetry default が
// 効くよう Client に Policy を登録する。Policy が無効 (0) ならば SetPolicy
// は呼ばれない (PolicyMap が nil のままで PolicyFor がゼロ Policy を返す
// fast-path が維持される)。
func TestApplyClientPolicies_DeliverMaxAttempts(t *testing.T) {
	c := queue.NewClient(&stubDriver{client: &recordingDriverClient{}})
	defer func() { _ = c.Close() }()

	cfg := &config.Config{DeliverJobMaxAttempts: intp(5)}
	applyClientPolicies(c, cfg)

	// reflectionで取れないので indirect に: SetPolicy がもう 1 度
	// 呼ばれて上書きできることだけ確認する。
	c.SetPolicy(queue.QueueName, queue.Policy{MaxAttempts: 99})
}

// 0 や nil なら applyClientPolicies は SetPolicy を呼ばないが、
// Client は引き続き正常に動く (panic しない / EnqueueDeliver する経路は
// 別 testで verify 済み)。
func TestApplyClientPolicies_NoOpForZero(t *testing.T) {
	c := queue.NewClient(&stubDriver{client: &recordingDriverClient{}})
	defer func() { _ = c.Close() }()

	require.NotPanics(t, func() {
		applyClientPolicies(c, &config.Config{})
		applyClientPolicies(c, &config.Config{DeliverJobMaxAttempts: intp(0)})
	})
}

// recordingDriverClient is a minimal driver.Client implementation that
// captures the last Enqueue call's options. server _test 内に持つ用途は
// applyClientPolicies が対象 queue (maintenance を除く 7 つ) に Policy を
// 伝搬するのを enqueue 経由で観測すること (queue.Client.policyFor が
// unexported のため)。
type recordingDriverClient struct {
	lastTaskType string
	lastOpts     driver.EnqueueOptions
}

func (r *recordingDriverClient) Enqueue(_ context.Context, taskType string, _ []byte, opts ...driver.EnqueueOption) error {
	r.lastTaskType = taskType
	r.lastOpts = driver.ApplyEnqueueOptions(opts)
	return nil
}

func (r *recordingDriverClient) Close() error { return nil }

// stubDriver wraps just enough of driver.Driver to satisfy queue.NewClient.
type stubDriver struct {
	client driver.Client
}

func (s *stubDriver) Client() driver.Client        { return s.client }
func (s *stubDriver) Inspector() driver.Inspector  { return nil }
func (s *stubDriver) Server() driver.Server        { return nil }
func (s *stubDriver) Scheduler() driver.Scheduler  { return nil }
func (s *stubDriver) Close() error                 { return nil }
func (s *stubDriver) WorkerCount(_ string) int     { return 0 }
func (s *stubDriver) Resize(_ string, _ int) error { return driver.ErrResizeNotSupported }

// TestApplyClientPolicies_AllFiveQueues verifies that applyClientPolicies
// propagates retention defaults onto all five application queues (deliver /
// inbox / export / push / webhook) — not just deliver / inbox like before
// #1193。各 queue で enqueue 後に recordingDriverClient が見る driver
// options が default の `{age: 7d, count: 30}` / `{age: 7d, count: 1000}`
// を含むことを assert する。
func TestApplyClientPolicies_AllFiveQueues(t *testing.T) {
	rec := &recordingDriverClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	applyClientPolicies(c, &config.Config{})

	cases := []struct {
		name string
		enq  func() error
	}{
		{
			name: "deliver",
			enq: func() error {
				return c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)})
			},
		},
		{
			name: "inbox",
			enq: func() error {
				return c.EnqueueInbox(context.Background(), queue.InboxPayload{Body: []byte(`{}`)})
			},
		},
		{
			name: "export",
			enq: func() error {
				return c.EnqueueExport(queue.ExportPayload{UserID: "u", Type: "notes"})
			},
		},
		{
			name: "push",
			enq: func() error {
				return c.EnqueueWebPush(context.Background(), queue.WebPushPayload{})
			},
		},
		{
			name: "webhook",
			enq: func() error {
				return c.EnqueueUserWebhook(context.Background(), queue.WebhookPayload{})
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, tt.enq())
			assert.Equal(t, defaultKeepCompleted, rec.lastOpts.KeepCompleted, "KeepCompleted default propagated")
			assert.True(t, rec.lastOpts.KeepCompletedSet, "KeepCompletedSet must be propagated")
			assert.Equal(t, defaultKeepFailed, rec.lastOpts.KeepFailed)
			assert.True(t, rec.lastOpts.KeepFailedSet)
			assert.Equal(t, defaultKeepCompletedAge, rec.lastOpts.KeepCompletedAge)
			assert.Equal(t, defaultKeepFailedAge, rec.lastOpts.KeepFailedAge)
		})
	}
}

// TestApplyClientPolicies_OperatorOptOut: deliverJobKeepCompleted=0 を
// operator が明示すると completed retention が 0 (= unlimited 蓄積に
// opt-out) として伝わり、driver opt 側で WithKeepCompleted が emit
// されなくなることを pin (#1193 review feedback)。
func TestApplyClientPolicies_OperatorOptOut(t *testing.T) {
	rec := &recordingDriverClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	applyClientPolicies(c, &config.Config{DeliverJobKeepCompleted: intp(0)})

	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)}))

	assert.False(t, rec.lastOpts.KeepCompletedSet, "operator opt-out must NOT set KeepCompletedSet")
	assert.Equal(t, 0, rec.lastOpts.KeepCompleted)
	// failed 側は default が残るはず。
	assert.Equal(t, defaultKeepFailed, rec.lastOpts.KeepFailed)
}

// TestDefaultPolicy: defaultPolicy() helper が buildPolicy(nil, 0, nil, nil)
// と同値であることを assert (refactoring 後の equivalence pin)。export/push/
// webhook は MaxAttempts default を当てないので defaultMaxAttempts=0。
func TestDefaultPolicy(t *testing.T) {
	assert.Equal(t, buildPolicy(nil, 0, nil, nil), defaultPolicy())
	// defaultPolicy は MaxAttempts を当てない (= 0)。
	assert.Equal(t, 0, defaultPolicy().MaxAttempts)
}

// TestApplyClientPolicies_DeliverInboxDefaultAttempts: operator が
// `<queue>JobMaxAttempts` を指定しなくても deliver=12 / inbox=8 が
// Policy.MaxAttempts に入り、EnqueueDeliver/EnqueueInbox で WithMaxRetry
// (N-1) が driver opts に伝搬することを pin (#1411)。MaxAttempts default が
// 無いと mkq attempts=0 で落ちた配送先がリトライされない回帰の防止。
func TestApplyClientPolicies_DeliverInboxDefaultAttempts(t *testing.T) {
	rec := &recordingDriverClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	applyClientPolicies(c, &config.Config{})

	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)}))
	// MaxAttempts=12 → WithMaxRetry(11) = 「初回 + 11 retry」。
	assert.True(t, rec.lastOpts.MaxRetrySet, "deliver default attempts must propagate")
	assert.Equal(t, defaultDeliverJobMaxAttempts-1, rec.lastOpts.MaxRetry)

	require.NoError(t, c.EnqueueInbox(context.Background(), queue.InboxPayload{Body: []byte(`{}`)}))
	assert.True(t, rec.lastOpts.MaxRetrySet, "inbox default attempts must propagate")
	assert.Equal(t, defaultInboxJobMaxAttempts-1, rec.lastOpts.MaxRetry)
}

// TestApplyClientPolicies_OperatorAttemptsOptOut: operator が
// deliverJobMaxAttempts=0 を明示すると opt-out (リトライ無し) として尊重され、
// WithMaxRetry が付かない (= mkq attempts=0)。KeepFailed/KeepCompleted と同じ
// 3 状態設計を MaxAttempts でも維持する (#1411)。
func TestApplyClientPolicies_OperatorAttemptsOptOut(t *testing.T) {
	rec := &recordingDriverClient{}
	c := queue.NewClient(&stubDriver{client: rec})
	defer func() { _ = c.Close() }()

	applyClientPolicies(c, &config.Config{DeliverJobMaxAttempts: intp(0)})

	require.NoError(t, c.EnqueueDeliver(queue.DeliverPayload{Inbox: "x", Body: []byte(`{}`)}))
	assert.False(t, rec.lastOpts.MaxRetrySet, "explicit 0 must opt out of retries")
	assert.Equal(t, 0, rec.lastOpts.MaxRetry)
}

// TestBuildPolicy validates the (maxAttempts, defaultMaxAttempts, keepFailed,
// keepCompleted) → Policy transform end-to-end. MaxAttempts は 3 状態:
// nil → defaultMaxAttempts、明示 0 → 0 (operator opt-out)、明示 N → N (#1411)。
// retention は unset → defaults (= 1000 / 30 / 7d / 7d)、明示 0 → 0、明示 N → N。
// Age 値は YAML key を持たないので常に固定 7d (#1184 / #1193)。
func TestBuildPolicy(t *testing.T) {
	defaultAges := struct {
		completed time.Duration
		failed    time.Duration
	}{
		completed: defaultKeepCompletedAge,
		failed:    defaultKeepFailedAge,
	}
	tests := []struct {
		name            string
		maxAttempts     *int
		defaultAttempts int
		keepFailed      *int
		keepCompleted   *int
		want            queue.Policy
	}{
		{
			"all unset, no attempts default",
			nil, 0, nil, nil,
			queue.Policy{
				KeepFailed:       defaultKeepFailed,
				KeepCompleted:    defaultKeepCompleted,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"unset attempts falls back to defaultMaxAttempts",
			nil, 12, nil, nil,
			queue.Policy{
				MaxAttempts:      12,
				KeepFailed:       defaultKeepFailed,
				KeepCompleted:    defaultKeepCompleted,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"explicit attempts overrides defaultMaxAttempts",
			intp(8), 12, nil, nil,
			queue.Policy{
				MaxAttempts:      8,
				KeepFailed:       defaultKeepFailed,
				KeepCompleted:    defaultKeepCompleted,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"explicit attempts 0 opts out even with defaultMaxAttempts",
			intp(0), 12, nil, nil,
			queue.Policy{
				MaxAttempts:      0,
				KeepFailed:       defaultKeepFailed,
				KeepCompleted:    defaultKeepCompleted,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"only keepFailed explicit 500",
			nil, 0, intp(500), nil,
			queue.Policy{
				KeepFailed:       500,
				KeepCompleted:    defaultKeepCompleted,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"only keepFailed explicit 0 (operator opt-out)",
			nil, 0, intp(0), nil,
			queue.Policy{
				KeepFailed:       0,
				KeepCompleted:    defaultKeepCompleted,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"only keepCompleted explicit 100",
			nil, 0, nil, intp(100),
			queue.Policy{
				KeepFailed:       defaultKeepFailed,
				KeepCompleted:    100,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"only keepCompleted explicit 0 (operator opt-out)",
			nil, 0, nil, intp(0),
			queue.Policy{
				KeepFailed:       defaultKeepFailed,
				KeepCompleted:    0,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
		{
			"all explicit",
			intp(4), 12, intp(2000), intp(50),
			queue.Policy{
				MaxAttempts:      4,
				KeepFailed:       2000,
				KeepCompleted:    50,
				KeepCompletedAge: defaultAges.completed,
				KeepFailedAge:    defaultAges.failed,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildPolicy(tt.maxAttempts, tt.defaultAttempts, tt.keepFailed, tt.keepCompleted)
			assert.Equal(t, tt.want, got)
		})
	}
}

// relationship queue は #2403 で追加した。retention policy が付いていないと
// 移行時の一括 follow で completed が積み上がり Redis を圧迫する。
// 4 task type すべてが relationship queue に載ることも同時に pin する
// (routing_test.go と重複するが、こちらは policy 適用後の driver opts を見る)。
func TestApplyClientPolicies_RelationshipHasRetention(t *testing.T) {
	cases := []struct {
		name string
		enq  func(c *queue.Client) error
	}{
		{"follow", func(c *queue.Client) error {
			return c.EnqueueFollow(queue.FollowPayload{FollowerID: "a", FolloweeID: "b"})
		}},
		{"unfollow", func(c *queue.Client) error {
			return c.EnqueueUnfollow(queue.UnfollowPayload{FollowerID: "a", FolloweeID: "b"})
		}},
		{"block", func(c *queue.Client) error {
			return c.EnqueueBlock(queue.BlockPayload{BlockerID: "a", BlockeeID: "b"})
		}},
		{"unblock", func(c *queue.Client) error {
			return c.EnqueueUnblock(queue.UnblockPayload{BlockerID: "a", BlockeeID: "b"})
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recordingDriverClient{}
			c := queue.NewClient(&stubDriver{client: rec})
			defer func() { _ = c.Close() }()

			applyClientPolicies(c, &config.Config{})
			require.NoError(t, tt.enq(c))

			assert.Equal(t, queue.RelationshipQueueName, rec.lastOpts.Queue)
			assert.Equal(t, defaultKeepCompleted, rec.lastOpts.KeepCompleted)
			assert.True(t, rec.lastOpts.KeepCompletedSet, "retention must actually be set")
			assert.Equal(t, defaultKeepFailed, rec.lastOpts.KeepFailed)
			assert.True(t, rec.lastOpts.KeepFailedSet)
		})
	}
}

// autoScaledQueues は個別 knob が無い queue を管理対象にする。relationship も
// 同じ扱いで、relationshipJobConcurrency を明示した場合だけ除外される。#2403。
func TestAutoScaledQueues_Relationship(t *testing.T) {
	managed, skipped := autoScaledQueues(&config.Config{})
	assert.Contains(t, managed, queue.RelationshipQueueName)
	assert.NotContains(t, skipped, queue.RelationshipQueueName)

	managed, skipped = autoScaledQueues(&config.Config{RelationshipJobConcurrency: intp(6)})
	assert.NotContains(t, managed, queue.RelationshipQueueName)
	assert.Contains(t, skipped, queue.RelationshipQueueName)

	// 0 は「未設定」と同じ扱い (他 queue の分岐と揃える)。
	managed, _ = autoScaledQueues(&config.Config{RelationshipJobConcurrency: intp(0)})
	assert.Contains(t, managed, queue.RelationshipQueueName)
}

// stuckWorkerAfter は符号だけが意味を持つ (0 = キューごとの既定、正 = 全キューに
// その値、負 = 無効)。負値を秒数に掛けて -N 秒にすると driver 側の
// 「負なら無効」判定は通るが、値そのものが意味を持つ将来の変更で壊れるので
// -1 に正規化していることを固定する。
func TestStuckWorkerAfter(t *testing.T) {
	for name, tc := range map[string]struct {
		seconds int
		want    time.Duration
	}{
		"未設定はキューごとの既定": {0, 0},
		"正の値は秒に変換":     {900, 15 * time.Minute},
		"負値は -1 に正規化":  {-5, -1},
	} {
		t.Run(name, func(t *testing.T) {
			got := stuckWorkerAfter(&config.Config{QueueStuckWorkerSeconds: tc.seconds})
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestMkqConfig_PassesStuckWorkerAfter gates the config seam for the only
// documented kill switch (`queueStuckWorkerSeconds: -1`).
//
// **配線を落としても他のテストは緑のまま**で、しかも config_dump は閾値を
// 表示し続けるので、止めたつもりが止まっていないことに気付けない (#2657)。
func TestMkqConfig_PassesStuckWorkerAfter(t *testing.T) {
	for name, tc := range map[string]struct {
		seconds int
		want    time.Duration
	}{
		"無効化": {-1, -1},
		"明示値": {900, 15 * time.Minute},
		"未設定": {0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{QueueStuckWorkerSeconds: tc.seconds}
			got := mkqConfig(cfg, 16, nil, nil, nil)
			assert.Equal(t, tc.want, got.StuckWorkerAfter)

			// driver 側の解決規則まで通して、export (既定では追跡しない) が
			// 意図どおりに切り替わることを見る。
			eff := mkqdriver.StuckWorkerThreshold("export", got.StuckWorkerAfter)
			if tc.seconds > 0 {
				assert.Equal(t, tc.want, eff, "explicit value applies to every queue")
			} else {
				assert.Zero(t, eff)
			}
		})
	}
}

// TestMkqConfig_PassesHandlerDeadline gates the config seam for #2658,
// mirroring the stuck-worker knob.
func TestMkqConfig_PassesHandlerDeadline(t *testing.T) {
	for name, tc := range map[string]struct {
		seconds int
		want    time.Duration
	}{
		"無効化": {-1, -1},
		"明示値": {120, 2 * time.Minute},
		"未設定": {0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &config.Config{QueueHandlerDeadlineSeconds: tc.seconds}
			got := mkqConfig(cfg, 16, nil, nil, nil)
			assert.Equal(t, tc.want, got.HandlerDeadline)
		})
	}
}

// プラグイン専用のキューを driver へ渡すこと (#2818)。**worker が見るキューの
// 一覧は構築時に固定される**ので、渡さないとジョブが誰にも処理されない。
func TestMkqConfig_PluginQueues(t *testing.T) {
	cfg := &config.Config{}

	got := mkqConfig(cfg, 16, nil, nil, nil)
	assert.Nil(t, got.QueueNames, "プラグインが無ければ driver の既定に任せる")

	got = mkqConfig(cfg, 16, nil, nil, []string{"plugin:demo"})
	require.NotEmpty(t, got.QueueNames)
	assert.Contains(t, got.QueueNames, "plugin:demo")
	// **本体のキューを落とさないこと。** 上書きすると deliver / inbox が
	// 消えて連合が止まる。
	for _, want := range mkqdriver.QueueNames {
		assert.Containsf(t, got.QueueNames, want, "本体の %q が消えている", want)
	}
}

// recordingQueueClient captures the options of the last Enqueue call.
type recordingQueueClient struct {
	lastOpts driver.EnqueueOptions
}

func (r *recordingQueueClient) Enqueue(_ context.Context, _ string, _ []byte, opts ...driver.EnqueueOption) error {
	r.lastOpts = driver.ApplyEnqueueOptions(opts)
	return nil
}
func (r *recordingQueueClient) Close() error { return nil }

type recordingQueueDriver struct{ client driver.Client }

func (d *recordingQueueDriver) Client() driver.Client        { return d.client }
func (d *recordingQueueDriver) Inspector() driver.Inspector  { return nil }
func (d *recordingQueueDriver) Server() driver.Server        { return nil }
func (d *recordingQueueDriver) Scheduler() driver.Scheduler  { return nil }
func (d *recordingQueueDriver) Close() error                 { return nil }
func (d *recordingQueueDriver) WorkerCount(_ string) int     { return 0 }
func (d *recordingQueueDriver) Resize(_ string, _ int) error { return driver.ErrResizeNotSupported }

// **プラグインのキューにも retention を登録すること。**
//
// 登録が無いと `PolicyFor` が zero Policy を返し、`retentionOptsFromPolicy` の
// `> 0` guard で option が 1 つも出ない = **completed ジョブが Redis に無期限で
// 積まれる**。retention を付けたつもりが本番で no-op だった。
func TestApplyClientPolicies_RegistersPluginQueueRetention(t *testing.T) {
	rec := &recordingQueueClient{}
	c := queue.NewClient(&recordingQueueDriver{client: rec})
	defer func() { _ = c.Close() }()

	applyClientPolicies(c, &config.Config{})

	require.NoError(t, c.EnqueuePlugin(context.Background(), "someplugin", "job", []byte(`{}`)))
	require.True(t, rec.lastOpts.KeepCompletedSet,
		"プラグインのキューに completed の上限が渡っていない")
	require.Positive(t, rec.lastOpts.KeepCompleted)
	require.True(t, rec.lastOpts.KeepFailedSet)
}
