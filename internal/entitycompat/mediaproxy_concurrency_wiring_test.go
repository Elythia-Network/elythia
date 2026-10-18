package entitycompat

import (
	"testing"
)

// #3032 で足した `mediaProxyConcurrency` は、router が Service へ渡さないと
// **黙って既定値のまま**になる。設定ファイルに書いた運用者からは「効かない」
// としか見えず、build もテストも全部緑で通る。`internal/server` は CI の
// カバレッジ対象外 (CLAUDE.md Section 4) で router を組み立てるテストも無い
// ので、#2762 (`WireMetaToggles`) と同じ形で router.go の呼び出しを固定する。
//
// **枠そのものの挙動はここでは見ない。** 上限を超えないこと・過負荷で shed
// することは `internal/core/mediaproxy` の cpulimit_test.go が変異検証付きで
// 固定している。ここが見るのは「router から正しい引数で呼ばれていること」
// だけ。引数まで照合するので、`SetCPUConcurrency(0)` のように定数へ
// 書き換えて設定を無視する形も落ちる。
func TestMediaProxyConcurrencyIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"proxyService.SetCPUConcurrency(s.config.MediaProxyConcurrency)",
		"mediaProxyConcurrency を設定しても効かず、画像処理の枠が既定のままになる (#3032)")
}
