// Command paginationspec regenerates the golden per-endpoint pagination
// cursor form of upstream Misskey (#3369).
//
// Upstream combines the ID cursors (sinceId / untilId) with the date cursors
// (sinceDate / untilDate) in two ways:
//
//   - id-first: the endpoint passes the dates to
//     QueryService.makePaginationQuery, which ignores both dates when sinceId
//     or untilId is given (and sorts ascending for sinceId alone). Elythia
//     handlers of these endpoints call id.NormalizeCursorIDFirst.
//   - per-direction: the endpoint writes
//     `ps.untilId ?? (ps.untilDate ? gen(ps.untilDate) : null)` and the same for
//     since, so each direction uses its ID or else its date. Elythia handlers
//     call id.NormalizeCursor.
//
// An endpoint that declares sinceDate but never reads it is "unused"; only
// the endpoints named in knownUnused may be. The
// form "id-first-ordered-by-since" is id-first whose endpoint also overrides
// the order with `ps.sinceDate || ps.sinceId` (notes/mentions).
//
// An endpoint that declares sinceDate in a shape the tool cannot classify
// stops the tool instead of being left out. The gate
// (internal/server TestPaginationCursorFormMatchesUpstream) reads the output.
//
// Usage:
//
//	go run ./tools/paginationspec
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/elythia-network/elythia/internal/upstreamsrc"
)

// The forms written to the golden.
const (
	formIDFirst             = "id-first"
	formIDFirstOrderedSince = "id-first-ordered-by-since"
	formPerDirection        = "per-direction"
	formUnused              = "unused"
)

// knownUnused are the endpoints upstream declares the date cursors for but
// never reads, with how they page instead.
//
// **名指しした endpoint だけを unused にする。** 日付の読み方が新しい書き方
// (分割代入で `const { sinceDate } = ps` と受けるなど) になると、`ps.sinceDate` の
// 参照が見つからず「読まない」に化ける。それを黙って unused にすると、ゲートの照合から
// 外れる。
var knownUnused = map[string]string{
	"admin/avatar-decorations/list": "paramDef に日付を宣言するが、一覧を全件引いて ID でも日付でも絞らない",
}

var (
	// paramDef で日付のカーソルを宣言する endpoint を見る。users/followers のように
	// `allOf` の中で宣言するものもあるので、深さは問わない (空白を消した本文で照合)。
	declaresDateRe = regexp.MustCompile(`(?:sinceDate|untilDate):\{`)
	// 行全体のコメント。本文の照合は空白を全て消してから行うので、コメントの中の
	// `ps.sinceDate` を拾わないよう先に落とす。
	lineCommentRe  = regexp.MustCompile(`(?m)^\s*//.*$`)
	blockCommentRe = regexp.MustCompile(`(?s)/\*.*?\*/`)
	spaceRe        = regexp.MustCompile(`\s+`)

	// makePaginationQuery(q, ps.sinceId, ps.untilId, ps.sinceDate, ps.untilDate[, column])。
	// 型引数 (`makePaginationQuery<MiNoteDraft>(`) と、引数を改行して並べる書き方
	// (channels/followed) があるので、空白を消した本文で照合する。
	idFirstDirectRe = regexp.MustCompile(`makePaginationQuery(?:<[^>(]*>)?\([^;]*?,ps\.sinceId,ps\.untilId,ps\.sinceDate,ps\.untilDate[,)]`)
	// ps.untilId ?? (ps.untilDate ? this.idService.gen(ps.untilDate!) : null)。
	perDirectionUntilRe = regexp.MustCompile(`ps\.untilId\?\?\(ps\.untilDate\?this\.idService\.gen\(ps\.untilDate!?\):(?:null|undefined)\)`)
	perDirectionSinceRe = regexp.MustCompile(`ps\.sinceId\?\?\(ps\.sinceDate\?this\.idService\.gen\(ps\.sinceDate!?\):(?:null|undefined)\)`)
	// this.flashService.search(ps.query, { sinceId: ps.sinceId, untilId: ps.untilId, sinceDate: ps.sinceDate, untilDate: ps.untilDate, ... })。
	serviceCallRe = regexp.MustCompile(`this\.([a-z]\w*Service)\.(\w+)\([^;]*?\{sinceId:ps\.sinceId,untilId:ps\.untilId,sinceDate:ps\.sinceDate,untilDate:ps\.untilDate[,}]`)
	// (ps.sinceDate || ps.sinceId) ? 'ASC' : 'DESC' を並びに使う形 (notes/mentions)。
	orderBySinceRe = regexp.MustCompile(`\.orderBy\([^;]*?\(ps\.sinceDate\|\|ps\.sinceId\)\?'ASC':'DESC'\)`)
	dateUseRe      = regexp.MustCompile(`ps\.(?:sinceDate|untilDate)\b`)
)

// normalize drops comments and every whitespace.
func normalize(src string) string {
	src = blockCommentRe.ReplaceAllString(src, "")
	src = lineCommentRe.ReplaceAllString(src, "")
	return spaceRe.ReplaceAllString(src, "")
}

