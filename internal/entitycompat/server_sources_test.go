package entitycompat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
)

// serverPkg is the package whose wiring and route registrations the gates in
// this package read. They read every non-test source of it, not a single file.
const serverPkg = "internal/server"

// mustReadServerFiles names files that serverSourceFiles must return.
//
// **何も拾えなかったら落とす (CLAUDE.md Section 4)。** `git ls-files` の
// 呼び方を誤ると、空の一覧を全ゲートへ渡して「抽出が 0 件」以外の形で緑になり
// うる (例: `countInlineHandlers` は 0 件が正常)。組み立ての部品化 (#3666) で
// 動かしにくいファイルを名指しする。`router.go` は分割の対象なので名指ししない。
var mustReadServerFiles = []string{
	"internal/server/server.go",
	"internal/server/wiring_check.go",
}

// serverSource is one non-test source file of internal/server.
type serverSource struct {
	rel string // repository-relative, e.g. internal/server/router.go
	abs string
	src []byte
}

// serverSources returns the git-tracked non-test .go files directly under
// internal/server, sorted by path.
//
// **`git ls-files` で列挙する (CLAUDE.md Section 4)。** ディスクを走査すると、
// `git add` を忘れた新しいファイルが手元でだけ読まれ、CI で初めて落ちる。
//
// **サブディレクトリは読まない。** `internal/server/middleware` は別の
// パッケージで、ルートの登録も配線も持たない。混ぜると `api.Use(...)` の
// ような文字列がそちらにも現れうる。
//
// **`router.go` を名指ししない (#3672)。** 組み立てのコードを別のファイルへ
// 移すたびに、ゲートが「抽出が 0 件」で落ちないようにするため。
func serverSources(t *testing.T) []serverSource {
	t.Helper()
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--", serverPkg).Output()
	if err != nil {
		t.Fatalf("git ls-files %s: %v", serverPkg, err)
	}
	untracked := untrackedServerFiles(t, root)
	var files []serverSource
	for _, rel := range strings.Split(string(out), "\x00") {
		if !isServerSource(rel) {
			continue
		}
		abs := filepath.Join(root, filepath.FromSlash(rel))
		src, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		files = append(files, serverSource{rel: rel, abs: abs, src: src})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	got := map[string]bool{}
	for _, f := range files {
		got[f.rel] = true
	}
	for _, want := range mustReadServerFiles {
		if !got[want] {
			t.Fatalf("git ls-files が %s を返さない (%d ファイル)。列挙の仕方が壊れている%s",
				want, len(files), untrackedHint(untracked))
		}
	}
	return files
}

// isServerSource reports whether a repository-relative path is a non-test .go
// file directly under internal/server.
func isServerSource(rel string) bool {
	return rel != "" && filepath.ToSlash(filepath.Dir(rel)) == serverPkg &&
		strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go")
}

var (
	untrackedHintMu   sync.Mutex
	untrackedHintSeen = map[*testing.T]bool{}
)

// untrackedServerFiles returns the non-test .go files directly under
// internal/server that git does not track, and arranges for the test to name
// them if it fails.
//
// **git add を忘れた新しいファイルは、手元でも CI でも読まれない (#3672
// レビュー L-5)。** 列挙は `git ls-files` なので結果はずれないが、`go build` は
// 通るので、落ちた人は「配線が無い」「登録が無い」の原因に気付きにくい。
func untrackedServerFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--others", "--exclude-standard", "--", serverPkg).Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if isServerSource(rel) {
			files = append(files, rel)
		}
	}
	if len(files) == 0 {
		return nil
	}
	untrackedHintMu.Lock()
	seen := untrackedHintSeen[t]
	untrackedHintSeen[t] = true
	untrackedHintMu.Unlock()
	if !seen {
		t.Cleanup(func() {
			if t.Failed() {
				t.Log(strings.TrimSpace(untrackedHint(files)))
			}
			untrackedHintMu.Lock()
			delete(untrackedHintSeen, t)
			untrackedHintMu.Unlock()
		})
	}
	return files
}

