package queue_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
)

func TestPluginQueueNaming(t *testing.T) {
	assert.Equal(t, "plugin:demo", queue.PluginQueueName("demo"))
	assert.Equal(t, "plugin:demo:refresh", queue.PluginTaskType("demo", "refresh"))

	// **本体のキューと衝突しないこと。** `:` を含むので、名前の形が違う。
	for _, name := range queue.AllQueueNames() {
		assert.NotEqual(t, name, queue.PluginQueueName("demo"))
	}
}

// キュー名は Redis のキーになるので、形の怪しいものは弾く。
func TestPluginQueueNames_SkipsInvalid(t *testing.T) {
	got := queue.PluginQueueNames([]string{"demo", "", "Bad", "a b", "x/y", "ok-2"})
	assert.Equal(t, []string{"plugin:demo", "plugin:ok-2"}, got)
}

// enqueue は **専用キュー**へ、**再試行なし**で積む。
func TestClient_EnqueuePlugin(t *testing.T) {
	d := newFakeDriver()
	c := queue.NewClient(d)

	require.NoError(t, c.EnqueuePlugin(context.Background(), "demo", "refresh", []byte(`{"a":1}`)))

	require.Len(t, d.client.calls, 1)
	got := d.client.calls[0]
	assert.Equal(t, "plugin:demo:refresh", got.taskType)
	assert.JSONEq(t, `{"a":1}`, string(got.payload))
	o := driver.ApplyEnqueueOptions(got.opts)
	assert.Equal(t, "plugin:demo", o.Queue)
	// **MaxRetrySet を見る。** MaxRetry の 0 はゼロ値なので、オプションを
	// 渡していなくても通ってしまう (未設定の既定が 0 でない driver では、
	// 実際に意味が変わる)。
	assert.True(t, o.MaxRetrySet, "再試行の設定を明示していること")
	assert.Equal(t, 0, o.MaxRetry, "既定は再試行しない")
}

// 呼び出し側のオプションが後ろに来るので、既定を上書きできる。
func TestClient_EnqueuePlugin_OptionsOverrideDefaults(t *testing.T) {
	d := newFakeDriver()
	c := queue.NewClient(d)

	require.NoError(t, c.EnqueuePlugin(context.Background(), "demo", "refresh", nil,
		driver.WithMaxRetry(3), driver.WithProcessIn(90*time.Second)))

	o := driver.ApplyEnqueueOptions(d.client.calls[0].opts)
	assert.Equal(t, "plugin:demo", o.Queue, "キューは既定のまま")
	assert.Equal(t, 3, o.MaxRetry)
	assert.Equal(t, 90*time.Second, o.ProcessIn)
}

func TestClient_EnqueuePlugin_RejectsBadNames(t *testing.T) {
	c := queue.NewClient(newFakeDriver())
	for _, tt := range []struct{ plugin, job string }{
		{"", "refresh"},
		{"Bad Name", "refresh"},
		{"demo", ""},
		// **task type の名前空間を壊す形。** `:` や空白を通すと、別のジョブや
		// 本体の task type と見分けが付かない文字列になる。
		{"demo", "a:b"},
		{"demo", "a b"},
		{"demo", "a\nb"},
		{"demo", "-lead"},
	} {
		assert.Errorf(t, c.EnqueuePlugin(context.Background(), tt.plugin, tt.job, nil),
			"plugin=%q job=%q", tt.plugin, tt.job)
	}
	// 通る形。
	require.NoError(t, c.EnqueuePlugin(context.Background(), "demo", "refresh_v2-1", nil))
}

// fakeDriver は enqueue の引数だけを見るための最小実装。
//
// **実 Redis を使わない。** ここで確かめたいのは「どのキューへ、どの task type
// で、どのオプションで積むか」だけなので、driver を丸ごと立てる必要が無い。
type fakeDriver struct {
	driver.Driver
	client *fakeClient
}

func newFakeDriver() *fakeDriver { return &fakeDriver{client: &fakeClient{}} }

func (d *fakeDriver) Client() driver.Client       { return d.client }
func (d *fakeDriver) Inspector() driver.Inspector { return nil }

type enqueueCall struct {
	taskType string
	payload  []byte
	opts     []driver.EnqueueOption
}

type fakeClient struct{ calls []enqueueCall }

func (c *fakeClient) Enqueue(_ context.Context, taskType string, payload []byte, opts ...driver.EnqueueOption) error {
	c.calls = append(c.calls, enqueueCall{taskType: taskType, payload: payload, opts: opts})
	return nil
}

func (c *fakeClient) Close() error { return nil }

// peer の送信は **プラグイン専用のキュー**へ積む (#2819)。予約名なので、
// プラグイン自身のジョブと衝突しない。
func TestClient_EnqueuePluginPeer(t *testing.T) {
	d := newFakeDriver()
	c := queue.NewClient(d)

	require.NoError(t, c.EnqueuePluginPeer(context.Background(), "demo", []byte(`{"host":"x"}`),
		driver.WithMaxRetry(3)))

	require.Len(t, d.client.calls, 1)
	got := d.client.calls[0]
	assert.Equal(t, "plugin:demo:_peer", got.taskType)
	o := driver.ApplyEnqueueOptions(got.opts)
	assert.Equal(t, "plugin:demo", o.Queue)
	assert.Equal(t, 3, o.MaxRetry)

	assert.Error(t, c.EnqueuePluginPeer(context.Background(), "Bad Name", nil))
}

// **プラグインは `_peer` を名乗れない。** 名乗れると本体の送信ジョブと
// 区別が付かなくなる。
func TestPluginJobName_CannotClaimPeerSlot(t *testing.T) {
	c := queue.NewClient(newFakeDriver())
	assert.Error(t, c.EnqueuePlugin(context.Background(), "demo", queue.PluginPeerJobName, nil))
	assert.Equal(t, queue.PluginTaskType("demo", queue.PluginPeerJobName), queue.PluginPeerTaskType("demo"))
}
