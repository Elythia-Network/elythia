package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **組み立て (`New` / `newServer` / router) は、プロセス共有の状態を書き換えない
// こと** (#2795、#3803)。
//
// 以前の `newServer` はサーバーを 1 度だけ起動する前提で書かれていて、`pkg.SetXxx(...)`
// でグローバルを無条件に差し替えていた。テストは同じプロセスで何度も組み立てるので、
// `go test -shuffle` を有効にすると、その後ろに並んだ `avatar` / `emoji_redirect` が
// **署名付きプロキシ URL を受け取って落ちた** (#2795 で 5 seed すべて失敗した)。
// そのため、組み立てが書き換えるものを `restoreProcessGlobals` で零値に戻し、この
// テストがその一覧と実際の呼び出しを突き合わせていた。
//
// 書き換えるものは、使う部品へ依存として渡す形にして 1 つずつ消した。設定だけで
// 決まる3つ(`coretwofactor.SetTestMode` / `password.SetCost` /
// `corenote.SetHookConcurrency`)は#3780、`meself.SetEnricher`は#3784、`notehide`の
// 2つは#3786、`entity`の8つの`Set*`は#3790〜#3800。組み立ての外で書かれていた
// 2つ(初回セットアップの印と frontend の loader のキャッシュ)も、組み立てごとの
// 値にして#3803で消したので、`restoreProcessGlobals` も消した。
//
// 判定はソースの文字列一致。**コメント行は数えない** (コメントアウトして残すのは
// 消すのと同じ)。同 package の `wiring_check` が生ソースを読む形なのに合わせて
// ある。
//
// **どのファイルで呼ぶかは見ない (#3672)。** 以前は `router.go` と `server.go` の
// 2 つだけを読んでいたので、setter を別のファイルへ移すと (あるいは別のファイルで
// 足すと) 一覧から黙って消えた。今はパッケージ全体を読み、呼び出しの集合だけを
// 見る。
func TestConstructionSetsNoProcessGlobals(t *testing.T) {
	found, _ := processGlobalSetters(t)

	// 期待値が空なので、抽出が空振りしても緑になる。検出そのものは
	// TestProcessGlobalSettersReadEveryFile が fixture で確かめる。
	got := make([]string, 0, len(found))
	for call := range found {
		got = append(got, call)
	}
	sort.Strings(got)

	assert.Empty(t, got,
		"組み立てがプロセス共有の状態を書き換えている。世代ごとに作り直せるよう、"+
			"使う部品へ依存として渡す形にすること (#3803)。呼んでいるファイル: %v", found)
}

// processGlobalSetters returns every `pkg.SetXxx(...)` call that the non-test
// sources of this package make on an imported package, keyed by `pkg.SetXxx`,
// and every assignment to a variable of an imported package, keyed by
// `pkg.Var =` (or `pkg.Var ++` / `--`), with the files that make it. It also
// returns the files it read.
//
// 数えない形: 自分のパッケージの変数への代入、メソッドでの書き換え
// (`meta.V.Store(1)`・`meta.Once.Do(f)`)、`Set` で始まらない setter、dot import、
// `range` の左辺、ポインタの別名を経由した書き換え。普通に書いたときに踏む形を
// 落とすためのゲートで、意図的な回避は振る舞いのテストで受ける (CLAUDE.md
// Section 4)。逆に、import した名前を引数や局所変数で隠すと誤って数える
// (落ちるので気付ける)。
func processGlobalSetters(t *testing.T) (map[string][]string, []string) {
	t.Helper()
	return globalSettersIn(t, packageSources(t))
}

// globalSettersIn is processGlobalSetters over the given files. It returns the
// names of the files it parsed.
//
// **メソッド呼び出し (`s.foo.SetBar(...)`) は除く。** それはインスタンスの
// 設定であってプロセス共有の状態ではない。パッケージ名は import で解決する
// (import はファイルごとなので、ファイルごとに引く)。
func globalSettersIn(t *testing.T, files []packageSource) (map[string][]string, []string) {
	t.Helper()
	return globalSettersInSeen(t, files, nil)
}

// globalSettersInSeen is globalSettersIn that also records, in seen when it is
// not nil, the package names the files import.
func globalSettersInSeen(t *testing.T, files []packageSource, seen map[string]bool) (map[string][]string, []string) {
	t.Helper()
	out := map[string][]string{}
	var read []string
	for _, file := range files {
		read = append(read, file.name)
		src := nonCommentSource(file.src)
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file.name, src, 0)
		require.NoError(t, err)

		pkgs := map[string]bool{}
		for _, imp := range f.Imports {
			name := importName(strings.Trim(imp.Path.Value, `"`))
			if imp.Name != nil {
				name = imp.Name.Name
			}
			pkgs[name] = true
		}
		if seen != nil {
			for name := range pkgs {
				seen[name] = true
			}
		}

		add := func(key string) {
			if !slices.Contains(out[key], file.name) {
				out[key] = append(out[key], file.name)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok || !strings.HasPrefix(sel.Sel.Name, "Set") {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && pkgs[id.Name] {
					add(id.Name + "." + sel.Sel.Name)
				}
			case *ast.AssignStmt:
				// 他のパッケージの変数への代入 (`http.DefaultClient.Timeout = 1` など)
				// も、setter を通さない書き換えとして数える (#3803 レビュー M-1)。
				for _, lhs := range n.Lhs {
					if key, ok := packageVarRoot(lhs, pkgs); ok {
						add(key + " =")
					}
				}
			case *ast.IncDecStmt:
				if key, ok := packageVarRoot(n.X, pkgs); ok {
					add(key + " " + n.Tok.String())
				}
			}
			return true
		})
	}
	return out, read
}

