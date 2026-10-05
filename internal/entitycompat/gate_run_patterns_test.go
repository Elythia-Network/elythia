package entitycompat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runFlagRe extracts a `-run` value in any of the three shells write it in.
//
// **引用符を要求しない。** `limitspec-check` は `-run TestLimitSpecDrift` と
// 裸で書いており、`'…'` 前提の抽出では**その target だけ静かに検査対象から
// 落ちていた** (#2857 の初版が実際にそうだった)。
var runFlagRe = regexp.MustCompile(`-run[=\s]+('[^']*'|"[^"]*"|\S+)`)

// goTestPkgRe matches the package operands of a `go test` command.
var goTestPkgRe = regexp.MustCompile(`(^|\s)((?:\./|github\.com/)\S*)`)

// entitycompatDir is the package these gates live in.
const entitycompatDir = "internal/entitycompat"

// makeDryRun asks make what it would run, without running it.
//
// **Makefile を自前でパースしない** (#2857)。行継続・列 0 のコメント・recipe 中の
// 空行・同一 target の複数ルール・集約 target — どれも make の仕様で、自前の
// パーサに継ぎ足していくと**手当てするたびに隣の穴が開く**。3 周かけてそれを
// 繰り返したので、解決は make 自身にやらせる。`$(shell ...)` はこの Makefile に
// 無いので `-n` に副作用も無い。
func makeDryRun(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("make", append([]string{"-n"}, args...)...)
	cmd.Dir = repoRoot(t)
	// **exit code を見ない。** `-p` はデータベースを出したあと非ゼロで終わる
	// ことがある。欲しいのは出力なので、空だったときだけ落とす。
	out, _ := cmd.Output()
	if len(out) == 0 {
		t.Fatalf("make -n %v の出力が空", args)
	}
	// **make の出力にも行継続がそのまま残る。** 畳まないと `go test` と `-run`
	// が別の行に分かれ、その target を認識できない (実測)。
	return strings.ReplaceAll(string(out), "\\\n", " ")
}

// gateCommand is one `go test … -run …` that `make` would execute.
type gateCommand struct {
	pattern string
	dir     string
}

// gateCommandsOf returns every `go test … -run …` the target would run.
func gateCommandsOf(t *testing.T, target string) []gateCommand {
	t.Helper()
	var out []gateCommand
	for _, line := range strings.Split(makeDryRun(t, target), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "go test ") {
			continue
		}
		// **すべての `go test` 行を見る。** 1 本目しか見ないと、recipe を
		// 2 行に割った瞬間 2 本目が未検査になる。
		all := runFlagRe.FindAllStringSubmatch(line, -1)
		if len(all) == 0 {
			continue
		}
		// **最後の `-run` を採る。** go test は後勝ちなので、前を見ると実際に
		// 走るものと違うパターンを検査してしまう。
		cmd := gateCommand{pattern: strings.Trim(all[len(all)-1][1], `'"`), dir: entitycompatDir}
		if m := goTestPkgRe.FindStringSubmatch(line); m != nil {
			cmd.dir = packageDir(m[2])
		}
		out = append(out, cmd)
	}
	return out
}

// packageDir maps a `go test` package operand to a repo-relative directory.
func packageDir(operand string) string {
	p := strings.TrimSuffix(operand, "...")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "github.com/shiroha-a/mk")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "."
	}
	return p
}

// trackedTestNames returns the top-level Test* functions declared in the
// git-tracked test files under dir.
//
// **git が知っているファイルだけを見る。** ディスクを走査すると、`git add` を
// 忘れた新規ゲートが手元では見つかってしまい、CI で初めて落ちる。#2840 で
// 実際にその状態になった (`webpush_producer_test.go` が untracked のまま
// `make wiring-check` が緑だった)。
func trackedTestNames(t *testing.T, dir string) []string {
	t.Helper()
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", filepath.Join(dir, "*_test.go")).Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var names []string
	for _, rel := range strings.Fields(string(out)) {
		f, perr := parser.ParseFile(token.NewFileSet(), filepath.Join(root, rel), nil, 0)
		if perr != nil {
			// tracked だが作業ツリーから消えている形も「テストが無い」として扱う。
			if os.IsNotExist(perr) {
				continue
			}
			t.Fatalf("parse %s: %v", rel, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			names = append(names, fn.Name.Name)
		}
	}
	return names
}

// splitTopLevelAlternatives splits a regexp on `|` at paren depth 0.
func splitTopLevelAlternatives(pattern string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range pattern {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case '|':
			if depth == 0 {
				out = append(out, pattern[start:i])
				start = i + 1
			}
		}
	}
	return append(out, pattern[start:])
}

