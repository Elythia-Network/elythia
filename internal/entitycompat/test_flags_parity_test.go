package entitycompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ciOnlyTestFlags lists the flags CI passes that `make test` need not, with the
// reason each is safe to omit.
//
// **理由付きで持つのが要点。** CI に挙動を変える flag が増えたとき、ここに
// 足すか `make test` に足すかを**選ばせる**ためにある。無条件に無視すると
// #2841 と同じことが起きる (seed だけ揃えて `-race` を放置した)。
var ciOnlyTestFlags = map[string]string{
	// **値も検査する** (goDefaultTimeout)。理由が「既定と同じ値だから」なので、
	// 名前だけで除外すると CI が縮めたときに素通りする。
	"-timeout":      "Go の既定と同じ値なので指定しなくても挙動は変わらない",
	"-coverprofile": "カバレッジ閾値チェックのための計装。テストの合否には影響しない",
	"-covermode":    "同上",
}

// makeOnlyTestFlags lists the flags `make test` may add that CI does not pass.
//
// **こちらも allowlist にする。** 片方向だと `make test` に `-short` を足す形が
// 素通りし、「手元だけ skip される」= #2841 と同じ失敗モードになる (実測)。
var makeOnlyTestFlags = map[string]string{
	"-v": "手元は失敗したテスト名がすぐ要る。CI は tee でログを残すので付けない",
}

// goDefaultTimeout is `go test`'s built-in timeout ("go help testflag").
const goDefaultTimeout = "10m"

// makeTestPackages is the package pattern `make test` must use.
//
// **絞ると CI との差が出る。** CI は `go list ./...` から shard を作るので、
// 手元を `./internal/...` に狭めると `tests/e2e*` / `cmd/` / `tools/` /
// `plugin/` が一切走らないまま緑になる (実測)。
const makeTestPackages = "./..."

// testFlagsWithValue are the `go test` flags whose value is a separate token
// when not written as `-flag=value`.
//
// **これが無いと `-timeout 10m` の値を落とす。** 値トークンは `-` で始まらない
// ので、素朴に切ると値が常に空になり、`-shuffle 3` と `-shuffle 7` が
// 一致扱いになる (実測)。
var testFlagsWithValue = map[string]bool{
	"-bench": true, "-benchtime": true, "-blockprofile": true, "-blockprofilerate": true,
	"-count": true, "-coverpkg": true, "-coverprofile": true, "-covermode": true,
	"-cpu": true, "-cpuprofile": true, "-fuzz": true, "-fuzztime": true, "-memprofile": true,
	"-mutexprofile": true, "-outputdir": true, "-parallel": true, "-run": true,
	"-shuffle": true, "-skip": true, "-tags": true, "-timeout": true, "-trace": true,
}

// booleanTestFlags are the `go test` switches for which `-flag` and
// `-flag=true` mean the same thing.
//
// **正規化しないと等価な書き方で落ちる。** `-race` と `-race=true` は同じなのに、
// 値をそのまま比べると「値が違う」と報告してしまう (実測)。
var booleanTestFlags = map[string]bool{
	"-race": true, "-v": true, "-short": true, "-failfast": true,
	"-cover": true, "-benchmem": true, "-json": true, "-fullpath": true,
}

// normalizeFlagValue folds the two spellings of a boolean switch together.
func normalizeFlagValue(name, value string) string {
	if booleanTestFlags[name] && (value == "" || value == "true") {
		return "true"
	}
	return value
}

// parseTestFlags splits a `go test` command line into its flags and its
// positional arguments (the package patterns).
func parseTestFlags(cmd string) (flags map[string]string, args []string) {
	// **パイプライン以降を捨てる。** ci.yml は `2>&1 | tee test_output.txt` で
	// 終わるので、そのまま切ると `tee -a` の `-a` を go test の flag として
	// 読んでしまい、無関係な PR が意味不明な理由で落ちる (実測)。
	if i := strings.IndexAny(cmd, "|>"); i >= 0 {
		cmd = cmd[:i]
	}
	flags = make(map[string]string)
	tokens := strings.Fields(cmd)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") {
			if tok != "go" && tok != "test" {
				args = append(args, tok)
			}
			continue
		}
		if name, value, ok := strings.Cut(tok, "="); ok {
			flags[name] = normalizeFlagValue(name, value)
			continue
		}
		if testFlagsWithValue[tok] && i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
			flags[tok] = tokens[i+1]
			i++
			continue
		}
		flags[tok] = normalizeFlagValue(tok, "")
	}
	return flags, args
}

