package server

import (
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/plugin"
)

// pluginJobQueueNames returns the queues the worker must consume for the
// installed plugins (#2818).
//
// **ジョブか peer を持つものだけ。** キューを 1 つ足すと worker が 2 本増える
// (mkqdriver の unknownQueueConcurrency) ので、どちらも持たないプラグインの
// ために枠を取らない。無効化したプラグインも対象外 — ハンドラが登録されない
// ので、積んでも誰も処理しない。
//
// peer を含めるのは、送信の再送がこのキューに載るため (#2819)。プラグイン
// 自身が積めるかどうか (pluginQueue.hasJobs) とは別の判定なので、混ぜない。
func pluginJobQueueNames(plugins []plugin.Definition, settings map[string]map[string]any) []string {
	var names []string
	for _, def := range plugins {
		if (def.Jobs == nil && !def.Peered) || !pluginEnabled(settings[def.Name]) {
			continue
		}
		names = append(names, def.Name)
	}
	return queue.PluginQueueNames(names)
}
