package entitycompat

import (
	"go/ast"
	"go/token"
	"testing"

	"github.com/stretchr/testify/require"
)

// captcha の provider 集合は起動時の meta スナップショットから組まれるので、
// **`metaUpdated` に繋がないと管理画面で有効にしても再起動まで一切検証されない**。
//
// 症状が見えないのが問題。`/api/meta` は DB を読むのでフロントは captcha
// ウィジェットを描画し、**運営者からは ON に見える**。provider が 1 つも無い
// ときの `Verify` は成功を返すので、有効化したつもりのまま `/api/signup` と
// `/api/signin` が素通りする。クレデンシャルスタッフィング対策が効かない。
//
// `internal/server` は CI のカバレッジ対象外 (CLAUDE.md Section 4) で router を
// 組み立てるテストも無いため、配線の抜けは build もテストも緑のまま起きる。
//
// **`assertWired` では足りない。** あちらは normalize 済みの呼び出しを**集合**で
// 持つので、同じ文字列の呼び出しが 2 箇所にあると片方を消しても緑のまま通る
// (変異検証で実測)。ここは 2 箇所を**別々に**見る必要があるので AST で書く。
//
// **2 箇所とも要る。** 自 worker の更新は invalidation hook、他 worker からの
// 受信は subscriber を通る。
//
//   - hook だけ → 別 worker が変えたとき自分に反映されない (複数ノード構成で
//     だけ出る形)
//   - subscriber だけ → Redis が落ちている間、自分で変えても反映されない
//
// **Reload の挙動そのものはここでは見ない。** provider 集合が差し替わること・
// nil meta を据え置くことは `internal/core/captcha` の captcha_test.go が
// 変異検証付きで固定している。ここが見るのは「2 経路とも呼んでいること」だけ。
func TestCaptchaReloadIsWired(t *testing.T) {
	// internal/server の全ファイルを読む (#3672)。組み立てのコードを別の
	// ファイルへ移しても、同じ配線を見続ける。
	_, files := parseServerFiles(t, 0)

	var (
		hookHasReload              bool
		subscriberHasReload        bool
		subInvalidatesBeforeReload bool
		sawHook                    bool
		sawSubscriber              bool
	)

	// callsReload reports whether the func literal body calls reloadCaptcha.
	callsReload := func(n ast.Node) bool {
		found := false
		ast.Inspect(n, func(x ast.Node) bool {
			call, ok := x.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "reloadCaptcha" {
				found = true
				return false
			}
			return true
		})
		return found
	}

	inspectServerFiles(files, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "SetInvalidationHook":
			// cachedMeta.SetInvalidationHook(func() { ... })
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "cachedMeta" {
				return true
			}
			sawHook = true
			for _, a := range call.Args {
				if callsReload(a) {
					hookHasReload = true
				}
			}
		case "Subscribe":
			// internalPubSub.Subscribe(ctx, "metaUpdated", func([]byte) { ... })
			if !hasStringArg(call, "metaUpdated") {
				return true
			}
			sawSubscriber = true
			for _, a := range call.Args {
				if callsReload(a) {
					subscriberHasReload = true
				}
				if invalidatesBeforeReload(a) {
					subInvalidatesBeforeReload = true
				}
			}
		}
		return true
	})

	// **見つからなかったら落とす。** 配線の書き方が変わって探せなくなると、
	// 検査していないのに緑になる。
	require.True(t, sawHook, "cachedMeta.SetInvalidationHook の呼び出しが見つからない。書き方を変えたならこの gate も直すこと")
	require.True(t, sawSubscriber, `"metaUpdated" の Subscribe が見つからない。書き方を変えたならこの gate も直すこと`)

	require.True(t, hookHasReload,
		"cachedMeta の invalidation hook が reloadCaptcha を呼んでいない。\n"+
			"自 worker で meta を更新しても captcha に伝播せず、Redis が落ちている間は反映されない")
	require.True(t, subscriberHasReload,
		`"metaUpdated" の subscriber が reloadCaptcha を呼んでいない。`+"\n"+
			"別 worker が meta を更新したとき captcha に伝播せず、複数ノード構成でだけ古い設定が残る")

	// **reloadCaptcha が実際に差し替えていることも見る。** 上の 2 つは「呼ばれて
	// いること」しか見ないので、closure の中身を空振りさせる変異が素通りする
	// (実測)。こちらは 1 箇所しか無いので集合ベースの assertWired で足りる。
	assertWired(t, serverPkg, "captchaSvc.Reload(m)",
		"reloadCaptcha が呼ばれるだけで provider 集合を差し替えていない")

	// **handler へ渡していること (#3037 レビュー 2 周目)。**
	//
	// 1 周目の gate は「service を無条件に作っているか」「reload が配線されて
	// いるか」しか見ておらず、**その service を handler に渡す 2 行を消しても
	// 緑のまま通った** (実測)。`SetCaptcha` は router.go にしか無く、
	// `internal/api/signup` / `signin` のテストは自前で注入するので、
	// 消えたことに気付く経路がどこにも無い。症状はこの gate の doc が
	// 書いているものそのもの — **有効化したつもりのまま `/api/signup` と
	// `/api/signin` が素通りする**。
	assertWired(t, serverPkg, "signupHandler.SetCaptcha(captchaSvc)",
		"captcha service を signup handler に渡していない (登録が captcha を検証しない)")
	assertWired(t, serverPkg, "signinHandler.SetCaptcha(captchaSvc)",
		"captcha service を signin handler に渡していない (サインインが captcha を検証しない)")

	// **subscriber は先に meta の cache を落とすこと。** `reloadCaptcha` は
	// `cachedMeta.Fetch()` を読むので、`Invalidate` を落とすと**最大 5 分古い
	// スナップショット**から provider を組み直してしまい、入れ替わらない。
	// 呼ばれていることだけを見る形ではこの変異が素通りする。
	// **service は無条件に作ること。** 起動時に meta を読めなかったときに nil の
	// ままにすると、`SetCaptcha` も呼ばれず `reloadCaptcha` も
	// `captchaSvc == nil` で即 return するので、**その後 meta が読めるように
	// なっても captcha が永久に無効**になる。この配線が塞ごうとしている状態
	// そのもの。空の meta で作っておけば次の `metaUpdated` で provider が入る。
	require.True(t, captchaServiceIsUnconditional(files),
		"captchaSvc を条件付きで作っている。起動時に meta を読めないと captcha が永久に無効になる")

	require.True(t, subInvalidatesBeforeReload,
		`"metaUpdated" の subscriber が cachedMeta.Invalidate() を reloadCaptcha より前に呼んでいない。`+"\n"+
			"古い meta から provider を組み直すので、管理画面の変更が反映されない")
}