// joinContinuedLines folds a shell command written with trailing backslashes
// into one line, stopping at the first line without a continuation.
func joinContinuedLines(src string) string {
	var joined strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		cont := strings.HasSuffix(trimmed, `\`)
		joined.WriteString(" " + strings.TrimSuffix(trimmed, `\`))
		if !cont {
			break
		}
	}
	return joined.String()
}

// makeTestCommand extracts the `go test` line of the Makefile's `test:` recipe.
func makeTestCommand(t *testing.T) string {
	t.Helper()
	return makeTargetTestCommand(t, "test")
}

// makeTargetRecipeLines returns the recipe lines of the given target.
//
// **正規表現で `(?:\t.*\n)+` と書かない。** GNU make は recipe 中の空行を
// 読み飛ばして続きも実行するので、そこで切ると**以降の行が検査されなくなる**
// (#2857)。行頭固定なので `test` は `test-fast` を拾わない。
func makeTargetRecipeLines(src, target string) []string {
	head := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(target) + `:.*$`)
	loc := head.FindStringIndex(src)
	if loc == nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(src[loc[1]:], "\n")[1:] {
		// **空行も列 0 のコメントも recipe を終わらせない** (make 4.3 で実測)。
		// ここで切ると以降の行が検査されなくなる (#2857)。
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			break
		}
		out = append(out, line)
	}
	return out
}

// makeTargetTestCommand extracts the `go test` line of the given target's recipe.
func makeTargetTestCommand(t *testing.T, target string) string {
	t.Helper()
	lines := makeTargetRecipeLines(readRepoFile(t, "Makefile"), target)
	if lines == nil {
		t.Fatalf("Makefile の `%s:` recipe を読めなかった", target)
	}
	for i, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "\t"))
		// recipe 内のコメント行に `go test` があっても拾わない。
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if cmd, _ := stripRecipePrefix(trimmed); strings.HasPrefix(cmd, "go test ") {
			// ci.yml と同じ継続行スタイルで書かれても読めるようにする。
			cmd, goflags := stripRecipePrefix(joinContinuedLines(strings.Join(lines[i:], "\n")))
			// GOFLAGS が注入する flag も go test の flag として扱う。
			return cmd + goflags
		}
	}
	t.Fatalf("Makefile の `%s:` recipe に `go test` の行が無い", target)
	return ""
}

// stripRecipePrefix removes make's recipe modifiers (`@` / `-` / `+`) and any
// leading `NAME=value` environment assignments, returning the command and the
// flags that any `GOFLAGS` assignment injects.
//
// **modifier と env は Makefile として普通の書き方。** 剥がさないと
// `@go test ...` や `CGO_ENABLED=1 go test ...` で「recipe に go test が無い」と落ちる。
//
// **ただし `GOFLAGS` は捨てられない。** `go test` に flag を注入できるので、
// `GOFLAGS=-short go test ...` を黙って落とすと「手元だけ skip される」形が
// 素通りする (#2841 が潰した失敗モードそのもの)。
func stripRecipePrefix(line string) (cmd, goflags string) {
	line = strings.TrimLeft(strings.TrimSpace(line), "@-+")
	for {
		field, rest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || !strings.Contains(field, "=") || strings.HasPrefix(field, "-") {
			return strings.TrimSpace(line), goflags
		}
		if name, value, _ := strings.Cut(field, "="); name == "GOFLAGS" {
			goflags += " " + value
		}
		line = rest
	}
}

// ciTestCommand extracts the test-shards `go test` invocation from ci.yml.
func ciTestCommand(t *testing.T) string {
	t.Helper()
	src := readRepoFile(t, ".github/workflows/ci.yml")
	idx := strings.Index(src, "go test ${{ steps.shard.outputs.pkgs }}")
	if idx < 0 {
		t.Fatal(".github/workflows/ci.yml の test-shards の `go test` を読めなかった")
	}
	return joinContinuedLines(src[idx:])
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// `make test` が CI と同じテスト実行条件で走ることを検査する (#2841)。
//
// **揃っていないと、手元で緑のまま required check の `test` が落ちる。**
// #2795 で `-shuffle` の seed を揃えたのは「揃えないと順序依存が手元で
// 再現しない」からだったが、**同じ理屈が当てはまる `-race` は揃っていなかった**。
// ずれても何も言わないので誰も気付かなかった。`fe7ea8f2` で実際に踏んでいる
// (`TestPluginPeer_SendMeasuresEnvelope` が `make test` では緑、`-race` で落ちた)。
//
// **両方向を見る。** CI の flag は `ciOnlyTestFlags` に理由付きで挙げたもの以外
// `make test` にも同じ値で無ければならず、逆に `make test` の flag も
// `makeOnlyTestFlags` 以外は CI に無ければならない。片方向だと `-short` を
// 足す形が素通りする。
//
// **カバレッジ閾値の step は再現しない。** ここが見るのはテストの実行条件だけで、
// CI にはこの後に per-package のカバレッジ検査がある (CLAUDE.md Section 8)。
func TestMakeTestMatchesCIConditions(t *testing.T) {
	mk, mkArgs := parseTestFlags(makeTestCommand(t))
	ci, _ := parseTestFlags(ciTestCommand(t))
	// **片方が空なら落とす。** 書式が変わって拾えなくなると、検査していないのに
	// 緑になる (compose-check と同じ判断)。
	if len(mk) == 0 || len(ci) == 0 {
		t.Fatalf("flag を読めなかった (make=%v ci=%v)", mk, ci)
	}
	for name, want := range ci {
		if reason := ciOnlyTestFlags[name]; reason != "" {
			continue
		}
		got, ok := mk[name]
		if !ok {
			t.Errorf("`make test` に %s が無い (CI は %s=%q で走る)。\n"+
				"手元で緑のまま required check の `test` が落ちる。Makefile の\n"+
				"`test:` に足すか、挙動に影響しない理由を添えて ciOnlyTestFlags に\n"+
				"登録すること。", name, name, want)
			continue
		}
		if got != want {
			t.Errorf("%s の値が CI と違う (make=%q ci=%q)", name, got, want)
		}
	}
	for name := range mk {
		if _, ok := ci[name]; ok {
			continue
		}
		if reason := makeOnlyTestFlags[name]; reason != "" {
			continue
		}
		t.Errorf("`make test` にだけ %s がある。手元と CI で実行内容が変わる\n"+
			"(例: -short は手元だけ skip する)。外すか、理由を添えて\n"+
			"makeOnlyTestFlags に登録すること。", name)
	}
	// `-timeout` を名前で除外している理由が「既定と同じ値だから」なので、値を見る。
	// **未指定は既定 10m なので一致している。** 空文字と区別しないと、
	// ci.yml から行を消したときに事実に反するメッセージで落ちる。
	if got, ok := ci["-timeout"]; ok && got != goDefaultTimeout {
		t.Errorf("CI の -timeout が Go の既定と違う (ci=%q default=%q)。\n"+
			"`make test` は既定のまま走るので、CI だけ先に打ち切られる。\n"+
			"Makefile の `test:` にも同じ値を渡すこと。", got, goDefaultTimeout)
	}
	if len(mkArgs) != 1 || mkArgs[0] != makeTestPackages {
		t.Errorf("`make test` の対象が %q でない (%v)。CI は go list ./... から\n"+
			"shard を作るので、絞ると手元だけ走らないパッケージができる。", makeTestPackages, mkArgs)
	}
	// **`test-fast` の seed も見る。** CI の seed を変えたとき `make test` は
	// 赤くなるが、`test-fast` は黙って旧 seed のまま残る。反復用に回している
	// 開発者だけが CI と別の並び順を試すことになるので、ここで固定する。
	fast, _ := parseTestFlags(makeTargetTestCommand(t, "test-fast"))
	if got := fast["-shuffle"]; got != ci["-shuffle"] {
		t.Errorf("`make test-fast` の -shuffle が CI と違う (test-fast=%q ci=%q)",
			got, ci["-shuffle"])
	}
	// **`make check` が `test` を指していること。** `-race` 抜きの test-fast が
	// あるので、差し替えられていないかを固定する。
	// **前提リストだけを見る。** `[^\n]*` で行末まで見ると `## fmt → lint → test`
	// のようなヘルプコメントにマッチして、`check: fmt lint test-fast ## ... test ...`
	// が通ってしまう (実測)。`\btest\b` も使えない — `-` が単語境界なので
	// `test-fast` にマッチする。フィールド完全一致なら両方まとめて消える。
	// **全ルールを見る。** GNU make は同一 target の複数ルールの前提を結合するので、
	// `check: fmt lint` と `check: test` に分けても make は test を実行する。
	// 最初の 1 つだけ見ると、正しい書き方で落ちる。
	var prereqs []string
	for _, m := range regexp.MustCompile(`(?m)^check:([^#\n]*)`).FindAllStringSubmatch(readRepoFile(t, "Makefile"), -1) {
		prereqs = append(prereqs, strings.Fields(m[1])...)
	}
	if !slices.Contains(prereqs, "test") {
		t.Error("Makefile の `check:` が `test` に依存していない。" +
			"test-fast は -race が無いので、コミット前の検査にならない")
	}
}

// docsWithShuffleSeed are the docs that must quote the `-shuffle` seed.
//
// **一覧を持つのは「欠落」を見るため。** 値の不一致は全 md の走査で拾えるが、
// あるファイルが `-shuffle` を丸ごと落としても、他が残っていれば件数は 0 に
// ならない。**実際に README.md がその状態だった** — `-race -count=1 -timeout 10m`
// を「CI と同条件」と説明しながら `-shuffle` を持っていなかった (#2841 で修正)。
var docsWithShuffleSeed = []string{
	"CLAUDE.md", "README.md",
	"docs/testing.md", "docs/development.md", "docs/contributing.md", "docs/ci.md",
}

// shuffleSeedPattern matches `-shuffle=3` / `-shuffle 3` / `-shuffle=on`.
//
// **`-shuffle` への隣接を要求する。** 裸の `2795` は issue 番号としても現れるので、
// 隣接を外すと過去の経緯の記述で誤検知する。散文で書きたくなったら
// `-shuffle=3` のインライン表記に直すこと (docs/testing.md がそうしている)。
var shuffleSeedPattern = regexp.MustCompile(`-shuffle[= ]([0-9]+|on|off)`)

// doc に書かれた `-shuffle` の seed が CI と一致しているか検査する (#2841)。
//
// **これは実際に 2 度起きた。** #2795 が seed を `3` にしたあとも
// `docs/testing.md` と CLAUDE.md には `2795` が残り、**唯一の再現コマンドが
// 間違ったまま**だった。落ちたテストを doc の手順で追うと別の並び順を試すので、
// 順序依存が再現せず「flaky」と誤診断される。
//
// #2841 で `-race -count=1` を doc 6 ファイルに書き足したぶん、手で直す箇所が
// 増えている。#2640 が「doc 修正の最多の失敗型は片側更新」と結論した領域なので、
// せめて seed だけは機械で突き合わせる。
func TestDocsQuoteTheCIShuffleSeed(t *testing.T) {
	ci, _ := parseTestFlags(ciTestCommand(t))
	want := ci["-shuffle"]
	if want == "" {
		t.Fatal("ci.yml から -shuffle の seed を読めなかった")
	}
	// (1) 値の不一致は tracked な md 全体で見る。一覧に無いファイルに書かれても拾う。
	out, err := exec.Command("git", "-C", repoRoot(t), "ls-files", "*.md").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	checked := 0
	for _, rel := range strings.Fields(string(out)) {
		// 本家から取り込んだ frontend/ (#3379) は、こちらの CI の条件を書く場所ではない。
		if strings.HasPrefix(rel, "frontend/") {
			continue
		}
		for _, m := range shuffleSeedPattern.FindAllStringSubmatch(readRepoFile(t, rel), -1) {
			checked++
			if m[1] != want {
				t.Errorf("%s に %q がある (CI は -shuffle=%s)。\n"+
					"doc の再現コマンドが CI と別の並び順を試すので、順序依存の\n"+
					"失敗が手元で再現せず flaky と誤診断される。", rel, m[0], want)
			}
		}
	}
	// **1 つも見つからなかったら落とす。** 書式が変わって拾えなくなると、
	// 検査していないのに緑になる。
	if checked == 0 {
		t.Fatal("doc から -shuffle の記述を 1 つも読めなかった")
	}
	// (2) 欠落は名指しの一覧で見る。合計件数だと 1 ファイルが失っても気付けない。
	for _, rel := range docsWithShuffleSeed {
		if !shuffleSeedPattern.MatchString(readRepoFile(t, rel)) {
			t.Errorf("%s が -shuffle の seed を書いていない。\n"+
				"テスト条件を説明しているのに seed を落とすと、読んだ人が\n"+
				"CI と別の並び順を試すことになる。", rel)
		}
	}
}
