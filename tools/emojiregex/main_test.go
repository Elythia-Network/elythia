package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generatedPath returns the emoji pattern file of the Go MFM parser that the
// build list of this module resolves to.
func generatedPath(t *testing.T) string {
	t.Helper()
	p, err := goModuleRegexPath()
	require.NoError(t, err)
	return p
}

// copyGenerated copies the Go module's emoji pattern file to a writable path.
func copyGenerated(t *testing.T, dst string) {
	t.Helper()
	b, err := os.ReadFile(generatedPath(t))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, b, 0o644))
}

func readSnapshot(t *testing.T) regexSource {
	t.Helper()
	b, err := os.ReadFile("testdata/source.txt")
	require.NoError(t, err)
	src, err := parseSnapshot(b)
	require.NoError(t, err)
	return src
}

// TestGeneratedFileIsUpToDate converts the committed snapshot of the frontend's
// emoji regex and requires the Go module's pattern file to be byte-identical.
//
// Go のモジュールだけを上げて、frontend の @elythia-network/mfm (snapshot) が古いままのときにここで落ちる。
// frontend/ の node_modules が無くても回るように、元の正規表現は snapshot から読む。
// snapshot 自体 (正規表現と mfm-js / emoji-data の版) が frontend/ の node_modules (@elythia-network/mfm) と一致しているかは `make emoji-regex-check` (CI では frontend workflow の frontend-lint、手元では make frontend-check から呼ぶ) で見る。
func TestGeneratedFileIsUpToDate(t *testing.T) {
	src := readSnapshot(t)
	// 抽出が空振りして空の snapshot から空の生成物を作っても緑にならないよう、
	// 実在する選択肢を名指しで要求する (U+1F468 U+1F3FB の並びで始まるキスの絵文字)
	require.True(t, strings.HasPrefix(src.source, `(?:\ud83d\udc68\ud83c\udffb\u200d\u2764\ufe0f\u200d\ud83d\udc8b`), "snapshot does not start with the known first alternative")
	want, err := generate(src)
	require.NoError(t, err)
	got, err := os.ReadFile(generatedPath(t))
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got), "%s differs from the snapshot; update the Go module and the frontend's @elythia-network/mfm to the same release", generatedPath(t))
	assert.Contains(t, string(got), `const emojiDataVersion = "`+src.version+`"`)
	assert.Contains(t, string(got), `const emojiMfmJsVersion = "`+src.mfmjs+`"`)
}

func TestSnapshotRoundTrip(t *testing.T) {
	src := regexSource{mfmjs: "0.1.0", version: "1.2.3", source: `a|b`}
	got, err := parseSnapshot(src.snapshot())
	require.NoError(t, err)
	assert.Equal(t, src, got)
	for _, bad := range []string{"", "@misskey-dev/emoji-data 1\nabc", "other 1\nabc\n", "@misskey-dev/emoji-data 1\nabc\n", "mfm-js 1\nother 1\nabc\n", "mfm-js 1\n@misskey-dev/emoji-data 1\nabc"} {
		_, err := parseSnapshot([]byte(bad))
		assert.Error(t, err, bad)
	}
}

func TestExtractSource(t *testing.T) {
	// 生成された側は、正規表現の中の \ を \\ に、© などの文字を \u00a9 の形に書く
	got, err := extractSource([]byte(fakeRegexJS("0.26.0", "17.0.0", `a|\\ud83d\\ude00|[\u00a9\u00ae\\u2122]`, "y")))
	require.NoError(t, err)
	assert.Equal(t, regexSource{mfmjs: "0.26.0", version: "17.0.0", source: `a|\ud83d\ude00|[©®\u2122]`}, got)
	valid := fakeRegexJS("0.26.0", "17.0.0", `a`, "y")
	for name, js := range map[string]string{
		"empty":                "",
		"no emojiDataVersion":  strings.Replace(valid, "emojiDataVersion", "x", 1),
		"no emojiMfmJsVersion": strings.Replace(valid, "emojiMfmJsVersion", "x", 1),
		"no emojiRegex":        strings.Replace(valid, "emojiRegex", "x", 1),
		"two emojiRegex":       valid + "export const emojiRegex = new RegExp('b', 'y');\n",
		"u flag":               fakeRegexJS("0.26.0", "17.0.0", `a`, "uy"),
		"not sticky":           fakeRegexJS("0.26.0", "17.0.0", `a`, ""),
		// \\ と \uXXXX 以外の逃がし文字は読み違えるので受け付けない
		"hex escape":       fakeRegexJS("0.26.0", "17.0.0", `\x41`, "y"),
		"quote escape":     fakeRegexJS("0.26.0", "17.0.0", `\'`, "y"),
		"short escape":     fakeRegexJS("0.26.0", "17.0.0", `\u00a`, "y"),
		"surrogate escape": fakeRegexJS("0.26.0", "17.0.0", `\ud83d`, "y"),
	} {
		_, err := extractSource([]byte(js))
		assert.Error(t, err, name)
	}
}