// plainAlternationRe matches patterns made only of test-name characters,
// grouping parens and `|`.
var plainAlternationRe = regexp.MustCompile(`^[A-Za-z0-9_()|/]+$`)

// expandAlternatives turns `Test(A|B)C` into `TestAC` / `TestBC`.
//
// **グループの中も 1 名前ずつ見る。** 括弧を 1 要素として扱うと、
// `Test(Foo|ZZZNope)IsWired` のように**死んだ名前がグループに紛れても素通り**する。
// 13 名前を括り出すのは自然な可読性リファクタなので、その瞬間に名前単位の検査が
// 消えるのは困る。
//
// **展開するのはテスト名と `(` `)` `|` `/` だけで書かれた形に限る。** 量指定子や
// 文字クラスが混ざったら 1 要素として返す (誤った展開でありもしない名前を探すより、
// 検出力を落とすほうが安全)。
func expandAlternatives(pattern string) []string {
	if !plainAlternationRe.MatchString(pattern) {
		return splitTopLevelAlternatives(pattern)
	}
	var out []string
	for _, alt := range splitTopLevelAlternatives(pattern) {
		open := strings.Index(alt, "(")
		if open < 0 {
			out = append(out, alt)
			continue
		}
		closeAt := matchingParen(alt, open)
		if closeAt < 0 {
			out = append(out, alt)
			continue
		}
		for _, inner := range expandAlternatives(alt[open+1 : closeAt]) {
			out = append(out, expandAlternatives(alt[:open]+inner+alt[closeAt+1:])...)
		}
	}
	var cleaned []string
	for _, v := range out {
		if v != "" {
			cleaned = append(cleaned, v)
		}
	}
	if len(cleaned) == 0 {
		return splitTopLevelAlternatives(pattern)
	}
	return cleaned
}

// matchingParen returns the index of the `)` closing the `(` at open.
func matchingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// gatesPrerequisites returns the prerequisite targets of `gates:` as make
// resolves them.
func gatesPrerequisites(t *testing.T) []string {
	t.Helper()
	src := readRepoFile(t, "Makefile")
	var out []string
	// 同一 target の複数ルールは make が前提を結合するので、全ルールを読む。
	for _, loc := range regexp.MustCompile(`(?m)^gates:`).FindAllStringIndex(src, -1) {
		line := joinContinuedLines(src[loc[1]:])
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		out = append(out, strings.Fields(line)...)
	}
	return out
}

