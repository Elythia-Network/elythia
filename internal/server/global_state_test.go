package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/api/meself"
	apimeta "github.com/shiroha-a/mk/internal/api/meta"
	"github.com/shiroha-a/mk/internal/api/notehide"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	coretwofactor "github.com/shiroha-a/mk/internal/core/twofactor"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/frontendutil"
	"github.com/shiroha-a/mk/internal/misc/password"
)

// restoreProcessGlobals undoes the process-wide state that `New` /
// `newServer` install, so test order does not change results.
//
// **`internal/server` はプロセス共有の状態を張り替えるテストが多い。**
// `newServer` はサーバーを 1 度だけ起動する前提で書かれているのでグローバルを
// 無条件に差し替えるが、テストは同じプロセスで何度も呼ぶ。`go test -shuffle` を
// 有効にすると、その後ろに並んだ `avatar` / `emoji_redirect` が**署名付き
// プロキシ URL を受け取って落ちる** (#2795 で 5 seed すべて失敗した)。
//
// **保存して戻すのではなく零値に戻す。** どの setter も「まだ配線されていない」
// 状態を零値で表し、その場合の fallback が定義されている
// (`entity/mediaurl.go` の `currentMediaURLContext` など)。保存する形だと、
// 先に漏れた値を掴んでいたときにそれを持ち回してしまう。
//
// **同ファイルの `TestProcessGlobalsAreRestored` が、ここの一覧と
// `router.go` / `server.go` の実際の呼び出しが一致していることを見る。**
// 手で数えると漏れる — 実際、初版は `entity` の 7 つだけで、残り 5 つ
// (`notehide.SetFollowingRepo` / `coretwofactor.SetTestMode` /
// `meself.SetEnricher` / `password.SetCost` / `corenote.SetHookConcurrency`) を
// 落としていた。
func restoreProcessGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		entity.SetMediaURLContext(nil)
		entity.SetAvatarDecorationLookup(nil)
		entity.SetEmojiDecorationLookup(nil)
		entity.SetCanChatLookup(nil)
		entity.SetUserRolesLookup(nil)
		entity.SetShowRemoteBadgesLookup(nil)
		entity.SetInstanceIconURLLookup(nil)
		entity.SetSilencedLookup(nil)
		notehide.SetFollowingRepo(nil)
		coretwofactor.SetTestMode(false)
		meself.SetEnricher(nil)
		// **latch も戻す。** `RequireSetup` は「利用者を観測した」を覚えるので、
		// counter だけ外しても前のテストの観測が残る。
		apimeta.SetLocalUserCounter(nil)
		apimeta.ResetSetupLatchForTest()
		// この 2 つは cfg 由来。0 / 既定を渡すと各パッケージの既定に戻る。
		_ = password.SetCost(password.DefaultCost)
		corenote.SetHookConcurrency(0)
		frontendutil.ResetLoaderCacheForTest()
	})
}

// **loader fixture がテストをまたいで居座らないこと** (#2795)。
//
// `frontendutil` のキャッシュはプロセスに 1 つで、fixture は `t.TempDir()` に
// 置く。cleanup を登録しないと、**ディレクトリが消えた後もその内容がキャッシュに
// 残る**。他のテストがそれを掴むかどうかは実行順まかせなので、`-shuffle` の
// seed によって落ちたり落ちなかったりする。
//
// このテストは「fixture を使うテストの形」をその場で作って、`t.Cleanup` が
// 実際にキャッシュを捨てることを直接見る。**cleanup を外すと落ちる**ので、
// 6 箇所の登録が無検証にならない。
func TestLoaderCacheDoesNotOutliveFixture(t *testing.T) {
	const marker = "#splash{--marker:leak-probe}"

	// **このテスト自身も漏らさない。** 下で親レベルの `BootLoaderAssets` を
	// 呼ぶと `loaderOnce` が発火し、その時点の実ディレクトリの内容が
	// バイナリ終端まで固定される。CI では `built` が無いので空だが、frontend を
	// ビルド済みの手元では実 CSS が居座る。
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	// fixture を使うテストと同じ形。サブテストが終われば cleanup が走る。
	t.Run("fixture", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "style.css"),
			[]byte(marker), 0o644))
		t.Setenv("MISSKEY_FRONTEND_DIR", dir)
		frontendutil.ResetLoaderCacheForTest()
		t.Cleanup(frontendutil.ResetLoaderCacheForTest)

		require.Equal(t, marker, frontendutil.BootLoaderAssets().CSS,
			"fixture が読めていない (前提が崩れている)")
	})

	assert.NotEqual(t, marker, frontendutil.BootLoaderAssets().CSS,
		"fixture の loader CSS がキャッシュに居座っている (#2795)")
}

