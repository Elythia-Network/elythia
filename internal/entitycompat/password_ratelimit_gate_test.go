package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shiroha-a/mk/internal/server/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const modulePath = "github.com/shiroha-a/mk/"

// passwordVerifiers are the (import path, function) pairs that compare a
// user-supplied password against the stored hash.
var passwordVerifiers = map[string]string{
	"golang.org/x/crypto/bcrypt":          "CompareHashAndPassword",
	modulePath + "internal/misc/password": "Verify",
}

// passwordGuardCall is the method a handler calls to reserve a check against
// the per-account failure budget (`internal/api/i/password_check.go`).
const passwordGuardCall = "beginPasswordCheck"

// routeLimitedVerifiers lists the functions allowed to compare a password
// without the per-account guard, and why. Their routes must carry a route
// limit instead (checked below).
var routeLimitedVerifiers = map[string]string{
	"internal/api/signin#verifyPasswordTolerant": "未認証の signin 系。守るのは試行する側の IP で、" +
		"upstream と同じく `signin` / `signin-flow` の route 上限 (TOO_MANY_AUTHENTICATION_FAILURES) が受ける",
}

// パスワードを照合する経路には総当たりの上限が要る。
//
// **token を持つ攻撃者は、パスワードを照合する `i/*` を総当たりのオラクルに
// 使える。** 当たればパスワードそのものが手に入り、token を失効させても
// signin し直せる。
//
// **上限は route ごとの limiter ではなく handler の passwordguard に置く。**
// limiter は RequireAuth / RequireSecure より前に走り、成否に関係なく user
// bucket を消費するので、被害者の token を持つだけの第三者が
// `i/regenerate-token` (token 漏洩時の唯一の対処) を使い切れる。endpoint ごとの
// bucket だと合計の試行回数も endpoint の数だけ増える。passwordguard は
// **照合に失敗した回数だけ**をアカウント単位の 1 つの key で数える。
//
// 見るもの:
//   - `internal/api` の各関数のうち、パスワードを直接照合するものは
//     照合より前に `beginPasswordCheck` を呼ぶ。呼ばないものは
//     routeLimitedVerifiers に理由付きで載っていること
//   - routeLimitedVerifiers に届く route (同 package 内で推移的に導出) は
//     DefaultEndpointLimits に 1 時間 10 回以下の上限を持つこと
//   - router が guard を配線していること (nil だと素通しになる)
//
// **既知の取りこぼし**: 別 package (core のサービス) の中で照合する形、
// handler を変数ではなく式で渡す登録、パスワード以外の秘密 (TOTP など) は
// 見ない。`beginPasswordCheck` の戻り値を捨てて照合する形も見ない
// (振る舞いは internal/api/i の TestPasswordEndpoints_* が見る)。
func TestPasswordChecksAreFailureLimited(t *testing.T) {
	root := filepath.Join("..", "..")
	scans := scanAPIPasswordVerifiers(t, root)

	// **抽出の下限を名指しで持つ。** 違反 0 件が正常な検査なので、抽出が
	// 壊れると「検出 0 件」と区別が付かない。
	for _, want := range []string{"internal/api/i#comparePassword", "internal/api/i#ChangePassword"} {
		require.Contains(t, scans.guarded, want, "guard 付きの照合の抽出が壊れている")
	}

	seen := map[string]bool{}
	for _, key := range scans.unguarded {
		seen[key] = true
		if _, ok := routeLimitedVerifiers[key]; !ok {
			t.Errorf("%s はパスワードを照合するのに、照合より前に %s を呼んでいない。\n"+
				"token を持つ攻撃者がパスワードを無制限に総当たりできる。i/* なら\n"+
				"h.comparePassword を使うか、照合の前に h.beginPasswordCheck で予約すること。\n"+
				"route の limiter に上限を置く形にはしない (token を持つ第三者が使い切れる)",
				key, passwordGuardCall)
		}
	}
	for key, reason := range routeLimitedVerifiers {
		assert.True(t, seen[key], "routeLimitedVerifiers の %s は実在しない (死んだ entry)", key)
		assert.NotEmpty(t, reason, "%s に理由が無い", key)
	}

	routes := routeLimitedPasswordRoutes(t, root)
	for _, want := range []string{"signin", "signin-flow"} {
		require.Contains(t, routes, want, "route 上限で守る照合 route の抽出が壊れている")
	}
	for _, ep := range routes {
		t.Run(ep, func(t *testing.T) {
			limit, ok := middleware.DefaultEndpointLimits[ep]
			require.True(t, ok, "%s は handler の guard を持たずにパスワードを照合するのに、"+
				"DefaultEndpointLimits に上限が無い", ep)
			require.Positive(t, limit.Duration, "%s の窓が 0 で、実質無制限", ep)
			require.Positive(t, limit.Max, "%s の上限が 0 で、実質無制限", ep)
			perHour := float64(limit.Max) * float64(time.Hour) / float64(limit.Duration)
			assert.LessOrEqual(t, perHour, 10.0, "%s の上限が 1 時間あたり 10 回より緩い", ep)
		})
	}
}

// guard が nil だと beginPasswordCheck は素通しにするので、配線を落としても
// build もテストも通る。
func TestPasswordFailureGuardIsWired(t *testing.T) {
	assertWired(t, routerGo, "iHandler.SetPasswordFailureGuard(passwordFailureGuard)",
		"現在のパスワードを照合する i/* の総当たりが無制限になる。エラーもログも出ない。")
}