// `make gates` の各 target が名指しするテストが実在するか検査する (#2857)。
//
// **`go test -run` は該当が無くても成功する。** `ok ... [no tests to run]` で
// exit 0 になるので、**ゲートが消えても検査は緑**になる。ゲートは「壊したら
// 落ちる」ことが唯一の価値なので、消えても緑では意味がない。
//
// #2840 で実際に踏んだ。新設したゲートファイルが untracked のままで、
// `make wiring-check` は PASS が 12 件から 11 件に減るだけで何も言わずに通った。
//
// **件数ではなく名前で突き合わせる。** 期待件数を別に持つと、それ自体が
// 同期を要する第 2 の一覧になる。`-run` に書かれた名前がそのまま一覧なので、
// 「その名前に一致するテストが 1 つ以上あるか」だけを見れば足りる。
//
// **完全一致にはしない。** `shapecheck` の `ShapeL2` は複数のテストにまとめて
// マッチする substring、`notfound-check` の `TestScanCollapsedLookups` は
// `_APILayer` / `_CoreLayer` をまとめて指す前方一致。厳密にすると正当な書き方が
// 落ちる。接頭辞を保つ rename は `-run` でも引き続き当たるので、検出したいのは
// 「1 つも当たらなくなった」状態だけ。
func TestGateRunPatternsResolve(t *testing.T) {
	targets := gatesPrerequisites(t)
	if len(targets) == 0 {
		t.Fatal("Makefile の `gates:` から前提を 1 つも読めなかった")
	}
	// **`gates:` の外にあるゲート target も見る (#2898)。** `frontend-check` は
	// frontend/ のソースを読むので意図的に `gates:` から外してあるが、そこで
	// 名指しされたテストが消えても `go test -run` は `[no tests to run]` で
	// exit 0 になり、検査が止まったことに気付けない。#2857 が塞いだのと同じ型が
	// このぶんだけ残っていた。
	targets = append(targets, "frontend-check")
	// `upstream-check` も本家の取得が要るので `gates:` の外に置いてある (#3378)。
	targets = append(targets, "upstream-check")

	seen := make(map[string]bool)
	for _, target := range targets {
		cmds := gateCommandsOf(t, target)
		if len(cmds) == 0 {
			// **ゲートが空洞化していないか。** recipe から `go test` を消す /
			// コメントアウトすると、`make gates` は何も言わずに exit 0 で通る。
			// 「検証のために一時的に外して戻し忘れる」が実際に起きた形 (#2701)。
			t.Errorf("make %s は `-run` 付きの `go test` を 1 つも回さない。"+
				"ゲートを外したまま `gates:` に残すと、検査していないのに緑になる", target)
			continue
		}
		for _, cmd := range cmds {
			seen[cmd.pattern] = true
			names := trackedTestNames(t, cmd.dir)
			if len(names) == 0 {
				t.Errorf("make %s: %s に tracked なテストが 1 つも無い", target, cmd.dir)
				continue
			}
			for _, alt := range expandAlternatives(cmd.pattern) {
				// subtest 指定 (`TestX/sub`) は親の名前だけ見る。
				name := strings.SplitN(alt, "/", 2)[0]
				re, cerr := regexp.Compile(name)
				if cerr != nil {
					t.Errorf("make %s: -run の %q が正規表現として不正: %v", target, alt, cerr)
					continue
				}
				if !matchesAny(re, names) {
					t.Errorf("make %s が名指しする %q に一致するテストが %s に無い。\n"+
						"消した / rename した / `git add` を忘れたなら、Makefile の -run も直すこと。\n"+
						"**このずれは go test では検出できない** — 該当なしは exit 0 で通る。",
						target, alt, cmd.dir)
				}
			}
		}
	}

	// **ゲートが `gates:` から外れていないかも見る。** -run が解決しても、
	// 一括実行の対象から落ちていれば誰も回さない。同じ「黙って検査が止まる」型。
	// 対象の列挙も make のデータベースに任せる。
	for _, cmd := range allEntitycompatGateCommands(t) {
		if !seen[cmd.pattern] {
			t.Errorf("`-run %q` を回す target が `gates:` から辿れない。"+
				"一括実行から漏れると誰も回さない", cmd.pattern)
		}
	}
}

// allEntitycompatGateCommands returns every entitycompat gate command declared
// anywhere in the Makefile, as make's own database reports it.
func allEntitycompatGateCommands(t *testing.T) []gateCommand {
	t.Helper()
	var out []gateCommand
	for _, line := range strings.Split(makeDryRun(t, "-p"), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, "go test ") || !strings.Contains(line, entitycompatDir) {
			continue
		}
		all := runFlagRe.FindAllStringSubmatch(line, -1)
		if len(all) == 0 {
			continue
		}
		out = append(out, gateCommand{pattern: strings.Trim(all[len(all)-1][1], `'"`)})
	}
	return out
}

// matchesAny reports whether any name matches the pattern.
func matchesAny(re *regexp.Regexp, names []string) bool {
	for _, n := range names {
		if re.MatchString(n) {
			return true
		}
	}
	return false
}
