package entitycompat

import (
	"testing"
)

// URL プレビューと media proxy は、自分の constructor で Transport を作った後に
// router から共有の Transport へ差し替える (#3591)。この行を落としても build も
// テストも通り、その経路だけが別の接続のプールに戻る (同じ相手への接続を
// 他の経路と使い回さなくなる) だけで、エラーもログも出ない。`internal/server`
// は router を組み立てるテストが無いので、#2762 と同じ形で呼び出しを固定する。
//
// 差し替えた後の振る舞い (タイムアウトとリダイレクトの扱いを保つこと) は
// `internal/core/urlpreview` / `internal/core/mediaproxy` の transport_test.go が
// 見ている。ここが見るのは router から共有の Transport を渡していることだけ。
func TestOutboundTransportIsShared(t *testing.T) {
	assertWired(t, serverPkg,
		"urlPreviewFetcher.SetTransport(s.outboundTransport())",
		"URL プレビューが共有の Transport を使わず、同じ相手への接続を他の経路と使い回さなくなる (#3591)")
	assertWired(t, serverPkg,
		"proxyService.SetTransport(s.outboundTransport())",
		"media proxy が共有の Transport を使わず、同じ相手への接続を他の経路と使い回さなくなる (#3591)")
}