// untrackedHint formats the untracked files for a failure message.
func untrackedHint(files []string) string {
	if len(files) == 0 {
		return ""
	}
	return fmt.Sprintf("\ninternal/server に git add していない .go がある: %v。"+
		"ゲートは git ls-files で列挙するので、これらは読まれない (git add を忘れていないか)", files)
}

// serverSourcePaths returns the absolute paths of serverSources.
func serverSourcePaths(t *testing.T) []string {
	t.Helper()
	files := serverSources(t)
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.abs
	}
	return out
}

// relPath returns p relative to the repository root, for diagnostics. Paths
// outside the repository (test fixtures) are returned as they are.
func relPath(t *testing.T, p string) string {
	t.Helper()
	if r, err := filepath.Rel(repoRoot(t), p); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return p
}

// 列挙がテスト以外の .go を internal/server の直下に限って返すこと。
//
// **ゲート側の下限 (名指しの endpoint や配線) だけでは、列挙が広がりすぎた
// ことに気付けない。** middleware や `_test.go` を読み始めると、テストの中の
// 文字列が配線として数えられ、配線を消しても緑のまま通る。
func TestServerSourcesListsPackageFilesOnly(t *testing.T) {
	files := serverSources(t)
	if len(files) < 30 {
		t.Fatalf("internal/server の非テストの .go が %d ファイルしかない (30 以上あるはず)", len(files))
	}
	// **列挙のフィルタとは別の経路で「追跡済みの非テストの .go」を数える。**
	// ディスクのファイルを 1 つずつ `git ls-files --error-unmatch` に問う。
	// 列挙が 1 ファイルでも落とすと (組み立てを移した先を読み落とすと)、
	// ここと食い違う。
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(serverPkg)))
	if err != nil {
		t.Fatalf("read %s: %v", serverPkg, err)
	}
	var tracked, listed []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		rel := serverPkg + "/" + name
		if exec.Command("git", "-C", root, "ls-files", "--error-unmatch", "--", rel).Run() != nil {
			continue // 未追跡 (git add 前) のファイルは CI に無い
		}
		tracked = append(tracked, rel)
	}
	for _, f := range files {
		listed = append(listed, f.rel)
	}
	if !slices.Equal(tracked, listed) {
		t.Errorf("列挙 %v が、追跡済みの非テストの .go %v と一致しない", listed, tracked)
	}
	for _, f := range files {
		switch {
		case strings.HasSuffix(f.rel, "_test.go"):
			t.Errorf("%s はテストのファイル", f.rel)
		case filepath.ToSlash(filepath.Dir(f.rel)) != serverPkg:
			t.Errorf("%s は internal/server の直下ではない", f.rel)
		}
	}
}

