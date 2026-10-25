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
// chart 側は charthook の TestHooks_OnFollow_InstanceChartNeedsStatsFlag と
// TestHooks_SetMetaSource、移行は internal/core/move の
// TestPostMoveProcess_AdjustsInstanceFollowers が見る。
//
// instance の notesCount / usersCount (InstanceCounter) と移行の followersCount
// (SetInstanceStats) も、未配線なら黙って数えないだけなので同じく照合する。
func TestInstanceStatsGateIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"followingService.SetInstanceStatsGate(corefollowing.MetaInstanceStatsGate(metaRepo))",
		"enableStatsForFederatedInstances を切っても、フォロー / 解除で instance の\n"+
			"followersCount / followingCount が増減し続ける (#3330)")
	assertWired(t, serverPkg,
		"chartHooks.StatsForFederatedInst = m.EnableStatsForFederatedInstances",
		"enableStatsForFederatedInstances を切っても、instance chart の\n"+
			"following / followers が更新され続ける (#3330)")
	assertWired(t, serverPkg,
		"chartHooks.SetMetaSource(metaRepo.Fetch)",
		"admin で enableStatsForFederatedInstances などを切り替えても、chart と\n"+
			"instance の集計列が再起動まで前の設定のまま動く (#3330)")
	assertWired(t, serverPkg,
		"federationResolver.SetNoteChartHook(chartHooks)",
		"inbound Create や解決のついでに取り込んだリモートの投稿が、notes chart にも\n"+
			"instance の notesCount にも数えられない (削除だけが引く) (#3330)")
	assertWired(t, serverPkg,
		"chartHooks.InstanceCounter = instanceCounterBuffer",
		"instance の notesCount / usersCount が一切増減しない (#3330)")
	assertWired(t, serverPkg,
		"accountMover.SetInstanceStats(instanceRepo, corefollowing.MetaInstanceStatsGate(metaRepo), chartHooks)",
		"リモートアカウントが移行しても、その instance の followersCount と\n"+
			"chart が減らない (#3330)")
	// 配送の nodeinfo の取り直しの判定は、#3829 から deliver の processor の構築子の
	// 最後の引数。判定を反転させた関数も同じ型で渡せるので、型でも nil の検査でも
	// 捕まらない。構築子の呼び出しを引数まで照合する。
	assertWired(t, serverPkg,
		"deliverProcessor := processors.NewDeliverProcessor(apClient, instanceService, processors.NewRepoSigningKeySource(keypairRepo, keypairExtraRepo), corefederation.NewSuspendedChecker(metaRepo, instanceRepo), metadataRefresher, corefollowing.MetaInstanceStatsGate(metaRepo))",
		"enableStatsForFederatedInstances を切っても、配送のたびに相手の nodeinfo を\n"+
			"取り直し続ける (本家は切ると取りに行かない) (#3564)")
}
