package entitycompat

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The files whose wiring these gates pin.
const (
	routerGo = "internal/server/router.go"
	serverGo = "internal/server/server.go"
)

// wiringNodes returns normalised source text for every call, struct-literal
// field, and assignment in the given file.
//
// **AST で読む理由。** 初版は行の文字列一致で「`//` で始まる行は数えない」と
// していたが、それだと `/* ... */` で囲んだ配線が生きたものとして通る
// (実測)。パーサはコメントをそもそも構文木に載せないので、囲み方に依存しない。
//
// **呼び出しだけでは足りない。** 守りたい配線には struct literal のフィールド
// (`limiter: newPeerRateLimiter()`) と代入 (`pluginQueues := ...`) があり、
// どちらも CallExpr ではない。
//
// 出力は**常に 1 行**。ソースが複数行に折られていても畳まれるので、テスト側は
// 見たままの 1 行を書けばよい。ただし **KeyValueExpr は末尾カンマを含まない**
// (`limiter: newPeerRateLimiter()`)、**AssignStmt は文全体**を書く。
//
// 正規化は空白の連続を 1 つに畳むだけ。**全部消さない** — interpreted string
// literal の中身まで詰めると `f("a b")` と `f("ab")` が同じものになる
// (raw string literal の改行は畳まれる)。
//
// **守れないもの**: ファイル全体を走るので、未参照の関数の中に同じ配線があっても
// 数える。`if cond { ... }` のような条件付きの配線も素通りする。
// **レシーバを同名でシャドウする形** (`e := e.Group("/files")` の下の `e.Use(...)`)
// も素通りする — AST のテキストが同一になるため。塞ぐには型解決が要る。
func wiringNodes(t *testing.T, rel string) map[string]bool {
	t.Helper()
	// 絶対パスはそのまま使う (テスト用 fixture がリポジトリ外に置かれるため)。
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot(t), rel)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	out := make(map[string]bool)
	render := func(n ast.Node) {
		var buf bytes.Buffer
		// **元の fset を渡さない。** go/printer はソースの改行位置を保存するので、
		// 引数を複数行に折っただけで末尾カンマ入りの別物になる (実測)。無関係な
		// fset を渡すと位置を解決できず、1 行の正規形で出る。
		if err := printer.Fprint(&buf, token.NewFileSet(), n); err != nil {
			// 黙って捨てない。node を落とすと「配線が無い」と報告するので、
			// 原因から遠い偽の赤になる。
			t.Fatalf("print %s: %v", rel, err)
		}
		out[normalizeWiring(buf.String())] = true
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.CallExpr, *ast.KeyValueExpr, *ast.AssignStmt:
			render(n)
		}
		return true
	})
	// **1 つも読めなかったら落とす。** 書式が変わって拾えなくなると、
	// 検査していないのに緑になる。
	if len(out) == 0 {
		t.Fatalf("%s から配線を 1 つも読めなかった", rel)
	}
	return out
}