// **`restoreProcessGlobals` の一覧が `router.go` / `server.go` の実際の呼び出しと
// 一致していること** (#2795)。
//
// 手で数えると漏れる。初版は `entity` の 7 つだけを戻しており、
// `notehide.SetFollowingRepo` / `coretwofactor.SetTestMode` /
// `meself.SetEnricher` の 3 つを落としていた。落としたものが**今は読まれて
// いない**ので、テストは緑のまま通っていた — つまり #2795 以前の状態に戻る道が
// 開いたままだった。
//
// 判定はソースの文字列一致。**コメント行は数えない** (コメントアウトして残すのは
// 消すのと同じ)。同 package の `wiring_check` が生ソースを読む形なのに合わせて
// ある。
func TestProcessGlobalsAreRestored(t *testing.T) {
	// **ソース側を AST で数え、表と突き合わせる。** 表 → ソースの片方向だけだと、
	// 「新しい setter を足したが表にも restore にも書かなかった」が素通りする —
	// **漏らす人は表にも書き忘れる**ので、表だけを正にしても意味がない。
	found := processGlobalSetters(t)

	want := map[string]string{
		"entity.SetAvatarDecorationLookup": "router.go",
		"entity.SetEmojiDecorationLookup":  "router.go",
		"entity.SetCanChatLookup":          "router.go",
		"entity.SetUserRolesLookup":        "router.go",
		"entity.SetShowRemoteBadgesLookup": "router.go",
		"entity.SetInstanceIconURLLookup":  "router.go",
		"entity.SetMediaURLContext":        "router.go",
		"entity.SetSilencedLookup":         "router.go",
		"notehide.SetFollowingRepo":        "router.go",
		"coretwofactor.SetTestMode":        "router.go",
		"meself.SetEnricher":               "router.go",
		"meta.SetLocalUserCounter":         "router.go",
		"password.SetCost":                 "server.go",
		"corenote.SetHookConcurrency":      "server.go",
	}

	assert.Equal(t, want, found,
		"起動時に張り替えるグローバルの一覧が変わった。足したなら "+
			"restoreProcessGlobals にも足すこと (#2795)")

	// restore が実際に呼んでいるか。表と一致していても、restore が呼ばなければ
	// 意味がない。
	restore := restoreFuncBody(t)
	for call := range want {
		// **`(` まで含めて照合する。** 名前だけだと `_ = meself.SetEnricher` の
		// ような「参照はするが呼ばない」形を通してしまう (実際に通した)。
		name := call[strings.LastIndex(call, ".")+1:] + "("
		assert.Contains(t, restore, name,
			"restoreProcessGlobals が %s を戻していない (#2795)", call)
	}
}

// processGlobalSetters returns every `pkg.SetXxx(...)` call that `router.go`
// and `server.go` make on an imported package, keyed by `pkg.SetXxx`.
//
// **メソッド呼び出し (`s.foo.SetBar(...)`) は除く。** それはインスタンスの
// 設定であってプロセス共有の状態ではない。パッケージ名は import で解決する。
func processGlobalSetters(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, file := range []string{"router.go", "server.go"} {
		src := readNonCommentSource(t, file)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, src, 0)
		require.NoError(t, err)

		pkgs := map[string]bool{}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			name := path[strings.LastIndex(path, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			pkgs[name] = true
		}

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "Set") {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || !pkgs[id.Name] {
				return true
			}
			out[id.Name+"."+sel.Sel.Name] = file
			return true
		})
	}
	return out
}