// importName returns the package name an import path is referred to by
// without an alias.
//
// **パスの最後の要素とパッケージ名が違う形を解く。** `github.com/labstack/echo/v4`
// は `echo`、`github.com/redis/go-redis/v9` は `redis`、`gopkg.in/yaml.v3` は
// `yaml`。最後の要素をそのまま使うと `v4` で登録され、`echo.NotFoundHandler = …`
// を見逃していた (#3803 レビュー 2 周目 M-1)。それ以外で最後の要素とパッケージ名が
// 違う import (`golang-lru/v2` の `lru` など) は、別名を付けて書く前提にする。
func importName(path string) string {
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && regexp.MustCompile(`^v[0-9]+$`).MatchString(name) {
		name = parts[len(parts)-2]
	}
	name = regexp.MustCompile(`\.v[0-9]+$`).ReplaceAllString(name, "")
	return strings.TrimPrefix(name, "go-")
}

// packageVarRoot reports the `pkg.Var` an assignment target writes into when
// it is a variable of an imported package, or a field, element or pointee of
// one.
func packageVarRoot(e ast.Expr, pkgs map[string]bool) (string, bool) {
	for {
		switch x := e.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				if pkgs[id.Name] {
					return id.Name + "." + x.Sel.Name, true
				}
				return "", false
			}
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.SliceExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		default:
			return "", false
		}
	}
}

// setter をパッケージのどのファイルで呼んでも数えること (#3672 レビュー M-1)。
//
// **setter が 1 つも無いので、2 つだけを読む形に戻しても
// TestConstructionSetsNoProcessGlobals は緑のまま通る。** 段階 2 で組み立てを
// 分けると、移した先の setter が一覧から黙って消える。ここで、(a) 2 つ以外の
// ファイルの setter を数えること、(b) processGlobalSetters が packageSources の
// 全ファイルを読むこと、を見る。
func TestProcessGlobalSettersReadEveryFile(t *testing.T) {
	got, _ := globalSettersIn(t, []packageSource{
		{name: "setup.go", src: "package server\n\nimport \"example.com/entity\"\n\nfunc a() { entity.SetA(1) }\n"},
		{name: "routes_users.go", src: "package server\n\nimport \"example.com/notehide\"\n\nfunc b() { notehide.SetB(2); s.x.SetC(3) }\n"},
		{name: "assign.go", src: "package server\n\nimport (\n\t\"net/http\"\n\t\"example.com/meta\"\n)\n\nfunc c() { http.DefaultClient.Timeout = 1; meta.Count++; (*meta.P)[0] = 2; meta.S[1:][0] = 1; s.x.Y = 3; v := 1; v++ }\n"},
		{name: "versioned.go", src: "package server\n\nimport (\n\t\"github.com/labstack/echo/v4\"\n\t\"github.com/redis/go-redis/v9\"\n\t\"gopkg.in/yaml.v3\"\n)\n\nfunc d() { echo.NotFoundHandler = nil; redis.SetLogger(nil); yaml.X = 1 }\n"},
	})
	require.Equal(t, map[string][]string{
		"entity.SetA":            {"setup.go"},
		"notehide.SetB":          {"routes_users.go"},
		"http.DefaultClient =":   {"assign.go"},
		"meta.Count ++":          {"assign.go"},
		"meta.P =":               {"assign.go"},
		"meta.S =":               {"assign.go"},
		"echo.NotFoundHandler =": {"versioned.go"},
		"redis.SetLogger":        {"versioned.go"},
		"yaml.X =":               {"versioned.go"},
	}, got)

	// 本物のソースで、版の付いた import がパッケージ名で解けていること (抽出の下限)。
	seen := map[string]bool{}
	globalSettersInSeen(t, packageSources(t), seen)
	for _, name := range []string{"echo", "redis", "http"} {
		assert.True(t, seen[name], "import %q is not resolved to its package name", name)
	}

	// processGlobalSetters が packageSources の全ファイルを読んでいること。
	_, read := processGlobalSetters(t)
	var listed []string
	for _, f := range packageSources(t) {
		listed = append(listed, f.name)
	}
	require.Equal(t, listed, read, "processGlobalSetters が読んだファイルが、パッケージの非テストの .go と一致しない")
}

// nonCommentSource returns src with comments removed, so commenting a call
// out counts as removing it.
func nonCommentSource(src string) string {
	// **block comment も落とす。** `/* entity.SetSilencedLookup(...) */` は
	// 行コメント判定だけだと生き残り、「コメントアウトして残すのは消すのと
	// 同じ」が成立しなくなる。
	src = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(src, "")
	var kept []string
	for _, line := range strings.Split(src, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
