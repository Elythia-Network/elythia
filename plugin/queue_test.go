package plugin_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/plugin"
)

// **ゼロ値が既定。** 遅延なし・再試行なし・重複排除なしで積む。
func TestEnqueueOptions_ZeroValue(t *testing.T) {
	var o plugin.EnqueueOptions
	assert.Equal(t, time.Duration(0), o.Delay)
	assert.Equal(t, 0, o.MaxAttempts)
	assert.Equal(t, time.Duration(0), o.DedupTTL)
}

func TestEnqueueOptions_Apply(t *testing.T) {
	var o plugin.EnqueueOptions
	for _, fn := range []plugin.EnqueueOption{
		plugin.WithDelay(30 * time.Second),
		plugin.WithMaxAttempts(3),
		plugin.WithDedup(5 * time.Minute),
	} {
		fn(&o)
	}
	assert.Equal(t, 30*time.Second, o.Delay)
	assert.Equal(t, 3, o.MaxAttempts)
	assert.Equal(t, 5*time.Minute, o.DedupTTL)
}

// **後の指定が勝つ。** 呼び出し側が同じオプションを 2 回渡したとき、
// 黙って最初の値が残ると意図と違う設定で積むことになる。
func TestEnqueueOptions_LastWins(t *testing.T) {
	var o plugin.EnqueueOptions
	plugin.WithDelay(time.Second)(&o)
	plugin.WithDelay(time.Minute)(&o)
	assert.Equal(t, time.Minute, o.Delay)
}

// 負値やゼロもそのまま入る。**弾くのはホスト側の仕事**で、ここは値を運ぶだけ
// (0 と 1 はどちらも「再試行しない」として扱われる)。
func TestEnqueueOptions_NonPositive(t *testing.T) {
	var o plugin.EnqueueOptions
	plugin.WithDelay(-time.Second)(&o)
	plugin.WithMaxAttempts(-7)(&o)
	plugin.WithDedup(0)(&o)
	assert.Equal(t, -time.Second, o.Delay)
	assert.Equal(t, -7, o.MaxAttempts)
	assert.Equal(t, time.Duration(0), o.DedupTTL)
}