// TestConvert_Surrogates checks the translation from UTF-16 code units to code
// points on a small regex that has every construct emoji-data uses.
func TestConvert_Surrogates(t *testing.T) {
	alt, err := parseJS(`(?:\ud83d\ude00|\ud83c[\udffb-\udfff\udc04])|[#*0-9]\ufe0f?\u20e3|(?:[\u00a9\u00ae\u2122]\ufe0f)|(\u263a)(?:\ufe0f|(?!\ufe0e))|\ufe0f`)
	require.NoError(t, err)
	require.NoError(t, pairSurrogates(alt))
	var holds, fails strings.Builder
	alt.renderRE2(&holds, lookaheadHolds)
	alt.renderRE2(&fails, lookaheadFails)
	assert.Equal(t, `(?:\x{1F600}|[\x{1F3FB}-\x{1F3FF}\x{1F004}])|[\x{23}\x{2A}0-9]\x{FE0F}?\x{20E3}|(?:[\x{A9}\x{AE}\x{2122}]\x{FE0F})|(?:\x{263A})(?:\x{FE0F}|)|\x{FE0F}`, holds.String())
	assert.Equal(t, `(?:\x{1F600}|[\x{1F3FB}-\x{1F3FF}\x{1F004}])|[\x{23}\x{2A}0-9]\x{FE0F}?\x{20E3}|(?:[\x{A9}\x{AE}\x{2122}]\x{FE0F})|(?:\x{263A})(?:\x{FE0F}|[^\x00-\x{10FFFF}])|\x{FE0F}`, fails.String())

	la, err := findLookahead(alt)
	require.NoError(t, err)
	assert.Equal(t, lookahead{found: true, offset: 1, char: 0xfe0e}, la)
	assert.Equal(t, []charRange{{'#', '#'}, {'*', '*'}, {'0', '9'}, {0xa9, 0xa9}, {0xae, 0xae}, {0x2122, 0x2122}, {0x263a, 0x263a}, {0xfe0f, 0xfe0f}, {0x1f004, 0x1f004}, {0x1f3fb, 0x1f3ff}, {0x1f600, 0x1f600}}, alt.firstRunes())
	assert.Equal(t, 7, alt.maxBytes()) // '#' + U+FE0F + U+20E3
	minW, maxW := alt.width()
	assert.Equal(t, 1, minW)
	assert.Equal(t, 3, maxW)

	// 一致の最大バイト数は範囲の上端の長さで数える
	wide, err := parseJS(`[a-z]|[a-\u3042]`)
	require.NoError(t, err)
	assert.Equal(t, 3, wide.maxBytes())

	// 先読みが無ければ found は false
	noLA, err := parseJS(`a|b`)
	require.NoError(t, err)
	la, err = findLookahead(noLA)
	require.NoError(t, err)
	assert.False(t, la.found)
}

