// Command emojiregex generates the Unicode emoji pattern of the MFM parser
// (internal/activitypub/mfm/emoji_regex_gen.go) from the emoji regex of
// @misskey-dev/emoji-data, the one mfm-js uses for its unicodeEmoji parser.
// The regex is read from the frontend's MFM parser, @elythia-network/mfm,
// which ships the same regex, so that the server and the frontend read the
// same emoji.
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
	"strconv"
	"strings"
	"unicode/utf16"
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

// mfm-js の版も snapshot に残す。正規表現が同じでも、写す mfm-js の版が変わったら
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

// frontend の MFM の解析器 (@elythia-network/mfm、#3515) が持つ絵文字の正規表現。
// 解析器は emoji-data の正規表現を生成して同梱しており、生成元の emoji-data と、
// その使い方を写した mfm-js の版も書き出している。ここから読めば、サーバーと
// frontend が同じ正規表現で絵文字を読んでいることを確かめられる。
const (
	frontendMfmPackage = "@elythia-network/mfm"
	frontendMfmRegexJS = "built/emoji-regex.gen.js"
)

var (
	emojiDataVersionDecl  = regexp.MustCompile(`(?m)^export const emojiDataVersion = '([^'\\]+)';$`)
	emojiMfmJsVersionDecl = regexp.MustCompile(`(?m)^export const emojiMfmJsVersion = '([^'\\]+)';$`)
	// 文字列リテラルの中の逃がし文字は \\ と \uXXXX だけを受け付ける (他の書き方が
	// 出てきたら読み違えないよう落とす)。
	emojiRegexDecl = regexp.MustCompile(`(?m)^export const emojiRegex = new RegExp\('((?:[^'\\]|\\\\|\\u[0-9a-fA-F]{4})*)', '([a-z]*)'\);$`)
)

// emojiRegexFlags are the flags the parser must build the regex with: sticky
// (match at the current position only) and, like mfm-js, without `u`, so that
// it works on UTF-16 code units as the converter assumes.
const emojiRegexFlags = "y"

// loadSource reads the emoji regex that the frontend's MFM parser uses.
//
// 版は書き写さず、frontend が依存する @elythia-network/mfm から辿る。
func loadSource(misskeyDir string) (regexSource, error) {
	link := filepath.Join(misskeyDir, "packages", "frontend", "node_modules", filepath.FromSlash(frontendMfmPackage))
	dir, err := filepath.EvalSymlinks(link)
	if err != nil {
		return regexSource{}, fmt.Errorf("%s is not installed (run pnpm install in %s): %w", frontendMfmPackage, misskeyDir, err)
	}
	var pkg struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := readJSON(filepath.Join(dir, "package.json"), &pkg); err != nil {
		return regexSource{}, err
	}
	if pkg.Name != frontendMfmPackage {
		return regexSource{}, fmt.Errorf("%s is %q, want %s", link, pkg.Name, frontendMfmPackage)
	}
	js, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(frontendMfmRegexJS)))
	if err != nil {
		return regexSource{}, err
	}
	src, err := extractSource(js)
	if err != nil {
		return regexSource{}, fmt.Errorf("%s %s: %w", frontendMfmPackage, pkg.Version, err)
	}
	return src, nil
}

// extractSource reads the versions and the regex from the parser's
// emoji-regex.gen.js. Each declaration must appear exactly once.
func extractSource(js []byte) (regexSource, error) {
	one := func(re *regexp.Regexp, name string) ([][]byte, error) {
		m := re.FindAllSubmatch(js, -1)
		if len(m) != 1 {
			return nil, fmt.Errorf("found %d %s declarations, want 1", len(m), name)
		}
		return m[0], nil
	}
	data, err := one(emojiDataVersionDecl, "emojiDataVersion")
	if err != nil {
		return regexSource{}, err
	}
	mfmjs, err := one(emojiMfmJsVersionDecl, "emojiMfmJsVersion")
	if err != nil {
		return regexSource{}, err
	}
	re, err := one(emojiRegexDecl, "emojiRegex")
	if err != nil {
		return regexSource{}, err
	}
	if flags := string(re[2]); flags != emojiRegexFlags {
		return regexSource{}, fmt.Errorf("emojiRegex has flags %q, want %q", flags, emojiRegexFlags)
	}
	source, err := unquoteJS(string(re[1]))
	if err != nil {
		return regexSource{}, err
	}
	return regexSource{mfmjs: string(mfmjs[1]), version: string(data[1]), source: source}, nil
}

// unquoteJS decodes the content of a JavaScript string literal that escapes
// only with \\ and \uXXXX (outside the surrogate range).
//
// 正規表現の元の文字列には © ® がそのまま入っており、生成した側は \u00a9 の形で書く。
func unquoteJS(lit string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(lit); i++ {
		if lit[i] != '\\' {
			b.WriteByte(lit[i])
			continue
		}
		switch {
		case strings.HasPrefix(lit[i:], `\\`):
			b.WriteByte('\\')
			i++
		case strings.HasPrefix(lit[i:], `\u`) && len(lit) >= i+6:
			v, err := strconv.ParseUint(lit[i+2:i+6], 16, 16)
			if err != nil {
				return "", fmt.Errorf("bad escape %q in emojiRegex", lit[i:i+6])
			}
			if utf16.IsSurrogate(rune(v)) {
				return "", fmt.Errorf("surrogate escape %q in emojiRegex", lit[i:i+6])
			}
			b.WriteRune(rune(v))
			i += 5
		default:
			return "", fmt.Errorf("unsupported escape at %d in emojiRegex", i)
		}
	}
	return b.String(), nil
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
