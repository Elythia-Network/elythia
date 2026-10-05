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

const generatedPath = "../../internal/activitypub/mfm/emoji_regex_gen.go"

func readSnapshot(t *testing.T) regexSource {
	t.Helper()
	b, err := os.ReadFile("testdata/source.txt")
	require.NoError(t, err)
	src, err := parseSnapshot(b)
	require.NoError(t, err)
	return src
}

// TestGeneratedFileIsUpToDate regenerates the Go file from the committed
// snapshot of the emoji-data regex and requires it to be byte-identical.
//
// 生成物を手で直した、あるいは生成ツールを直して作り直し忘れたときにここで落ちる。
// submodule の node_modules が無くても回るように、元の正規表現は snapshot から読む。
// snapshot 自体 (正規表現と mfm-js / emoji-data の版) が frontend/ の node_modules と一致しているかは `make emoji-regex-check` (CI では frontend workflow の frontend-lint、手元では make frontend-check から呼ぶ) で見る。
func TestGeneratedFileIsUpToDate(t *testing.T) {
	src := readSnapshot(t)
	// 抽出が空振りして空の snapshot から空の生成物を作っても緑にならないよう、
	// 実在する選択肢を名指しで要求する (U+1F468 U+1F3FB の並びで始まるキスの絵文字)
	require.True(t, strings.HasPrefix(src.source, `(?:\ud83d\udc68\ud83c\udffb\u200d\u2764\ufe0f\u200d\ud83d\udc8b`), "snapshot does not start with the known first alternative")
	want, err := generate(src)
	require.NoError(t, err)
	got, err := os.ReadFile(generatedPath)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got), "%s is stale; run `make emoji-regex`", generatedPath)
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

func TestExtractRegex(t *testing.T) {
	got, err := extractRegex([]byte("//#region\nconst emojiRegex = /a|\\ud83d\\ude00/g;\n//#endregion\n"))
	require.NoError(t, err)
	assert.Equal(t, `a|\ud83d\ude00`, got)
	for _, js := range []string{"", "export const x = 1;\n", "const emojiRegex = /a/g;\nconst emojiRegex = /b/g;\n"} {
		_, err := extractRegex([]byte(js))
		assert.Error(t, err, js)
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

// fakeMisskey lays out the files loadSource reads, the way pnpm installs them.
func fakeMisskey(t *testing.T, mfmjsVersion, source, emojiDataWant, emojiDataHave, mfmjsBundle string) string {
	t.Helper()
	root := t.TempDir()
	mfmjs := filepath.Join(root, "node_modules", ".pnpm", "mfm-js@0.26.0", "node_modules", "mfm-js")
	data := filepath.Join(root, "node_modules", ".pnpm", "mfm-js@0.26.0", "node_modules", "@misskey-dev", "emoji-data")
	write := func(path, content string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}
	deps := `{}`
	if emojiDataWant != "" {
		deps = `{"@misskey-dev/emoji-data": "` + emojiDataWant + `"}`
	}
	write(filepath.Join(mfmjs, "package.json"), `{"version": "`+mfmjsVersion+`", "dependencies": `+deps+`}`)
	write(filepath.Join(mfmjs, "built", "index.mjs"), mfmjsBundle)
	write(filepath.Join(data, "package.json"), `{"version": "`+emojiDataHave+`"}`)
	write(filepath.Join(data, "built", "regex.mjs"), "//#region src/regex/index.ts\nconst emojiRegex = /"+source+"/g;\n//#endregion\nexport { emojiRegex };\n")
	link := filepath.Join(root, "packages", "frontend", "node_modules", "mfm-js")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(mfmjs, link))
	return root
}

// mfmjsBundleExcerpt is the part of mfm-js 0.26.0's built/index.mjs the
// generator requires.
const mfmjsBundleExcerpt = "function regexp(pattern) {\n\tconst re = RegExp(`^(?:${pattern.source})`, pattern.flags);\n}\n" +
	"\tunicodeEmoji: () => {\n\t\treturn " + mfmjsUnicodeEmoji + "\n\t\t});\n\t},\n"

func TestRun_WritesThenChecks(t *testing.T) {
	src := readSnapshot(t)
	root := fakeMisskey(t, src.mfmjs, src.source, src.version, src.version, mfmjsBundleExcerpt)
	dir := t.TempDir()
	out := filepath.Join(dir, "gen.go")
	snap := filepath.Join(dir, "source.txt")
	args := []string{"-misskey", root, "-out", out, "-snapshot", snap}

	var stdout, stderr bytes.Buffer
	require.NoError(t, run(args, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "wrote")
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	want, err := os.ReadFile(generatedPath)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got), "generating from the installed package must give the committed file")
	gotSnap, err := os.ReadFile(snap)
	require.NoError(t, err)
	assert.Equal(t, src.snapshot(), gotSnap)

	stdout.Reset()
	require.NoError(t, run(append(args, "-check"), &stdout, &stderr))
	assert.Contains(t, stdout.String(), "up to date")

	// 生成物か snapshot のどちらかがずれていたら落とす
	for _, path := range []string{out, snap} {
		orig, err := os.ReadFile(path)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(orig, '\n'), 0o644))
		err = run(append(args, "-check"), &stdout, &stderr)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "stale")
		require.NoError(t, os.WriteFile(path, orig, 0o644))
	}
	// mfm-js だけを上げても (正規表現が同じでも) 落とす
	bumped := fakeMisskey(t, "0.99.0", src.source, src.version, src.version, mfmjsBundleExcerpt)
	err = run([]string{"-misskey", bumped, "-out", out, "-snapshot", snap, "-check"}, &stdout, &stderr)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stale")

	require.NoError(t, os.Remove(snap))
	assert.Error(t, run(append(args, "-check"), &stdout, &stderr))
}

