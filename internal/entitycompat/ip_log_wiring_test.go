package entitycompat

import (
	"testing"
)

// IP 履歴の記録 (#3103) は router が配線しないと 1 件も入らない。
//
// **3 本とも、1 本ずつ消せば build もテストも (この gate 以外は) 全部緑になることを
// 実測した。** 3 本同時に消すと `ipLogService` が未使用になって build が落ちるので、
// 気付ける形に見えるが、1 本ずつの変更では何も言わない。
// `internal/server` は CI のカバレッジ対象外 (CLAUDE.md Section 4) で router を
// 組み立てるテストも無く、middleware / iplog / repository のテストはすべて fake 越し
// なので、**3 層を繋いだ経路は一度も実行されない**。#2762 (`WireMetaToggles`) と
// 同じ形で router.go の呼び出しを固定する。
//
// **記録の中身はここでは見ない。** 重複排除・meta の尊重・正規化は
// `internal/core/iplog` が、SQL は `internal/repository` の実 PostgreSQL のテストが
// 変異検証付きで固定している。ここが見るのは「router から正しい引数で呼ばれて
// いること」だけ。引数まで照合するので、`iplog.NewService(userIPRepo, metaRepo, 0)` の
// ように窓を書き換える形も落ちる。
func TestIPLogServiceIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"iplog.NewService(userIPRepo, metaRepo, iplog.DedupeWindow)",
		"IP 履歴が 1 件も記録されなくなり、関連アカウント検索 (#3066) の材料が消える")
}

// 認証済みリクエストごとの記録は middleware の 1 行が担う。
//
// **落としても静かに壊れる。** `enableIpLogging` を有効にしても API 経由の観測が
// 入らなくなり、記録はサインイン時だけ = #3103 以前の密度へ戻る。エラーにも
// 警告にもならない。
func TestClientIPMiddlewareIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"api.Use(middleware.RecordClientIP(ipLogService))",
		"認証済みリクエストの IP が記録されなくなり、サインイン時の観測しか残らない (#3103)")
}

// サインイン経路も同じサービスを通す。**未配線だと再ログイン時の観測が落ちる**
// うえ、`enableIpLogging` を起動時に読む旧配線へ戻す変更を検出できない (#3107)。
func TestSigninIPRecorderIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"signinHandler.SetIPRecorder(ipLogService)",
		"サインイン時の IP が記録されなくなる (#3103 / #3107)")
}