// normalizeWiring collapses runs of whitespace so gofmt's struct alignment and
// line wrapping do not change what these gates match.
func normalizeWiring(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// 空白を「詰める」のではなく「1 つに畳む」ことを固定する (#2856)。
//
// **全部消すと文字列リテラルの中身まで詰まる。** `f("a b")` と `f("ab")` が
// 同じ配線として扱われ、片方を書けばもう片方が通る。現行の対象に該当する
// ペアは無いので変異検証では差が出ないが、対象が増えたときに黙って壊れる。
//
// **守れるのは interpreted string literal だけ。** raw string literal の改行は
// 畳まれるので、“ f(`a\nb`) “ と “ f(`a b`) “ は同じものになる。配線に
// raw string を書くことは無いので許容している。
func TestNormalizeWiringKeepsStringLiteralSpacing(t *testing.T) {
	if got, other := normalizeWiring(`f("a b")`), normalizeWiring(`f("ab")`); got == other {
		t.Errorf("文字列リテラル内の空白まで詰めている (%q と %q が同じ)", got, other)
	}
	// raw string は畳まれる (既知の限界。変えるなら意図的に変えること)。
	if got, other := normalizeWiring("f(`a\nb`)"), normalizeWiring("f(`a b`)"); got != other {
		t.Errorf("raw string の扱いが変わった (%q と %q)", got, other)
	}
	// gofmt の整列や改行は畳む。
	for _, tt := range []struct{ in, want string }{
		{in: "limiter:   newPeerRateLimiter()", want: "limiter: newPeerRateLimiter()"},
		{in: "f(a,\n\tb)", want: "f(a, b)"},
	} {
		if got := normalizeWiring(tt.in); got != tt.want {
			t.Errorf("normalizeWiring(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

var (
	wiringCacheMu sync.Mutex
	wiringCache   = map[string]map[string]bool{}
)

// wiredContains reports whether the file wires the given node, parsing each
// file once.
//
// **map を返さない。** 共有のキャッシュをそのまま渡すと、呼び出し側が 1 行
// 書き込むだけでプロセス内の全ゲートが汚染される。`-shuffle` が入っているので
// 症状は「別のテストが理由なく緑 / 赤になる」になり、原因から遠い。
func wiredContains(t *testing.T, rel, key string) bool {
	t.Helper()
	wiringCacheMu.Lock()
	defer wiringCacheMu.Unlock()
	nodes, ok := wiringCache[rel]
	if !ok {
		nodes = wiringNodes(t, rel)
		wiringCache[rel] = nodes
	}
	return nodes[key]
}

// assertWired fails when the file does not contain the given wiring verbatim.
//
// **ファイル名から node 集合を引く。** 集合と「どのファイルの話か」を別々に
// 受け取る形だと、router.go を読みながら server.go の配線として緑になる
// 組み合わせが書けてしまう (実測)。
func assertWired(t *testing.T, rel, wiring, symptom string) {
	t.Helper()
	if !wiredContains(t, rel, normalizeWiring(wiring)) {
		t.Errorf("%s に %q の配線が無い (コメントは数えない)。\n%s\n"+
			"**配線を書き換えたなら、この gate の文字列も直すこと** — 引数名を"+
			"変えただけでも落ちる (照合は引数まで含む)。", rel, wiring, symptom)
	}
}

// readAllNotifications の Web Push は router が producer を配線しないと出ない。
//
// **producer だけ無い状態が実際に長く続いていた** (#2831)。`PushReadAllNotifications`
// の実装も `sw_subscription` の列も `/api/sw/*` の受け口も、queue の processor まで
// 揃っているのに**呼ぶ側がどこにも無く**、自分のテストからしか呼ばれていなかった。
// SW 側は upstream と同じく readAllNotifications を受けて表示中の OS 通知を閉じる
// 実装を持っているので、別端末のトーストが「すべて既読」で消えないまま残っていた。
//
// nil は「push しない」なので、**この行を落としても build もテストも通る**。
// 症状は「他の端末の通知が消えない」だけで、エラーもログも出ない。
//
// **引数まで含めて照合する** — 関数名だけ見ると `SetReadAllPusher(nil)` が通り抜けて、
// push が死んだまま緑になる。
func TestReadAllNotificationsPusherIsWired(t *testing.T) {
	assertWired(t, routerGo, "notificationService.SetReadAllPusher(webPushService)",
		"readAllNotifications の Web Push が出なくなり、他端末に表示中の\n"+
			"OS 通知が「すべて既読」でも閉じなくなる。エラーもログも出ない。")
}

// chat の Web Push も router が producer を配線しないと出ない (#2840)。
//
// **新設した TestWebPushProducersAreWired では捕まらない。** あちらは
// `internal/core/chat` の中に `s.chatPusher.PushNewChatMessage(` があるかを
// 見るだけで、その `chatPusher` に実物が入るかは見ていない。router の 1 行を
// 消しても build もテストも gate も全部緑になることを実測した。
//
// **SetUserRepo も要る。** push の body には SW が無条件に読む `fromUser` が
// 必要で、create 経路の row は `FromUser` を Preload しないため repo から
// 引き直す。引けないときは body を壊すより push を落とす実装なので、
// この行が無いとチャットの通知が**全滅する** (`slog.Warn` は出る)。
// `SetAPDelivery` が同じ repo を上書きするので消しても動いてしまい、
// 「実は効いていない配線」に見える状態を避けるためここで固定する。
func TestChatPusherIsWired(t *testing.T) {
	assertWired(t, routerGo, "chatService.SetChatPusher(webPushService)",
		"チャットの Web Push が出なくなる。エラーもログも出ない")
	assertWired(t, routerGo, "chatService.SetUserRepo(userRepo)",
		"push body の fromUser を引けず、チャットの Web Push が全滅する")
}

// chart の save エラーは、logger を**配線時に解決した形**で渡さないと
// ticker goroutine の出力先が実行時の `slog.Default()` になる (#2872)。
//
// `slog.Warn` はパッケージ関数なので、渡すと「ticker が実際に書く時点の
// default」に出る。`Chart` 側は構築時の logger を握るようにしたので、
// ManagementService だけ実行時解決のままだと出力先が食い違う。
//
// **現状の production では差が出ない** — 非テストの `slog.SetDefault` は
// `internal/cli/serve` (serve / dump-routes) と `internal/cli/migrate` だけで、
// 前者は `server.New` より前に走る。差が出る前に #2867 / #2872 と揃えて固定しておくのが目的。
//
// **`internal/server` はカバレッジ 0% 例外で router のテストが薄い。** この 1 行を
// 戻しても build もテストも通ることを実測したので、ここで固定する。
func TestChartManagementLoggerIsResolvedAtWiring(t *testing.T) {
	assertWired(t, routerGo, "chartMgmt.SetLogger(slog.Default().Warn)",
		"起動後に slog.SetDefault を呼ぶコードが入った時点で、chart の save エラーが\n"+
			"ticker の書き込み時点の default に出るようになり、Chart 側 (構築時に固定)\n"+
			"と食い違う。**今は起きない** — 非テストの SetDefault は internal/cli/serve と\n"+
			"internal/cli/migrate だけで、前者は server.New より前。差が出る前に固定しておく")
}

// 複数行に折った配線が 1 行の正規形に畳まれることを固定する (#2856)。
//
// **`printer.Fprint` に元の fset を渡すと畳まれない。** go/printer はソースの
// 改行位置を保存するので、引数を折っただけで末尾カンマ入りの別物になり、
// 配線は生きているのにゲートが赤くなる (実測)。無関係な fset を渡すと位置を
// 解決できず 1 行で出る。
//
// **現行のソースは全部 1 行なので、この選択を戻しても配線ゲートは全部緑のまま
// 通ってしまう。** ここで直接固定しないと守れない。
func TestWiringNodesFoldsMultilineCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	const src = `package p

func f(e E, cfg C) {
	e.Use(middleware.HSTS(
		cfg.URL,
		cfg.DisableHSTS,
	))
}
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	nodes := wiringNodes(t, filepath.Join(dir, "x.go"))
	if !nodes["e.Use(middleware.HSTS(cfg.URL, cfg.DisableHSTS))"] {
		t.Errorf("複数行の呼び出しが 1 行に畳まれていない: %v", keysOf(nodes))
	}
}

// keysOf returns the map's keys for diagnostics.
func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// リモート統計の取得ゲートが **fail-closed な述語**で配線されていること。
//
// **型が同じなので取り違えられる。** `ShouldSkipDelivery` を反転したものを
// 渡しても `SetHostAllowedChecker(func(string) bool)` としては通り、起動時
// 検査も「配線されている」と判定する。しかしあちらは meta が読めないとき
// fail-open (= 配送する) に倒すので、**DB 障害のあいだ defederate した相手へ
// 「誰をいつ見たか」が漏れる**。統計は付加情報なので、取れないより出すほうが
// 悪い。
func TestRemoteStatsGateUsesFailClosedPredicate(t *testing.T) {
	assertWired(t, routerGo,
		"remoteStatsFetcher.SetHostAllowedChecker(instanceService.CanFetchOptionalRemoteData)",
		"リモート統計の取得は fail-closed な述語で配線すること。\n"+
			"`ShouldSkipDelivery` は meta が読めないとき配送する側へ倒れるので、\n"+
			"反転して渡すと DB 障害のあいだ defederate した相手へ出てしまう")
}