func TestRun_FailsOnBrokenInstall(t *testing.T) {
	src := readSnapshot(t)
	dir := t.TempDir()
	out := []string{"-out", filepath.Join(dir, "gen.go"), "-snapshot", filepath.Join(dir, "source.txt")}
	cases := map[string]string{
		"no mfm-js":            t.TempDir(),
		"no emoji-data dep":    fakeMisskey(t, src.mfmjs, src.source, "", src.version, mfmjsBundleExcerpt),
		"version mismatch":     fakeMisskey(t, src.mfmjs, src.source, src.version, "0.0.1", mfmjsBundleExcerpt),
		"unicodeEmoji changed": fakeMisskey(t, src.mfmjs, src.source, src.version, src.version, "regexp(emojiRegex)"),
		"unconvertible regex":  fakeMisskey(t, src.mfmjs, `a*`, src.version, src.version, mfmjsBundleExcerpt),
	}
	for name, root := range cases {
		var stdout, stderr bytes.Buffer
		assert.Error(t, run(append([]string{"-misskey", root}, out...), &stdout, &stderr), name)
	}
	var stdout, stderr bytes.Buffer
	assert.Error(t, run([]string{"-unknown"}, &stdout, &stderr))
	assert.Error(t, run([]string{"extra"}, &stdout, &stderr))

	// 書き込めない出力先
	root := fakeMisskey(t, src.mfmjs, src.source, src.version, src.version, mfmjsBundleExcerpt)
	assert.Error(t, run([]string{"-misskey", root, "-out", filepath.Join(dir, "missing", "gen.go")}, &stdout, &stderr))
	assert.Error(t, run([]string{"-misskey", root, "-out", filepath.Join(dir, "gen.go"), "-snapshot", filepath.Join(dir, "missing", "s.txt")}, &stdout, &stderr))

	// package.json が JSON でない
	broken := fakeMisskey(t, src.mfmjs, src.source, src.version, src.version, mfmjsBundleExcerpt)
	link, err := filepath.EvalSymlinks(filepath.Join(broken, "packages", "frontend", "node_modules", "mfm-js"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(link, "package.json"), []byte("{"), 0o644))
	assert.Error(t, run(append([]string{"-misskey", broken}, out...), &stdout, &stderr))
}