// writeServerFixture writes a package server file holding body in setup().
func writeServerFixture(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	src := "package server\n\nfunc setup() {\n" + body + "\n}\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// ルートを解析するヘルパーが、渡したファイルを全部読むこと (#3672)。
//
// **組み立てのコードを別のファイルへ移すと、2 つ目以降のファイルにだけ登録が
// ある状態になる。** 先頭のファイルしか読まない形に戻すと、ゲートは移した
// 登録を黙って検査しなくなる (inline の guard は 0 件が正常なので緑のまま)。
func TestRouteParsersReadEveryFile(t *testing.T) {
	dir := t.TempDir()
	a := writeServerFixture(t, dir, "a.go", `
	api := s.echo.Group("/api")
	api.POST("/first", h.Method, middleware.RequireAuth())
	promoGroup := api.Group("/promo")
	promoGroup.POST("/inline-a", func(c echo.Context) error { return nil })
`)
	b := writeServerFixture(t, dir, "b.go", `
	api.POST("/second", h.Method, middleware.RequireAdmin())
	api.GET("/inline-b", func(c echo.Context) error { return nil })
`)

	regs := parseRouteRegistrations(t, a, b)
	if _, ok := regs["second"]; !ok {
		t.Errorf("2 つ目のファイルの登録を読んでいない: %v", keysOfString(regs))
	}
	if _, ok := regs["first"]; !ok {
		t.Errorf("1 つ目のファイルの登録を読んでいない: %v", keysOfString(regs))
	}
	if got := parseRouteLevels(t, a, b)["second"]; got != "admin" {
		t.Errorf("2 つ目のファイルの権限が %q (admin のはず)", got)
	}
	if got, want := countInlineHandlers(t, a, b), []string{"/inline-a", "/inline-b"}; !slices.Equal(got, want) {
		t.Errorf("countInlineHandlers = %v, want %v", got, want)
	}
	if got := scanInlineRoutes(t, a, b); len(got["inline-b"]) != 1 {
		t.Errorf("scanInlineRoutes が 2 つ目のファイルの closure を拾っていない: %v", got)
	}
	if got := parseGetRoutes(t, a, b); !got["inline-b"] {
		t.Errorf("parseGetRoutes が 2 つ目のファイルの GET を拾っていない: %v", got)
	}
}

// recordCriticalWiring の表を別のファイルへ移しても数えること (#3672)。
func TestCountCriticalWiringEntriesReadsEveryFile(t *testing.T) {
	dir := t.TempDir()
	a := writeServerFixture(t, dir, "a.go", `
	x := 1
`)
	b := writeServerFixture(t, dir, "b.go", `
	s.recordCriticalWiring([]criticalWiring{{"a", true}, {"b", true}})
`)
	if got := countCriticalWiringEntries(t, a, b); got != 2 {
		t.Errorf("countCriticalWiringEntries = %d, want 2", got)
	}
}

func keysOfString(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// どれを検査するか決まらない形を落とすこと (#3672 レビュー M-1 / L-1 / L-4)。
func TestRouteRegistrationsAcrossFiles(t *testing.T) {
	const post = "api.POST(\"/emojis\", h.List, middleware.RequireAuth())\n"
	const get = "api.GET(\"/emojis\", h.List)\n"

	// 同じ method と endpoint を 2 つのファイルが登録していたらエラー。
	if _, err := routeRegistrationsFrom([]string{"a.go", "b.go"}, []string{post, post}); err == nil {
		t.Error("同じ POST /emojis を 2 つのファイルが登録しているのにエラーにならない")
	}
	// GET と POST を別々のファイルに分けるのは正当。
	got, err := routeRegistrationsFrom([]string{"a.go", "b.go"}, []string{post, get})
	if err != nil {
		t.Errorf("GET と POST を別々のファイルに分けただけで落ちた: %v", err)
	}
	if _, ok := got["emojis"]; !ok {
		t.Errorf("emojis を拾っていない: %v", got)
	}
	// 別のファイルのコメントの例は、登録として数えない。
	got, err = routeRegistrationsFrom([]string{"a.go", "b.go"}, []string{post, "// 例: " + post})
	if err != nil {
		t.Errorf("コメントの中の api.POST を登録として数えた: %v", err)
	}
	if !strings.Contains(got["emojis"], "RequireAuth") {
		t.Errorf("emojis の登録が %q (a.go のもののはず)", got["emojis"])
	}
}

// recordCriticalWiring を 2 つのファイルで呼んだら落とすこと (#3672 レビュー M-1)。
func TestCountCriticalWiringInRejectsTwoCalls(t *testing.T) {
	const call = "package server\n\nfunc f() {\n\ts.recordCriticalWiring([]criticalWiring{{\"a\", true}})\n}\n"
	if _, err := countCriticalWiringIn([]string{"a.go", "b.go"}, []string{call, call}); err == nil {
		t.Error("2 つのファイルの recordCriticalWiring を数えてしまった")
	}
	if n, err := countCriticalWiringIn([]string{"a.go"}, []string{call}); err != nil || n != 1 {
		t.Errorf("countCriticalWiringIn = %d, %v (1 のはず)", n, err)
	}
}
