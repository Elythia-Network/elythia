package mkqdriver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestQueueNames pins the exact set of queues mkqdriver pre-defines.
//
// この表は「あるべき姿」ではなく現在の実態を固定するもの。queue を増減する
// こと自体は正しい変更なので、期待値を更新して通せばよい。ただしその際に
// **`frontend/packages/misskey-js/src/consts.ts` の `queueTypes` も必ず
// 合わせること。** 管理画面のジョブキュータブは API 応答
// ではなくその定数から生成されるため、ずれると存在しない queue のタブが常時
// ゼロ表示になり、実在する queue が画面から見えなくなる (#2323)。
//
// 同期はコメントでの指示しか無く、実際 #2403 で relationship を足すまで
// 検出手段が無かった。Go 側から frontend のファイルを読むのは CI 構成に
// 依存して脆いので、代わりにここで一覧を固定して更新時に目に入るようにする。
func TestQueueNames(t *testing.T) {
	assert.Equal(t, []string{
		"deliver",
		"inbox",
		"push",
		"export",
		"webhook",
		"maintenance",
		"objectStorage",
		"relationship",
	}, QueueNames)
}

// TestDefaultQueueConcurrency_CoversAllQueues verifies every pre-defined
// queue has a hot-tuned default. 抜けていると unknownQueueConcurrency (2) に
// 落ちるが、これは operator 定義の custom queue 向けの値であって、mk-go が
// 自分で定義した queue が黙って 2 worker で動くのは事故。
func TestDefaultQueueConcurrency_CoversAllQueues(t *testing.T) {
	for _, name := range QueueNames {
		n, ok := defaultQueueConcurrency[name]
		assert.Truef(t, ok, "queue %q has no entry in defaultQueueConcurrency", name)
		assert.Positivef(t, n, "queue %q must have a positive default concurrency", name)
	}
	assert.Len(t, defaultQueueConcurrency, len(QueueNames),
		"defaultQueueConcurrency must not carry entries for queues that no longer exist")
}

// relationship は #2403 で deliver から分離した。upstream の 16 ではなく 4 を
// 既定にしているのは、relationship job が DB bound で db.maxOpenConns (既定 25)
// を HTTP 経路と共有するため。変更する場合は docs/configuration.md と
// docs/divergence/config.md §4-3 の記述も合わせること。
func TestDefaultQueueConcurrency_Relationship(t *testing.T) {
	assert.Equal(t, 4, defaultQueueConcurrency["relationship"])
}
