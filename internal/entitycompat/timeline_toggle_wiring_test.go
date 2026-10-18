package entitycompat

import (
	"testing"
)

// #2762 は「`meta.enableFanoutTimelineDbFallback` の列も admin 公開もあるのに、
// 読み取り経路へ配線されていない」という穴だった。配線は
// `coretimeline.WireMetaToggles` に集約してあるが、**router からの呼び出しごと
// 消しても build は通り、テストも全部緑になる**。`internal/server` は CI の
// カバレッジ対象外 (CLAUDE.md Section 4) で、router を組み立てるテストも無い。
//
// そこで同 package の他の gate (limit_specs / secure_endpoints / permissions) と
// 同じく router.go をソースとして読み、呼び出しが残っていることを固定する。
//
// **呼び出しの形まで守る。** 何が配線されるかは `internal/core/timeline` の
// TestWireMetaToggles_Wires* が setter ごとに変異検証付きで固定しているので、
// ここが見るのは「router から正しい引数で呼ばれていること」だけ。
//
// コメント行は数えない。判定は `wiringNodes` の AST 照合なので、`//` でも
// `/* */` でも同じように落ちる (#2856)。**引数まで照合する** ので、
// 初版の前方一致では通り抜けた `WireMetaToggles(hook, nil, nil)` も落ちる。
func TestTimelineTogglesAreWired(t *testing.T) {
	assertWired(t, serverPkg,
		"coretimeline.WireMetaToggles(timelineFanoutHook, timelineService, metaRepo)",
		"FTT のトグル (enableFanoutTimeline / enableFanoutTimelineDbFallback) が "+
			"admin から切り替えても効かなくなる (#2762)")
}
