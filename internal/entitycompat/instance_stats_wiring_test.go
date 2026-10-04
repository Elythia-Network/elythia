package entitycompat

import "testing"

// meta.enableStatsForFederatedInstances は router が 2 箇所へ渡さないと効かない
// (#3330)。
//
// following service の gate も chart hook のフラグも、**未配線なら本家の既定値
// (true) として動く**ので、行ごと消しても build もテストも通る。症状は
// 「admin で切っても instance の followersCount / followingCount と instance chart の
// following / followers が増減し続ける」だけで、エラーもログも出ない。
// `internal/server` は CI のカバレッジ対象外 (CLAUDE.md Section 4) なので、
// TestTimelineTogglesAreWired と同じく router.go を AST で照合して固定する。
//
// **引数まで照合する** — `SetInstanceStatsGate(nil)` や、chart 側へ別のフラグを
// 代入する取り違えも落ちる。gate そのものの挙動は internal/core/following の
// TestMetaInstanceStatsGate と TestFollow_InstanceStatsGateOff_LeavesCounters、
// chart 側は charthook の TestHooks_OnFollow_InstanceChartNeedsStatsFlag が見る。
func TestInstanceStatsGateIsWired(t *testing.T) {
	assertWired(t, routerGo,
		"followingService.SetInstanceStatsGate(corefollowing.MetaInstanceStatsGate(metaRepo))",
		"enableStatsForFederatedInstances を切っても、フォロー / 解除で instance の\n"+
			"followersCount / followingCount が増減し続ける (#3330)")
	assertWired(t, routerGo,
		"chartHooks.StatsForFederatedInst = m.EnableStatsForFederatedInstances",
		"enableStatsForFederatedInstances を切っても、instance chart の\n"+
			"following / followers が更新され続ける (#3330)")
}