type passwordVerifierScan struct {
	guarded   []string // "<pkg dir>#<func>" that reserve before verifying
	unguarded []string
}

// scanAPIPasswordVerifiers classifies every function under internal/api that
// calls a password verifier directly.
func scanAPIPasswordVerifiers(t *testing.T, root string) passwordVerifierScan {
	t.Helper()
	var out passwordVerifierScan
	apiRoot := filepath.Join(root, "internal", "api")
	dirs := 0
	require.NoError(t, filepath.WalkDir(apiRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		dirs++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, fn := range packageFuncs(t, path) {
			pos := fn.firstVerify
			if pos == token.NoPos {
				continue
			}
			key := filepath.ToSlash(rel) + "#" + fn.name
			if fn.firstGuard != token.NoPos && fn.firstGuard < pos {
				out.guarded = append(out.guarded, key)
			} else {
				out.unguarded = append(out.unguarded, key)
			}
		}
		return nil
	}))
	require.Greater(t, dirs, 50, "internal/api を走査できていない")
	sort.Strings(out.guarded)
	sort.Strings(out.unguarded)
	return out
}

type funcInfo struct {
	name        string
	firstVerify token.Pos
	firstGuard  token.Pos
	callees     map[string]bool // same-package callees by name
}

// packageFuncs parses the non-test Go files in dir.
//
// 名前だけで追う (レシーバの型は見ない) ので、同名メソッドがあると広めに拾う。
// 取りこぼすより安全側。
func packageFuncs(t *testing.T, dir string) []*funcInfo {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	fset := token.NewFileSet()
	var out []*funcInfo
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		require.NoError(t, err)
		aliases := map[string]string{} // local alias -> verifier func name
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			fn, ok := passwordVerifiers[p]
			if !ok {
				continue
			}
			alias := filepath.Base(p)
			if imp.Name != nil {
				alias = imp.Name.Name
			}
			aliases[alias] = fn
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			info := &funcInfo{name: fd.Name.Name, callees: map[string]bool{}}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					info.callees[fun.Name] = true
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok {
						if want, ok := aliases[x.Name]; ok && want == fun.Sel.Name {
							if info.firstVerify == token.NoPos || call.Pos() < info.firstVerify {
								info.firstVerify = call.Pos()
							}
							return true
						}
					}
					if fun.Sel.Name == passwordGuardCall &&
						(info.firstGuard == token.NoPos || call.Pos() < info.firstGuard) {
						info.firstGuard = call.Pos()
					}
					info.callees[fun.Sel.Name] = true
				}
				return true
			})
			out = append(out, info)
		}
	}
	return out
}

// routeLimitedPasswordRoutes returns the router.go endpoints (without the
// `/api/` prefix) whose handler reaches a routeLimitedVerifiers function
// through the same package.
func routeLimitedPasswordRoutes(t *testing.T, root string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, routerGo), nil, 0)
	require.NoError(t, err)

	imports := map[string]string{} // alias -> import path
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := filepath.Base(p)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		imports[name] = p
	}

	// handler 変数 -> package の import path。`x := pkg.NewHandler(...)` と
	// `x := &pkg.Handler{...}` を拾う。
	handlerPkg := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, lhs := range as.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			if p := pkgOfExpr(as.Rhs[i], imports); p != "" {
				handlerPkg[id.Name] = p
			}
		}
		return true
	})

	reaching := map[string]map[string]bool{} // import path -> method names
	var out []string
	resolved := 0
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (sel.Sel.Name != "POST" && sel.Sel.Name != "GET") {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "api" {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		path, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		h, ok := call.Args[1].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		hv, ok := h.X.(*ast.Ident)
		if !ok {
			return true
		}
		pkg, ok := handlerPkg[hv.Name]
		if !ok || !strings.HasPrefix(pkg, modulePath+"internal/api/") {
			return true
		}
		resolved++
		if _, done := reaching[pkg]; !done {
			rel := strings.TrimPrefix(pkg, modulePath)
			reaching[pkg] = funcsReaching(packageFuncs(t, filepath.Join(root, rel)), rel)
		}
		if reaching[pkg][h.Sel.Name] {
			out = append(out, strings.TrimPrefix(path, "/"))
		}
		return true
	})
	// handler 変数の解決が壊れると全 route が素通りになる。
	require.Greater(t, resolved, 300, "router.go の handler 変数を package へ解決できていない")
	sort.Strings(out)
	return out
}

// funcsReaching returns the functions of the package rel that reach a
// routeLimitedVerifiers entry of the same package.
func funcsReaching(funcs []*funcInfo, rel string) map[string]bool {
	found := map[string]bool{}
	for _, fn := range funcs {
		if _, ok := routeLimitedVerifiers[rel+"#"+fn.name]; ok {
			found[fn.name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range funcs {
			if found[fn.name] {
				continue
			}
			for c := range fn.callees {
				if found[c] {
					found[fn.name] = true
					changed = true
					break
				}
			}
		}
	}
	return found
}

// pkgOfExpr returns the import path of `pkg.F(...)` / `&pkg.T{...}`.
func pkgOfExpr(e ast.Expr, imports map[string]string) string {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	var fun ast.Expr
	switch v := e.(type) {
	case *ast.CallExpr:
		fun = v.Fun
	case *ast.CompositeLit:
		fun = v.Type
	default:
		return ""
	}
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return imports[x.Name]
}