// TestConvert_MatchesLikeJavaScript runs the rendered pattern on inputs whose
// results under the JavaScript regex are known.
func TestConvert_MatchesLikeJavaScript(t *testing.T) {
	alt, err := parseJS(`\ud83d\udc68(?:\ud83c[\udffb-\udfff])?\u200d\ud83d\udcbb|\ud83d\udc68|[\u263a\u270c](?:\ufe0f|(?!\ufe0e))(?:\ud83c[\udffb-\udfff])?`)
	require.NoError(t, err)
	require.NoError(t, pairSurrogates(alt))
	render := func(mode lookaheadMode) *regexp.Regexp {
		var b strings.Builder
		b.WriteString("^(?:")
		alt.renderRE2(&b, mode)
		b.WriteString(")")
		return regexp.MustCompile(b.String())
	}
	holds, fails := render(lookaheadHolds), render(lookaheadFails)
	match := func(s string) string {
		re := holds
		if r := []rune(s); len(r) > 1 && r[1] == 0xfe0e {
			re = fails
		}
		return re.FindString(s)
	}
	cases := map[string]string{
		"\U0001F468\U0001F3FB\u200d\U0001F4BBx": "\U0001F468\U0001F3FB\u200d\U0001F4BB",
		"\U0001F468\u200d\U0001F4BB":            "\U0001F468\u200d\U0001F4BB",
		"\U0001F468\U0001F3FB":                  "\U0001F468", // 肌の色だけの並びは無い
		"\u263a":                                "\u263a",
		"\u263a\ufe0f\U0001F3FB":                "\u263a\ufe0f\U0001F3FB",
		"\u263a\U0001F3FB":                      "\u263a\U0001F3FB",
		"\u263a\ufe0e":                          "",
		"\u270c\ufe0e\U0001F3FB":                "",
	}
	for in, want := range cases {
		assert.Equal(t, want, match(in), "%U", []rune(in))
	}
}

func TestConvert_RejectsUnsupportedSyntax(t *testing.T) {
	for _, src := range []string{
		`a*`, `a+`, `a{2}`, `a??`, `a?*`, `.`, `^a`, `a$`, `]`, `(?=a)`, `(?<a>b)`, `[^a]`, `[a`, `(a`, `a)`,
		`\d`, `\u12`, `\uzzzz`, `[\d]`, `[a-]`, `[b-a]`, `[]`, `[[]`, `[a-\d]`, `(?!ab)`, `(?!a?)`, `(?![a])`, `(?!a)?`,
	} {
		alt, err := parseJS(src)
		assert.Error(t, err, "%s parsed as %v", src, alt)
	}
	_, err := parseJS("\xff")
	assert.Error(t, err)
}

func TestConvert_RejectsUnpairedSurrogates(t *testing.T) {
	for _, src := range []string{
		`\ud83d`,                // 上位だけ
		`\ude00`,                // 下位だけ
		`\ud83d?\ude00`,         // 上位が任意
		`\ud83d\ude00?`,         // 下位が任意 (上位だけでも一致する)
		`\ud83da`,               // 下位でない文字が続く
		`\ud83d[\ude00a]`,       // クラスに下位でない文字がある
		`\ud83d(?:\ude00)`,      // グループが続く
		`[\ud83d\ude00]`,        // クラスの中のペア
		`(?:\ud83d)\ude00`,      // グループの中で切れている
		`a(?!\ud83d)`,           // サロゲートの先読み
		`\ud83d[\udfff-\ue000]`, // 範囲が下位の外へはみ出す
	} {
		alt, err := parseJS(src)
		require.NoError(t, err, src)
		assert.Error(t, pairSurrogates(alt), src)
	}
	// 直接書かれたアストラル文字は、ペアに分けてから同じ規則で読む
	alt, err := parseJS("\U0001F600|[#]")
	require.NoError(t, err)
	require.NoError(t, pairSurrogates(alt))
	assert.Equal(t, []charRange{{'#', '#'}, {0x1f600, 0x1f600}}, alt.firstRunes())
}

func TestFindLookahead_RejectsWhatTwoPatternsCannotExpress(t *testing.T) {
	for _, src := range []string{
		`(?:a|ab)(?!x)`,       // 先頭からの位置が経路で変わる
		`a?(?!x)`,             // 同上
		`a(?!x)|b(?!y)`,       // 見る文字が違う
		`a(?!x)|bc(?!x)`,      // 見る位置が違う
		`(?:a(?!x)|ab(?!x))b`, // 入れ子でも位置が違う
	} {
		alt, err := parseJS(src)
		require.NoError(t, err, src)
		require.NoError(t, pairSurrogates(alt), src)
		_, err = findLookahead(alt)
		assert.Error(t, err, src)
	}
}