// captchaServiceIsUnconditional reports whether captchaSvc is assigned by a
// plain `:=` (not declared with `var` and filled inside an `if`).
//
// **形で見るしかない。** `internal/server` は CI のカバレッジ対象外で router を
// 組み立てるテストも無いので、nil のまま残る経路は build もテストも緑のまま
// 起きる。
func captchaServiceIsUnconditional(files []*ast.File) bool {
	// **`:=` の右辺が constructor そのものであることまで見る (#3037 レビュー)。**
	//
	// 「`:=` があって `var` が無い」だけだと、
	//
	//	captchaSvc := (*corecaptcha.Service)(nil)
	//	if m, err := metaRepo.Fetch(); err == nil { captchaSvc = ... }
	//
	// が gate 緑のまま同じバグを完全に再導入する。**変異検証で押さえたのは
	// 巻き戻し前の特定 diff 1 形だけだった。**
	assigned := false
	conditional := false
	aliases := sectionDepsAliases(files)
	inspectServerFiles(files, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.DeclStmt:
			// `var captchaSvc *corecaptcha.Service` を探す。
			gd, ok := t.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, name := range vs.Names {
					if name.Name == "captchaSvc" {
						conditional = true
					}
				}
			}
		case *ast.AssignStmt:
			// 節の受け直し (`captchaSvc := d.captchaSvc`、#3901) は作り方ではない。
			if aliases[t] {
				return true
			}
			for i, lhs := range t.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name != "captchaSvc" {
					continue
				}
				if t.Tok != token.DEFINE {
					// 後から代入し直す形 (= 条件付き)。
					conditional = true
					continue
				}
				if i < len(t.Rhs) && isCaptchaConstructorCall(t.Rhs[i]) {
					assigned = true
					continue
				}
				// `:=` だが右辺が constructor でない (nil 初期化など)。
				conditional = true
			}
		}
		return true
	})
	return assigned && !conditional
}

// isCaptchaConstructorCall reports whether e is
// `corecaptcha.NewServiceWithClient(...)`.
func isCaptchaConstructorCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewServiceWithClient" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "corecaptcha"
}

// invalidatesBeforeReload reports whether the body calls
// `cachedMeta.Invalidate()` before `reloadCaptcha()`.
//
// **順序を見るのが要点。** `reloadCaptcha` は `cachedMeta.Fetch()` を読むので、
// 先に cache を落としていないと古いスナップショットから provider を組み直す。
// 呼び出しの有無だけでは検出できない。
func invalidatesBeforeReload(n ast.Node) bool {
	invalidateAt, reloadAt := token.NoPos, token.NoPos
	ast.Inspect(n, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if id, ok := fun.X.(*ast.Ident); ok && id.Name == "cachedMeta" && fun.Sel.Name == "Invalidate" {
				if !invalidateAt.IsValid() {
					invalidateAt = call.Pos()
				}
			}
		case *ast.Ident:
			if fun.Name == "reloadCaptcha" && !reloadAt.IsValid() {
				reloadAt = call.Pos()
			}
		}
		return true
	})
	return invalidateAt.IsValid() && reloadAt.IsValid() && invalidateAt < reloadAt
}

// hasStringArg reports whether the call has the given basic string literal arg.
func hasStringArg(call *ast.CallExpr, want string) bool {
	for _, a := range call.Args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		if len(lit.Value) >= 2 && lit.Value[1:len(lit.Value)-1] == want {
			return true
		}
	}
	return false
}
