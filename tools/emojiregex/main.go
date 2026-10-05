// Command emojiregex generates the Unicode emoji pattern of the MFM parser
// (internal/activitypub/mfm/emoji_regex_gen.go) from the emoji regex of
// @misskey-dev/emoji-data, the one mfm-js uses for its unicodeEmoji parser.
//
// The JavaScript regex works on UTF-16 code units; the generated pattern is the
// same regex over code points for Go's regexp package. Both pick the first
// matching alternative from the left, so they read the same emoji.
//
// Usage:
//
//	go run ./tools/emojiregex            # regenerate from frontend/
//	go run ./tools/emojiregex -check     # fail when the outputs are stale
//
// The regex source is also written to a snapshot file so that the tests of
// this package can check the generated file without frontend/node_modules.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"regexp/syntax"
	"strings"
)

const (
	defaultMisskeyDir = "frontend"
	defaultOut        = "internal/activitypub/mfm/emoji_regex_gen.go"
	defaultSnapshot   = "tools/emojiregex/testdata/source.txt"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "emojiregex:", err)
		os.Exit(1)
	}
}

func run(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("emojiregex", flag.ContinueOnError)
	fs.SetOutput(errOut)
	misskeyDir := fs.String("misskey", defaultMisskeyDir, "checkout of the Misskey fork with node_modules installed")
	outPath := fs.String("out", defaultOut, "generated Go file")
	snapshotPath := fs.String("snapshot", defaultSnapshot, "snapshot of the regex source")
	check := fs.Bool("check", false, "compare instead of writing; fail when the outputs are stale")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}

	src, err := loadSource(*misskeyDir)
	if err != nil {
		return err
	}
	gen, err := generate(src)
	if err != nil {
		return err
	}
	snap := src.snapshot()
	if *check {
		for _, f := range []struct {
			path string
			want []byte
		}{{*outPath, gen}, {*snapshotPath, snap}} {
			got, err := os.ReadFile(f.path)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, f.want) {
				return fmt.Errorf("%s is stale; run `make emoji-regex`", f.path)
			}
		}
		fmt.Fprintf(out, "emoji regex is up to date (mfm-js %s, emoji-data %s)\n", src.mfmjs, src.version)
		return nil
	}
	if err := os.WriteFile(*outPath, gen, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(*snapshotPath, snap, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote %s and %s (mfm-js %s, emoji-data %s)\n", *outPath, *snapshotPath, src.mfmjs, src.version)
	return nil
}

// regexSource is the emoji regex of one emoji-data version, as used by one
// mfm-js version.
type regexSource struct {
	mfmjs   string // mfm-js version
	version string // @misskey-dev/emoji-data version
	source  string
}

const (
	snapshotMfmJsHeader = "mfm-js "
	snapshotHeader      = "@misskey-dev/emoji-data "
)

// mfm-js の版も snapshot に残す。正規表現が同じでも、mfm-js を上げたら
// unicodeEmoji の使い方と期待値 (testdata/emoji_mfmjs.json) を確かめ直す必要が
// あるので、版が変わったら -check を落とす。
func (s regexSource) snapshot() []byte {
	return []byte(snapshotMfmJsHeader + s.mfmjs + "\n" + snapshotHeader + s.version + "\n" + s.source + "\n")
}

func parseSnapshot(b []byte) (regexSource, error) {
	lines := strings.SplitN(string(b), "\n", 3)
	if len(lines) != 3 || !strings.HasPrefix(lines[0], snapshotMfmJsHeader) || !strings.HasPrefix(lines[1], snapshotHeader) || !strings.HasSuffix(lines[2], "\n") {
		return regexSource{}, errors.New("malformed snapshot")
	}
	return regexSource{
		mfmjs:   strings.TrimPrefix(lines[0], snapshotMfmJsHeader),
		version: strings.TrimPrefix(lines[1], snapshotHeader),
		source:  strings.TrimSuffix(lines[2], "\n"),
	}, nil
}

// mfm-js の unicodeEmoji の実装。正規表現の使い方 (フラグを落として今の位置から
// 読む) と、U+FE0F だけのときに文字として返すことを、この形で前提にしている。
// mfm-js を上げてこの形が変わったら、生成した判定が mfm-js と揃っているかを
// 確かめ直す必要があるので、見つからなければ落とす。
const mfmjsUnicodeEmoji = `regexp(RegExp(emojiRegex.source)).map((content) => {
			return content === "` + "\ufe0f" + `" ? content : UNI_EMOJI(content);`

var (
	emojiRegexDecl = regexp.MustCompile(`(?m)^const emojiRegex = /(.+)/([a-z]*);$`)
	// mfm-js の regexp() コンビネータは ^(?:...) で包んで今の位置に固定する。
	mfmjsRegexpCombinator = "RegExp(`^(?:${pattern.source})`, pattern.flags)"
)

// loadSource reads the emoji regex that the frontend's mfm-js depends on.
//
// 版は書き写さず、frontend が使う mfm-js から辿る。pnpm は依存を
// `.pnpm/<pkg>@<ver>/node_modules/` に兄弟として並べるので、mfm-js の実体の隣に
// mfm-js 自身が依存する版の emoji-data がある。
func loadSource(misskeyDir string) (regexSource, error) {
	link := filepath.Join(misskeyDir, "packages", "frontend", "node_modules", "mfm-js")
	mfmjsDir, err := filepath.EvalSymlinks(link)
	if err != nil {
		return regexSource{}, fmt.Errorf("mfm-js is not installed (run pnpm install in %s): %w", misskeyDir, err)
	}
	var mfmjs struct {
		Version      string            `json:"version"`
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := readJSON(filepath.Join(mfmjsDir, "package.json"), &mfmjs); err != nil {
		return regexSource{}, err
	}
	want := mfmjs.Dependencies["@misskey-dev/emoji-data"]
	if want == "" {
		return regexSource{}, fmt.Errorf("mfm-js %s does not depend on @misskey-dev/emoji-data", mfmjs.Version)
	}
	bundle, err := os.ReadFile(filepath.Join(mfmjsDir, "built", "index.mjs"))
	if err != nil {
		return regexSource{}, err
	}
	for _, needle := range []string{mfmjsUnicodeEmoji, mfmjsRegexpCombinator} {
		if !bytes.Contains(bundle, []byte(needle)) {
			return regexSource{}, fmt.Errorf("mfm-js %s no longer contains %q; check that its unicodeEmoji still works the way this generator assumes", mfmjs.Version, needle)
		}
	}

	dataDir := filepath.Join(filepath.Dir(mfmjsDir), "@misskey-dev", "emoji-data")
	var data struct {
		Version string `json:"version"`
	}
	if err := readJSON(filepath.Join(dataDir, "package.json"), &data); err != nil {
		return regexSource{}, err
	}
	if data.Version != want {
		return regexSource{}, fmt.Errorf("mfm-js wants emoji-data %s but %s is installed", want, data.Version)
	}
	js, err := os.ReadFile(filepath.Join(dataDir, "built", "regex.mjs"))
	if err != nil {
		return regexSource{}, err
	}
	src, err := extractRegex(js)
	if err != nil {
		return regexSource{}, err
	}
	return regexSource{mfmjs: mfmjs.Version, version: data.Version, source: src}, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// extractRegex returns the source of the single `const emojiRegex = /.../;`.
func extractRegex(js []byte) (string, error) {
	m := emojiRegexDecl.FindAllSubmatch(js, -1)
	if len(m) != 1 {
		return "", fmt.Errorf("found %d emojiRegex declarations, want 1", len(m))
	}
	return string(m[0][1]), nil
}

// generate converts the regex and renders the Go file.
func generate(src regexSource) ([]byte, error) {
	alt, err := parseJS(src.source)
	if err != nil {
		return nil, err
	}
	if err := pairSurrogates(alt); err != nil {
		return nil, err
	}
	minW, _ := alt.width()
	if minW == 0 {
		return nil, errors.New("the regex can match the empty string")
	}
	// 抽出が空振りした・書式が変わったなどで絵文字をほとんど持たない正規表現を、
	// 生成物として通さない。下限は 17.0.0 の選択肢の数 (267) より小さく取る。
	if n := alt.countAlternatives(); n < 200 {
		return nil, fmt.Errorf("the regex has only %d alternatives; extraction probably went wrong", n)
	}
	la, err := findLookahead(alt)
	if err != nil {
		return nil, err
	}

	var holds, fails strings.Builder
	holds.WriteString("^(?:")
	alt.renderRE2(&holds, lookaheadHolds)
	holds.WriteString(")")
	fails.WriteString("^(?:")
	alt.renderRE2(&fails, lookaheadFails)
	fails.WriteString(")")
	for _, p := range []string{holds.String(), fails.String()} {
		// Perl 構文 (\x{...}) で通ることを、実行時と同じ regexp.Compile で確かめる
		if _, err := regexp.Compile(p); err != nil {
			return nil, err
		}
		if _, err := syntax.Parse(p, syntax.Perl); err != nil {
			return nil, err
		}
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "// Code generated by tools/emojiregex from @misskey-dev/emoji-data %s; DO NOT EDIT.\n\n", src.version)
	b.WriteString("package mfm\n\n")
	b.WriteString("import \"unicode\"\n\n")
	fmt.Fprintf(&b, "// emojiDataVersion is the @misskey-dev/emoji-data version the patterns were generated from.\n")
	fmt.Fprintf(&b, "const emojiDataVersion = %q\n\n", src.version)
	b.WriteString("// emojiMfmJsVersion is the mfm-js version whose unicodeEmoji the patterns reproduce.\n")
	fmt.Fprintf(&b, "const emojiMfmJsVersion = %q\n\n", src.mfmjs)
	b.WriteString("// emojiPatternLookaheadHolds is the emoji regex of mfm-js with its negative\n")
	b.WriteString("// lookaheads replaced by the empty string, for inputs where they succeed.\n")
	fmt.Fprintf(&b, "const emojiPatternLookaheadHolds = %q\n\n", holds.String())
	b.WriteString("// emojiPatternLookaheadFails is the emoji regex of mfm-js with the branches\n")
	b.WriteString("// holding a negative lookahead made unmatchable, for inputs where they fail.\n")
	fmt.Fprintf(&b, "const emojiPatternLookaheadFails = %q\n\n", fails.String())
	b.WriteString("// emojiLookaheadOffset is the number of code points from the start of a match\n")
	b.WriteString("// at which every negative lookahead looks, and emojiLookaheadRune is the\n")
	b.WriteString("// character they reject. emojiHasLookahead is false when the regex has none.\n")
	fmt.Fprintf(&b, "const (\n\temojiHasLookahead = %t\n\temojiLookaheadOffset = %d\n\temojiLookaheadRune = %#x\n)\n\n", la.found, la.offset, la.char)
	b.WriteString("// emojiMaxBytes is the longest match in UTF-8 bytes.\n")
	fmt.Fprintf(&b, "const emojiMaxBytes = %d\n\n", alt.maxBytes())
	b.WriteString("// emojiFirstRunes holds every code point a match can start with.\n")
	b.WriteString("var emojiFirstRunes = &unicode.RangeTable{\n")
	writeRangeTable(&b, alt.firstRunes())
	b.WriteString("}\n")
	return format.Source(b.Bytes())
}

func (alt *alternation) countAlternatives() int {
	n := 0
	for _, seq := range alt.branches {
		n++
		for _, it := range seq.items {
			if it.kind == kindGroup {
				n += it.group.countAlternatives()
			}
		}
	}
	return n
}

func writeRangeTable(b *bytes.Buffer, rs []charRange) {
	var r16, r32 []charRange
	latin := 0
	for _, r := range rs {
		switch {
		case r.hi <= 0xFFFF:
			r16 = append(r16, r)
		case r.lo > 0xFFFF:
			r32 = append(r32, r)
		default:
			r16 = append(r16, charRange{r.lo, 0xFFFF})
			r32 = append(r32, charRange{0x10000, r.hi})
		}
		if r.hi <= 0xFF {
			latin++
		}
	}
	if len(r16) > 0 {
		b.WriteString("\tR16: []unicode.Range16{\n")
		for _, r := range r16 {
			fmt.Fprintf(b, "\t\t{Lo: %#04x, Hi: %#04x, Stride: 1},\n", r.lo, r.hi)
		}
		b.WriteString("\t},\n")
	}
	if len(r32) > 0 {
		b.WriteString("\tR32: []unicode.Range32{\n")
		for _, r := range r32 {
			fmt.Fprintf(b, "\t\t{Lo: %#x, Hi: %#x, Stride: 1},\n", r.lo, r.hi)
		}
		b.WriteString("\t},\n")
	}
	fmt.Fprintf(b, "\tLatinOffset: %d,\n", latin)
}