func TestGenerate_RejectsBrokenSources(t *testing.T) {
	for _, src := range []string{
		`a?`,             // 空文字列に一致する
		`a|b|c`,          // 選択肢が少なすぎる (抽出の空振り)
		`a*`,             // 解析できない
		`\ud83d`,         // ペアにならない
		`a(?!x)|bc(?!y)`, // 先読みを 2 つの正規表現で表せない
	} {
		_, err := generate(regexSource{version: "0", source: src})
		assert.Error(t, err, src)
	}
}

// fakeRegexJS renders built/emoji-regex.gen.js of @elythia-network/mfm, with
// literal as the content of the regex's string literal.
func fakeRegexJS(mfmjsVersion, emojiDataVersion, literal, flags string) string {
	return "// Code generated by tools/emojiregex from @misskey-dev/emoji-data " + emojiDataVersion + "; DO NOT EDIT.\n" +
		"export const emojiDataVersion = '" + emojiDataVersion + "';\n" +
		"export const emojiMfmJsVersion = '" + mfmjsVersion + "';\n" +
		"export const emojiRegex = new RegExp('" + literal + "', '" + flags + "');\n" +
		"export const emojiFirstUnits = [\n    0x0023, 0x0023,\n];\n"
}

// fakeMisskey lays out the files loadSource reads, the way pnpm installs them,
// with source as the regex source (escaped into the string literal).
func fakeMisskey(t *testing.T, name, mfmjsVersion, emojiDataVersion, source, flags string) string {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "node_modules", ".pnpm", "@elythia-network+mfm@0.1.0", "node_modules", "@elythia-network", "mfm")
	write := func(path, content string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	write(filepath.Join(pkg, "package.json"), `{"name": "`+name+`", "version": "0.1.0"}`)
	write(filepath.Join(pkg, "built", "emoji-regex.gen.js"), fakeRegexJS(mfmjsVersion, emojiDataVersion, strings.ReplaceAll(source, `\`, `\\`), flags))
	link := filepath.Join(root, "packages", "frontend", "node_modules", "@elythia-network", "mfm")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(pkg, link))
	return root
}

func TestRun_WritesThenChecks(t *testing.T) {
	src := readSnapshot(t)
	root := fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, src.source, "y")
	dir := t.TempDir()
	out := filepath.Join(dir, "gen.go")
	copyGenerated(t, out)
	snap := filepath.Join(dir, "source.txt")
	args := []string{"-misskey", root, "-out", out, "-snapshot", snap}

	var stdout, stderr bytes.Buffer
	require.NoError(t, run(args, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "wrote")
	gotSnap, err := os.ReadFile(snap)
	require.NoError(t, err)
	assert.Equal(t, src.snapshot(), gotSnap)

	stdout.Reset()
	require.NoError(t, run(append(args, "-check"), &stdout, &stderr))
	assert.Contains(t, stdout.String(), "up to date")

	// Go のモジュールか snapshot のどちらかがずれていたら落とす
	for _, path := range []string{out, snap} {
		orig, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(orig, '\n'), 0o644))
		err = run(append(args, "-check"), &stdout, &stderr)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale")
		require.NoError(t, os.WriteFile(path, orig, 0o644))
	}
	// 写す mfm-js の版か emoji-data の版だけが変わっても (正規表現が同じでも) 落とす
	for _, bumped := range []string{
		fakeMisskey(t, frontendMfmPackage, "0.99.0", src.version, src.source, "y"),
		fakeMisskey(t, frontendMfmPackage, src.mfmjs, "99.0.0", src.source, "y"),
	} {
		err = run([]string{"-misskey", bumped, "-out", out, "-snapshot", snap, "-check"}, &stdout, &stderr)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale")
	}
	// 正規表現が変わったら落とす
	changed := fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, strings.Replace(src.source, `\ufe0f`, `\ufe0e`, 1), "y")
	err = run([]string{"-misskey", changed, "-out", out, "-snapshot", snap, "-check"}, &stdout, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale")

	// Go のモジュールが追いついていなければ、check でなくても snapshot を書かずに落とす
	err = run([]string{"-misskey", changed, "-out", out, "-snapshot", snap}, &stdout, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale")
	gotSnap, err = os.ReadFile(snap)
	require.NoError(t, err)
	assert.Equal(t, src.snapshot(), gotSnap, "the snapshot must not follow a frontend the Go module does not match")

	require.NoError(t, os.Remove(snap))
	assert.Error(t, run(append(args, "-check"), &stdout, &stderr))
}

// TestRun_DefaultsToTheGoModule checks that without -out the command compares
// against the Go module that this module's build list resolves to.
func TestRun_DefaultsToTheGoModule(t *testing.T) {
	src := readSnapshot(t)
	root := fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, src.source, "y")
	snap := filepath.Join(t.TempDir(), "source.txt")
	require.NoError(t, os.WriteFile(snap, src.snapshot(), 0o644))
	var stdout, stderr bytes.Buffer
	require.NoError(t, run([]string{"-misskey", root, "-snapshot", snap, "-check"}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "up to date")
	assert.True(t, strings.HasSuffix(filepath.ToSlash(generatedPath(t)), "/"+goModuleRegex))
	assert.Contains(t, filepath.ToSlash(generatedPath(t)), "github.com/elythia-network/mfm@")

	changed := fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, strings.Replace(src.source, `\ufe0f`, `\ufe0e`, 1), "y")
	assert.Error(t, run([]string{"-misskey", changed, "-snapshot", snap, "-check"}, &stdout, &stderr))
}

func TestRun_FailsOnBrokenInstall(t *testing.T) {
	src := readSnapshot(t)
	dir := t.TempDir()
	out := []string{"-out", filepath.Join(dir, "gen.go"), "-snapshot", filepath.Join(dir, "source.txt")}
	cases := map[string]string{
		"not installed":       t.TempDir(),
		"other package":       fakeMisskey(t, "mfm-js", src.mfmjs, src.version, src.source, "y"),
		"u flag":              fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, src.source, "uy"),
		"unconvertible regex": fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, `a*`, "y"),
	}
	for name, root := range cases {
		var stdout, stderr bytes.Buffer
		assert.Error(t, run(append([]string{"-misskey", root}, out...), &stdout, &stderr), name)
	}
	var stdout, stderr bytes.Buffer
	assert.Error(t, run([]string{"-unknown"}, &stdout, &stderr))
	assert.Error(t, run([]string{"extra"}, &stdout, &stderr))

	// Go のモジュールの生成物が読めない / snapshot の書き込み先が無い
	root := fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, src.source, "y")
	assert.Error(t, run([]string{"-misskey", root, "-out", filepath.Join(dir, "missing", "gen.go")}, &stdout, &stderr))
	gen := filepath.Join(dir, "gen.go")
	copyGenerated(t, gen)
	assert.Error(t, run([]string{"-misskey", root, "-out", gen, "-snapshot", filepath.Join(dir, "missing", "s.txt")}, &stdout, &stderr))

	// package.json が JSON でない / 正規表現のファイルが無い
	for _, file := range []string{"package.json", "built/emoji-regex.gen.js"} {
		broken := fakeMisskey(t, frontendMfmPackage, src.mfmjs, src.version, src.source, "y")
		link, err := filepath.EvalSymlinks(filepath.Join(broken, "packages", "frontend", "node_modules", "@elythia-network", "mfm"))
		require.NoError(t, err)
		if file == "package.json" {
			require.NoError(t, os.WriteFile(filepath.Join(link, file), []byte("{"), 0o644))
		} else {
			require.NoError(t, os.Remove(filepath.Join(link, filepath.FromSlash(file))))
		}
		assert.Error(t, run(append([]string{"-misskey", broken}, out...), &stdout, &stderr), file)
	}
}