// restoreFuncBody returns the body of restoreProcessGlobals, comments removed.
func restoreFuncBody(t *testing.T) string {
	t.Helper()
	src := readNonCommentSource(t, "global_state_test.go")
	start := strings.Index(src, "func restoreProcessGlobals(")
	require.GreaterOrEqual(t, start, 0, "restoreProcessGlobals が見つからない")
	end := strings.Index(src[start:], "\n}\n")
	require.GreaterOrEqual(t, end, 0, "restoreProcessGlobals の終端が見つからない")
	return src[start : start+end]
}

// readNonCommentSource returns the file's source with comment-only lines
// removed, so commenting a call out counts as removing it.
func readNonCommentSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	require.NoError(t, err)
	// **block comment も落とす。** `/* entity.SetSilencedLookup(...) */` は
	// 行コメント判定だけだと生き残り、「コメントアウトして残すのは消すのと
	// 同じ」が成立しなくなる。
	src := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(b), "")
	var kept []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// **loader fixture を置くテストは必ずキャッシュを捨てること** (#2795)。
//
// `frontendutil` のキャッシュはプロセスに 1 つで、fixture は `t.TempDir()` に
// 置く。`t.Cleanup` を登録しないと**ディレクトリが消えた後も内容がキャッシュに
// 残る**。どのテストがそれを掴むかは実行順まかせなので、`-shuffle` の seed に
// よって落ちたり落ちなかったりする。
//
// **登録そのものを gate で強制する。** 個々の登録は変異検証が効かない —
// `TestFrontendHTML_SplashColor` の `<style>` 抽出を splash 名指しに直した時点で、
// **登録を全部 (このコミットが足した 7 件 + 元からあった 2 件) 外しても 40 seed で
// 落ちなくなった** (= 無検証のコードになった)。
// 「今たまたま誰も踏んでいない」ことと「安全」は別なので、形で縛る。
func TestLoaderFixtureTestsResetCache(t *testing.T) {
	fset := token.NewFileSet()
	// **`parser.ParseDir` は使わない** (Go 1.25 で非推奨)。非推奨の理由は「build tag を
	// 見ないので package とファイルの対応が不正確」だが、ここは**このディレクトリの
	// `_test.go` を全部見たい**ので、その不正確さがむしろ要件に合う。
	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	// **slice で持つ。** `os.ReadDir` はファイル名順にソート済みで返すので、
	// map に入れ直すと理由なく報告順が非決定的になる (docs/gates.md の sqlbind-check
	// entry を参照)。旧 `parser.ParseDir` は map を返していた。
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		require.NoErrorf(t, perr, "%s を parse できない", name)
		files = append(files, f)
	}

	// env 名を定数に切り出す形も追えるように、まず const の中身を集める。
	consts := map[string]string{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			for i, name := range vs.Names {
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
						consts[name.Name] = lit.Value
					}
				}
			}
			return true
		})
	}

	checked := 0
	for _, f := range files {
		path := fset.Position(f.Pos()).Filename
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// **`t.Setenv` を呼んだのと同じスコープに登録されているかを
			// 見る。** サブテストに逃がすと、そこを抜けた時点で reset が
			// 走った後に親が再び読み込んでしまう。`t` は shadow されても
			// 名前が同じなので、受け手の名前一致では防げない (実測で
			// 素通りした)。
			for _, scope := range loaderFixtureScopes(fn.Body, consts) {
				checked++
				assert.True(t, registersLoaderReset(scope),
					"%s:%s は loader ディレクトリを差し替えるのに "+
						"同じスコープで t.Cleanup(frontendutil.ResetLoaderCacheForTest) を "+
						"登録していない (#2795)",
					filepath.Base(path), fn.Name.Name)
			}
		}
	}
	// **検出 0 件は PASS と区別が付かない。** 走査が壊れたら落とす。実測 10 に
	// 対して下限を近く取る — 8 だと 2 件静かに落ちても通る。
	require.GreaterOrEqual(t, checked, 10,
		"loader fixture を置くテストを %d 個しか見つけられなかった。走査が壊れている", checked)
}

