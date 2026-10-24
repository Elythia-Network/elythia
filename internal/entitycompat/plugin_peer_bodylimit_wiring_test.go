package entitycompat

import (
	"testing"
)

// peer の受け口の本文上限は、newServer が BodyLimitByPath へ表を渡さないと効かない。
//
// **第 2 引数を nil にしても build もテストも通る。** middleware 側のテストは表を
// 自分で組み立てて渡すので、呼び出し側を見ていない。落とすと全プラグインの受け口が
// `/api` の 1 MiB に戻り、**宣言した上限が黙って無意味になる** — 署名を検証する前に
// 読める本文が 16 倍になるので、気付ける症状は出ない。
//
// 判定は `wiringNodes` の AST 照合 (`assertWired` と同じ形)。**コメント行は
// 数えない** — `//` でも `/* */` でも落ちる (#2856)。
//
// **引数まで含めて照合する。** 関数名だけを見ると `BodyLimitByPath(x, nil)` が
// 通り抜けて、上限が死んだまま緑になる。
//
// **`e.Use(` まで含める。** 内側の呼び出しだけを見ると
// `e.Group("/api").Use(...)` へ移す退行が素通りする (実測)。body に作用する
// middleware は **global かつ `auth.Authenticate` より前**でないと、auth が
// token 抽出で body を io.ReadAll するので bypass される (#1958 / #2075)。
// **順序までは守れない** — `e.Use` の並びは見ていないので、auth より後ろへ
// 動かす退行は検出できない。
func TestPluginPeerBodyLimitIsWired(t *testing.T) {
	assertWired(t, serverGo,
		"e.Use(middleware.BodyLimitByPath(cfg.MaxFileSize, peerBodyLimitsByPath(plugins, cfg.Plugins)))",
		"プラグインごとの peer 本文上限が /api の 1 MiB に戻る")
}

// peer のレート制限は deps に limiter が入っていないと丸ごと効かない。
//
// **nil は「全部通す」。** テストが組み立てる deps は limiter を持たないので
// nil 許容にしてあり、落としても build もテストも通る。落ちるのは
// 「認証を通った相手 (と署名を持たない相手) が受け口を無制限に叩ける」状態で、
// 症状が出るのは攻撃を受けたときだけ。
// **これは呼び出しではなく struct literal のフィールド。** `wiringNodes` は
// KeyValueExpr も集めるので同じ形で書ける。空白は正規化されるので、gofmt の
// 整列が変わっても落ちない (実際に変わって落ちたことがある)。
func TestPluginPeerRateLimiterIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"limiter: newPeerRateLimiter()",
		"pluginPeerDeps の limiter が nil になり、peer の受け口が無制限になる")
}

// catchall は名前付き関数を router から参照していないと効かない。
//
// **無名関数に戻しても build もテストも通る。** `apiCatchall` は package-level
// なので使われなくなっても go vet は咎めず、直接呼ぶテストは緑のまま。落ちるのは
// 「受け口の無いプラグインへの POST が 200 + {} に戻る」ところで、症状は
// 相手側にしか出ない (#2822)。
func TestAPICatchallIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		`api.Any("/*", apiCatchall)`,
		"peer の受け口が無いプラグインへの POST が 200 + {} に戻る")
}

// プラグイン専用キューの名前は driver へ渡さないと worker が見ない (#2818)。
//
// **第 3 引数を nil にしても build もテストも通る。** mkqConfig を直接叩く
// テストはあるが、newServer がその結果を渡すことは誰も見ていない。落とすと
// mkq が Define しないので、**enqueue が `unknown queue` で全部落ちる** —
// 機能が丸ごと死ぬのに緑になる。
// **前半は代入なので呼び出しでは拾えない。** `wiringNodes` は AssignStmt も
// 集めるので、名前を含めて固定できる。
func TestPluginJobQueuesAreWired(t *testing.T) {
	const symptom = "プラグインのキューが driver に登録されず、enqueue が unknown queue で落ちる"
	assertWired(t, serverGo, "pluginQueues := pluginJobQueueNames(plugins, cfg.Plugins)", symptom)
	assertWired(t, serverGo, "buildQueueDriver(context.Background(), cfg, pluginQueues)", symptom)
}

// peer の送信は queue client を配線しないと積めない (#2819)。
//
// **落としても build もテストも通る。** pluginPeer は enqueuer が nil のとき
// warn を出して捨てるので、落ちるのは「送信が全部消える」ところ。相手側にしか
// 症状が出ないうえ、こちらのログを見ないと気付けない。
func TestPluginPeerEnqueuerIsWired(t *testing.T) {
	assertWired(t, serverPkg,
		"enqueuer: s.queueClient",
		"pluginPeerDeps の enqueuer が nil になり、peer の送信が全部捨てられる")
}
