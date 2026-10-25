package entitycompat

import "testing"

// **配送と受信の queue の processor が、それぞれ自分の設定を読む判定を受け取っていること
// (#3829)。**
//
// 配送停止 (`meta.deliverSuspendedSoftware`、#3558) の判定は
// `corefederation.NewSuspendedChecker`、受信の拒否 (`meta.inboxSuspendedSoftware`、#3362) の
// 判定は `corefederation.NewInboxSuspendedChecker` で作り、どちらも同じ型
// `*corefederation.SuspendedChecker` を返す (読む meta の列は中の関数で分かれる)。
// 構築子の必須の引数にしたので渡し忘れはコンパイルで、nil は
// `TestBuiltHandlersHaveNoNilDependencies` で落ちるが、**取り違えは型でも nil の検査でも
// 捕まらない**。取り違えると、管理画面の配送停止が受信に、受信の拒否が配送に効き、
// どちらもエラーもログも出ない。構築子の呼び出しを引数まで照合する。
func TestQueueProcessorSoftwareCheckersAreWired(t *testing.T) {
	assertWired(t, serverPkg,
		"deliverProcessor := processors.NewDeliverProcessor(apClient, instanceService, "+
			"processors.NewRepoSigningKeySource(keypairRepo, keypairExtraRepo), "+
			"corefederation.NewSuspendedChecker(metaRepo, instanceRepo), "+
			"metadataRefresher, corefollowing.MetaInstanceStatsGate(metaRepo))",
		"配送の processor が配送停止 (deliverSuspendedSoftware) の判定を受け取っていない。\n"+
			"管理画面で配送停止にしたソフトウェアへ配送し続ける (#3558)")
}