// loaderFixtureScopes returns every scope (function body or function literal
// body) that calls t.Setenv for a frontend loader directory.
//
// **スコープ単位で返す。** サブテストは別スコープなので、そこに `Cleanup` を
// 置いても親の `Setenv` は守られない。
func loaderFixtureScopes(body *ast.BlockStmt, constLiterals map[string]string) []*ast.BlockStmt {
	var out []*ast.BlockStmt
	var visit func(b *ast.BlockStmt)
	visit = func(b *ast.BlockStmt) {
		if setsFrontendDirInScope(b, constLiterals) {
			out = append(out, b)
		}
		for _, lit := range nestedFuncLits(b) {
			visit(lit.Body)
		}
	}
	visit(body)
	return out
}

// nestedFuncLits returns the function literals directly inside b (not the ones
// nested deeper inside another literal).
func nestedFuncLits(b *ast.BlockStmt) []*ast.FuncLit {
	var out []*ast.FuncLit
	walk := func(n ast.Node) {
		ast.Inspect(n, func(x ast.Node) bool {
			if x == n {
				return true
			}
			if lit, ok := x.(*ast.FuncLit); ok {
				out = append(out, lit)
				return false
			}
			return true
		})
	}
	walk(b)
	return out
}

// scopeInspect walks b without descending into nested function literals.
func scopeInspect(b *ast.BlockStmt, fn func(ast.Node) bool) {
	ast.Inspect(b, func(n ast.Node) bool {
		if n == ast.Node(b) {
			return true
		}
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		return fn(n)
	})
}

// setsFrontendDirInScope reports whether b itself (not a nested literal) calls
// t.Setenv for a loader directory.
func setsFrontendDirInScope(b *ast.BlockStmt, constLiterals map[string]string) bool {
	found := false
	scopeInspect(b, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Setenv" {
			return true
		}
		// **リテラルに限定しない。** `const frontendDirEnv = "..."` に切り出す
		// ごく普通の整理で走査から外れてしまう (実測で 2 件静かに落ちた)。
		ast.Inspect(call.Args[0], func(x ast.Node) bool {
			val := ""
			switch v := x.(type) {
			case *ast.BasicLit:
				val = v.Value
			case *ast.Ident:
				// 定数名は env 名を含まないので、名前ではなく**定義を引く**。
				val = constLiterals[v.Name]
			}
			if strings.Contains(val, "MISSKEY_FRONTEND_DIR") ||
				strings.Contains(val, "MISSKEY_FRONTEND_EMBED_DIR") {
				found = true
			}
			return !found
		})
		return !found
	})
	return found
}

// registersLoaderReset reports whether the block registers the cache reset as
// a cleanup. **`ResetLoaderCacheForTest()` を素で呼ぶだけでは足りない** — それは
// 「入るとき」の掃除で、居座りを防ぐのは「出るとき」の登録のほう。
func registersLoaderReset(b *ast.BlockStmt) bool {
	found := false
	scopeInspect(b, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Cleanup" {
			return true
		}
		// `t.Cleanup(frontendutil.ResetLoaderCacheForTest)` と
		// `t.Cleanup(func(){ ... ResetLoaderCacheForTest() ... })` の両方。
		ast.Inspect(call.Args[0], func(x ast.Node) bool {
			if id, ok := x.(*ast.SelectorExpr); ok &&
				id.Sel.Name == "ResetLoaderCacheForTest" {
				found = true
			}
			return !found
		})
		return !found
	})
	return found
}
