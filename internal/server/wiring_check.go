package server

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// errWiringCheckSkipped is what constructionError reports when setupRoutes
// never ran the check.
//
// **検査そのものを消す経路を塞ぐ。** setupRoutes 側の代入行を消すだけで
// 検査全体が止まり、しかも verifyCriticalWiring も predicate も未使用の
// まま**コンパイルが通り単体テストも緑**になる (Go は未使用の
// package-level 関数を咎めない)。これは本 PR が塞ぎに来た「1 つ消しても
// 気付けない」がそのまま 1 段上に再現した状態だった (#2682 review M-1)。
//
// **別途「未実行」を代入して回る方式は採らない。** その代入行自体が
// 1 行消せる対象になるだけで、実際に消してもテストは緑のままだった
// (#2682 review L-A)。Server.wiringChecked のゼロ値を「未実行」にして、
// 消すべき行を無くしてある。
var errWiringCheckSkipped = errors.New("critical wiring check did not run: setupRoutes must assign wiringChecked/wiringErr")

// criticalWiringCount is the number of entries setupRoutes is expected to
// register. Guards against an entry being dropped from the table.
const criticalWiringCount = 89

// criticalWiring names one dependency whose absence degrades behaviour
// silently instead of failing visibly.
type criticalWiring struct {
	// name identifies the dependency in the startup error.
	//
	// **grep できる識別子ではなく、運用者向けの安定ラベル。** 多くは
	// "<package>.<field>" だが、消費側が別パッケージにあるもの
	// (inbox.signatureVerifier / inbox.replayGuard は processors) や、
	// 単一の field ではなく複数の配線が揃って初めて成立するもの
	// (signup.ticketConsumption = db + ticketRepo) がある
	// (#2682 review L-C)。実体は各 Has… 述語の GoDoc を見ること。
	name string
	// wired is the consumer's own report of whether it received the
	// dependency.
	wired bool
	// effect describes what happens when it is missing, so the operator can
	// judge severity without reading the source.
	effect string
}

// verifyCriticalWiring reports the dependencies that setupRoutes is expected to
// wire but did not.
//
// **ここに載せるのは「未配線でも動いてしまう」ものだけ。** 未配線で panic する
// 依存や、404 / 空リストを返して利用者から見て明らかに壊れるものは対象外で、
// 気付けないのは fallback が**より緩い**方向に働くものに限られる (棚卸しは #2674)。
//
// 消費側に fallback を残したままにしているのは、テストが未配線の依存で
// 大量に組み立てているため (notes は 341 本中 327 本が未配線経路に依存する)。
// fallback を消す代わりに、**本番で発火しうる状態を起動時に落とす**。
//
// 載せているのは認証・一回性の保証に関わるもの (#2682) と、可視性・権限・上限に
// 関わるもの (#2683)。後者は前者ほど鋭くないが、いずれも**利用者から見えない形で
// 制限が外れる**。
func verifyCriticalWiring(deps []criticalWiring) error {
	// **件数の下限を持つ。** entry を 1 つ消しても他のテストは緑のままなので、
	// 数そのものを縛らないと「表から抜く」形の劣化を検出できない。
	// entry を意図的に増減したらここも更新すること。
	//
	// **止められるのは「1 行消す」だけ。** 行を消して下限も一緒に下げれば
	// 通るし、predicate 呼び出しを `true` リテラルに書き換えれば件数は
	// 変わらない。テストは全て criticalWiringCount から導出されるので、
	// 数そのものを独立に固定してもいない。entitycompat の golden gate は
	// 実数よりずっと低い下限を test 側に置く *slack* な gate だが、こちらは
	// 実数に張り付いた *tight* な gate で、防げるのは事故だけ
	// (#2682 review L-1)。
	if n := len(deps); n < criticalWiringCount {
		return fmt.Errorf("critical wiring table shrank: got %d entries, want >= %d", n, criticalWiringCount)
	}
	var missing []string
	for _, d := range deps {
		if !d.wired {
			missing = append(missing, fmt.Sprintf("%s (%s)", d.name, d.effect))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("critical wiring missing: %s", strings.Join(missing, "; "))
}

// recordCriticalWiring runs the check and marks it as having run.
//
// **2 つの field を別々に代入させない。** 分けて書くと、結果の代入だけを
// 消して「検査済み・問題なし」の状態を作れてしまう (#2682 review L-A)。
// 呼び出し側からは 1 つの操作にしか見えないようにする。
func (s *Server) recordCriticalWiring(deps []criticalWiring) {
	s.wiringChecked = true
	s.wiringErr = verifyCriticalWiring(deps)
}

// constructionError reports the first fatal problem recorded during
// setupRoutes, or nil.
//
// setupRoutes は戻り値を持てないので field へ記録し、ここでまとめて拾う。
// **New から直接 if を並べない。** 並べると 1 つ消しても他のテストが緑のままで、
// 「配線検査そのものを黙って外す」ことができてしまう (#2682 review)。
func (s *Server) constructionError() error {
	if s.pluginSetupErr != nil {
		return s.pluginSetupErr
	}
	if !s.wiringChecked {
		return errWiringCheckSkipped
	}
	return s.wiringErr
}