// serviceForwardsDates reports whether method of the service in coreDir
// passes its options' dates to makePaginationQuery.
func serviceForwardsDates(coreDir, service, method string) (bool, error) {
	file := filepath.Join(coreDir, strings.ToUpper(service[:1])+service[1:]+".ts")
	data, err := os.ReadFile(file)
	if err != nil {
		return false, err
	}
	body := normalize(string(data))
	start := regexp.MustCompile(`(?:public|private)(?:async)?` + regexp.QuoteMeta(method) + `\(`).FindStringIndex(body)
	if start == nil {
		return false, fmt.Errorf("%s: method %s not found", file, method)
	}
	rest := body[start[1]:]
	// 次のメソッドの宣言までを本体とみなす。
	if end := regexp.MustCompile(`@bindThis|(?:public|private)(?:async)?\w+\(`).FindStringIndex(rest); end != nil {
		rest = rest[:end[0]]
	}
	re := regexp.MustCompile(`makePaginationQuery(?:<[^>(]*>)?\([^;]*?,(\w+)\.sinceId,(\w+)\.untilId,(\w+)\.sinceDate,(\w+)\.untilDate[,)]`)
	m := re.FindStringSubmatch(rest)
	return m != nil && m[1] == m[2] && m[2] == m[3] && m[3] == m[4], nil
}

// classify returns the cursor form of one endpoint source, "" when it does not
// declare sinceDate.
func classify(src, coreDir string) (string, error) {
	body := normalize(src)
	uses := len(dateUseRe.FindAllString(body, -1))
	if !declaresDateRe.MatchString(body) && uses == 0 {
		return "", nil
	}
	var forms []string
	consumed := 0
	if n := len(idFirstDirectRe.FindAllString(body, -1)); n > 0 {
		forms = append(forms, formIDFirst)
		consumed += 2 * n
	}
	u, s := perDirectionUntilRe.FindAllString(body, -1), perDirectionSinceRe.FindAllString(body, -1)
	if len(u) > 0 || len(s) > 0 {
		if len(u) != 1 || len(s) != 1 {
			return "", fmt.Errorf("per-direction cursor appears %d (until) / %d (since) times", len(u), len(s))
		}
		forms = append(forms, formPerDirection)
		// 1 つの式に ps.untilDate が 2 回ずつ出る。
		consumed += 4
	}
	for _, m := range serviceCallRe.FindAllStringSubmatch(body, -1) {
		ok, err := serviceForwardsDates(coreDir, m[1], m[2])
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%s.%s does not pass the dates to makePaginationQuery", m[1], m[2])
		}
		forms = append(forms, formIDFirst)
		consumed += 2
	}
	if n := len(orderBySinceRe.FindAllString(body, -1)); n > 0 {
		if len(forms) != 1 || forms[0] != formIDFirst || n != 1 {
			return "", fmt.Errorf("orderBy on ps.sinceDate outside an id-first endpoint")
		}
		forms[0] = formIDFirstOrderedSince
		consumed += n
	}
	if len(forms) == 0 && uses == 0 {
		return formUnused, nil
	}
	if len(forms) != 1 {
		return "", fmt.Errorf("cursor forms %v: expected exactly one", forms)
	}
	// 分類に使っていない日付の参照が残っていれば、知らない形で日付を読んでいる。
	if uses != consumed {
		return "", fmt.Errorf("reads the dates %d times, the %s form accounts for %d", uses, forms[0], consumed)
	}
	return forms[0], nil
}

func main() {
	// 本家は .cache/misskey/<版> から読む (#3378)。
	up, _ := upstreamsrc.Dir(".")
	backend := filepath.Join(up, "packages/backend/src")
	epDir := flag.String("endpoints", filepath.Join(backend, "server/api/endpoints"), "path to Misskey backend endpoints dir")
	coreDir := flag.String("core", filepath.Join(backend, "core"), "path to Misskey backend core dir (services)")
	out := flag.String("out", "internal/entitycompat/testdata/golden_pagination_cursor.json", "golden output path")
	flag.Parse()
	if err := upstreamsrc.Check(*epDir); err != nil {
		fmt.Fprintln(os.Stderr, "paginationspec:", err)
		os.Exit(1)
	}

	forms := map[string]string{}
	err := filepath.WalkDir(*epDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".test.ts") {
			return nil
		}
		rel, err := filepath.Rel(*epDir, path)
		if err != nil {
			return err
		}
		ep := strings.TrimSuffix(filepath.ToSlash(rel), ".ts")
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		form, err := classify(string(data), *coreDir)
		if err != nil {
			return fmt.Errorf("%s: %w; teach tools/paginationspec the new form", ep, err)
		}
		if form == formUnused {
			if _, ok := knownUnused[ep]; !ok {
				return fmt.Errorf("%s: declares the date cursors but reads neither in a known form; teach tools/paginationspec the new form, or add it to knownUnused if upstream really ignores them", ep)
			}
		}
		if form != "" {
			forms[ep] = form
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "paginationspec: %v\n", err)
		os.Exit(1)
	}
	// 何も拾えなければ書式が変わった疑いがある。どちらの形も、実在する endpoint を
	// 名指しで要求する。
	for ep, want := range map[string]string{
		"federation/users":              formIDFirst,
		"channels/followed":             formIDFirst,
		"flash/search":                  formIDFirst,
		"notes/mentions":                formIDFirstOrderedSince,
		"notes/timeline":                formPerDirection,
		"v2/admin/emoji/list":           formPerDirection,
		"admin/avatar-decorations/list": formUnused,
	} {
		if forms[ep] != want {
			fmt.Fprintf(os.Stderr, "paginationspec: ERROR %s is %q, want %q (upstream format change?)\n", ep, forms[ep], want)
			os.Exit(1)
		}
	}

	for ep := range knownUnused {
		if forms[ep] != formUnused {
			fmt.Fprintf(os.Stderr, "paginationspec: ERROR knownUnused has %s, but it is %q now; drop it\n", ep, forms[ep])
			os.Exit(1)
		}
	}

	buf, err := json.MarshalIndent(forms, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "paginationspec: marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(buf, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "paginationspec: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("paginationspec: wrote %d endpoints -> %s\n", len(forms), *out)
}
