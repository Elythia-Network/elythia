package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// collectTSEndpoints walks dir and returns a sorted, deduplicated list of
// API paths corresponding to Misskey TS endpoints. Each .ts file under dir
// (excluding the registration plumbing files `endpoint.ts` / `endpoints.ts`
// / `endpoint-list.ts` / *.d.ts) is treated as one endpoint with API path
// `/api/<relative-path-without-extension>`.
//
// The TS source defines endpoints by file convention: `admin/foo.ts` is
// exposed as POST /api/admin/foo. Phase 1 では POST 固定で扱う (Misskey の
// /api/* は数件の例外を除き全 POST、例外検知は Phase 2 で扱う)。
func collectTSEndpoints(dir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".d.ts") {
			return nil
		}
		// Vitest unit test (`foo.test.ts`) は endpoint ではないので除外。
		// 残すと `notes/create.test` のような偽 endpoint が matrix に
		// 紛れ込んでしまう。
		if strings.HasSuffix(name, ".test.ts") {
			return nil
		}
		// 注意: endpoints/ 内の `endpoint.ts` / `endpoints.ts` は実 API
		// endpoint (`/api/endpoint` / `/api/endpoints` — meta tag)。plumbing
		// 用 aggregator (`endpoint-list.ts`) は endpoints/ の外にあるので、
		// この walk では出現しない。よって名前ベースの追加 filter は不要。
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = strings.TrimSuffix(rel, ".ts")
		// Windows path separator 対策。Misskey TS の repo は POSIX 前提だが
		// `filepath.Rel` は OS 依存 separator を返すので明示的に "/" に
		// 揃えておく。
		rel = filepath.ToSlash(rel)
		paths = append(paths, "/api/"+rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}
	sort.Strings(paths)
	return dedupSorted(paths), nil
}

// dedupSorted returns the input slice with consecutive duplicates removed.
// 入力は事前に sort 済みであることが前提。
//
// 注意: 返り値は入力の backing array を共有するため、呼び出し側で `in` を
// 別途参照する場合は破壊される可能性がある。本パッケージ内では
// collectTSEndpoints から呼ばれて即返却されるだけなので影響なし。
func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := in[:1]
	for _, s := range in[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}

// Report holds the comparison result between TS and mk-go endpoints.
type Report struct {
	// Both も method 込みで持つ。endpoints/ 由来はすべて POST だが、
	// ApiServerService.ts 直登録の `GET /api/v1/instance/peers` を POST と
	// 表示してしまうと matrix が実態と食い違う。
	Both []BothRoute
	// TSOnly は method 込みで持つ。endpoints/ 由来はすべて POST だが、
	// ApiServerService.ts の直登録には GET (`/api/v1/instance/peers`) も
	// あり、method を落とすと未実装 endpoint を正しく表示できない。
	TSOnly []TSOnlyRoute
	MkOnly []MkOnlyRoute
}

// BothRoute is an endpoint present on both sides.
type BothRoute struct {
	Method string
	Path   string
}

// TSOnlyRoute is a Misskey TS endpoint that mk-go does not implement.
type TSOnlyRoute struct {
	Method string
	Path   string
}

// MkOnlyCategory classifies why a mk-go route has no counterpart in TS, so
// the matrix can present them in meaningful groups rather than one flat dump.
type MkOnlyCategory string

const (
	// CatGETVariant: mk-go offers a non-POST method for a path whose POST
	// is already present (typically GET added on top of an existing POST).
	CatGETVariant MkOnlyCategory = "get_variant"
	// CatChatExt: paths under /api/chat/* — federated chat 拡張 (yojo-art/
	// cherrypick 系列)。Misskey TS 本家 chat にない endpoint をまとめる。
	CatChatExt MkOnlyCategory = "chat_ext"
	// CatOther: 上記いずれにも当てはまらない mk-go 独自 / alias endpoint。
	CatOther MkOnlyCategory = "other"
)

// MkOnlyRoute is a mk-go route that has no counterpart in TS.
type MkOnlyRoute struct {
	Method   string
	Path     string
	Category MkOnlyCategory
}

// tsDirectRoute is one endpoint that Misskey TS registers directly on
// fastify in `ApiServerService.ts` instead of via the endpoints/ directory
// convention. filename-derived collection cannot see these.
type tsDirectRoute struct {
	Method string
	Path   string
}

// fastifyDirectRe matches `fastify.post(...)` / `fastify.get(...)` route
// registrations and captures the method plus the quoted path. The generic
// type parameter (`fastify.post<{ Body: ... }>(`) may span multiple lines,
// so the path is captured from the argument list rather than requiring it on
// the same line as the method name.
//
// 対応する 2 形:
//
//	fastify.post<{ Body: { code: string; } }>('/signup-pending', ...)   // 1 行
//	fastify.post<{                                                      // 複数行
//	    Body: { ... };
//	}>('/signup', ...)
var fastifyDirectRe = regexp.MustCompile(`fastify\.(post|get)\s*(?:<[\s\S]*?>)?\s*\(\s*'([^']+)'`)

// collectTSDirectRoutes parses ApiServerService.ts and returns the routes it
// registers straight on fastify (i.e. outside endpoints/).
//
// 以前は path を hardcode していたが、本家の版を上げるたびに手で追随する
// 必要があり実際に古くなっていた (`/miauth/:session/check` が漏れ、
// `GET /v1/instance/peers` は method 違いで検出対象ですらなかった)。
// source から直接読むことで bump 時の追随漏れを構造的に無くす。
//
// path が空 (= ファイル不在 / 想定外の書式) の場合は呼び出し側でエラーに
// する。silently 空集合にすると直登録 endpoint が丸ごと "mk-go only" に
// 誤分類され、matrix が静かに壊れるため。
func collectTSDirectRoutes(path string) ([]tsDirectRoute, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read ApiServerService.ts: %w", err)
	}
	var out []tsDirectRoute
	for _, m := range fastifyDirectRe.FindAllStringSubmatch(string(src), -1) {
		method, p := strings.ToUpper(m[1]), m[2]
		// `fastify.get('/*', ...)` は 404 fallback の catch-all で endpoint
		// ではない。mk-go 側でも Echo の catch-all を除外している。
		if p == "/*" {
			continue
		}
		out = append(out, tsDirectRoute{Method: method, Path: "/api" + p})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no direct fastify routes found in %s (format changed?)", path)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out, nil
}

// classifyMkOnly assigns a category. Priority order:
// get_variant > chat_ext > other.
func classifyMkOnly(method, path string, mkHasPOST map[string]bool) MkOnlyCategory {
	if method != "POST" && mkHasPOST[path] {
		return CatGETVariant
	}
	if strings.HasPrefix(path, "/api/chat/") {
		return CatChatExt
	}
	return CatOther
}

func compare(tsEndpoints []string, direct []tsDirectRoute, mk *DumpedRoutes) Report {
	ts := make(map[string]bool, len(tsEndpoints)+len(direct))
	for _, p := range tsEndpoints {
		ts[p] = true
	}
	// filename-derived な TS endpoint 集合に、ApiServerService.ts が router
	// 直登録している POST path を補う。これらは endpoints/ 配下に無いので
	// walk では拾えない。非 POST の直登録は method 単位で別途突き合わせる。
	var tsDirectNonPOST []tsDirectRoute
	for _, d := range direct {
		if d.Method == "POST" {
			ts[d.Path] = true
			continue
		}
		tsDirectNonPOST = append(tsDirectNonPOST, d)
	}

	mkPOSTpaths := make(map[string]bool)
	mkMethodPaths := make(map[string]bool)
	type pendingRoute struct{ method, path string }
	var pending []pendingRoute
	for _, r := range mk.Routes {
		p := stripQueryString(r.Path)
		// 対象は /api/* の POST のみ。/api/* 以外 (e.g. /.well-known/*,
		// /healthz, /nodeinfo/*) は Misskey 連合 endpoint や internal なので
		// Misskey API 互換性 matrix の対象外。
		if !strings.HasPrefix(p, "/api/") {
			continue
		}
		// Echo は 404 用に各 method 1 つずつ "/api/*" catch-all route を
		// 自動登録する。method 名 "echo_route_not_found" の擬似 route や
		// path が exactly "/api/*" のものは route ではなく Echo 内部 artifact
		// なので matrix から除外する。
		if p == "/api/*" || r.Method == "echo_route_not_found" {
			continue
		}
		if r.Method == "POST" {
			mkPOSTpaths[p] = true
		}
		mkMethodPaths[r.Method+" "+p] = true
		pending = append(pending, pendingRoute{method: r.Method, path: p})
	}

	var both []BothRoute
	var tsOnly []TSOnlyRoute
	// matchedBoth は "METHOD PATH" で both に分類済の mk-go route を記録する。
	// mk-only の走査でこれを引いて二重計上を防ぐ。
	matchedBoth := make(map[string]bool)
	for p := range ts {
		if mkPOSTpaths[p] {
			both = append(both, BothRoute{Method: "POST", Path: p})
			matchedBoth["POST "+p] = true
		} else {
			tsOnly = append(tsOnly, TSOnlyRoute{Method: "POST", Path: p})
		}
	}
	// 非 POST の直登録 (現状 `GET /api/v1/instance/peers`) は method 込みで
	// 突き合わせる。mk-go 側に同 method の route が無ければ未実装。
	for _, d := range tsDirectNonPOST {
		key := d.Method + " " + d.Path
		if mkMethodPaths[key] {
			both = append(both, BothRoute{Method: d.Method, Path: d.Path})
			matchedBoth[key] = true
			continue
		}
		tsOnly = append(tsOnly, TSOnlyRoute{Method: d.Method, Path: d.Path})
	}

	// classify pending mk-go routes into "both" vs "mk-go only" 後に
	// category 付与する。GET variant の判定で mkPOSTpaths を参照するので、
	// 上の POST 走査が完了してから行う必要がある。
	var mkOnly []MkOnlyRoute
	for _, pr := range pending {
		if matchedBoth[pr.method+" "+pr.path] {
			continue // 既に both に分類済
		}
		mkOnly = append(mkOnly, MkOnlyRoute{
			Method:   pr.method,
			Path:     pr.path,
			Category: classifyMkOnly(pr.method, pr.path, mkPOSTpaths),
		})
	}

	sort.Slice(both, func(i, j int) bool {
		if both[i].Path != both[j].Path {
			return both[i].Path < both[j].Path
		}
		return both[i].Method < both[j].Method
	})
	sort.Slice(tsOnly, func(i, j int) bool {
		if tsOnly[i].Path != tsOnly[j].Path {
			return tsOnly[i].Path < tsOnly[j].Path
		}
		return tsOnly[i].Method < tsOnly[j].Method
	})
	sort.Slice(mkOnly, func(i, j int) bool {
		if mkOnly[i].Path != mkOnly[j].Path {
			return mkOnly[i].Path < mkOnly[j].Path
		}
		return mkOnly[i].Method < mkOnly[j].Method
	})

	return Report{Both: both, TSOnly: tsOnly, MkOnly: mkOnly}
}

// mkOnlyCategoryOrder defines section order. CatChatExt は連合 chat 拡張
// (cherrypick 系) で production の意味的 extra なので CatGETVariant の次に
// 置く。CatOther は調査対象になりうる残差。
var mkOnlyCategoryOrder = []struct {
	cat   MkOnlyCategory
	title string
	note  string
}{
	{
		cat:   CatGETVariant,
		title: "GET variant 追加",
		note:  "Misskey TS が POST のみ提供する endpoint に対し、mk-go が同パスへ GET を追加登録したもの。browser から直接 link で叩く用途等の利便目的で、対応する POST は両側に存在する。",
	},
	{
		cat:   CatChatExt,
		title: "cherrypick 系 chat 拡張",
		note:  "yojo-art/cherrypick 由来の federated chat 拡張 endpoint。Misskey TS 本家 chat は local-only だが、cherrypick 系列が ActivityPub 連合可能に拡張しており、mk-go はこの拡張セットを upstream として取り込んでいる。",
	},
	{
		cat:   CatOther,
		title: "その他 mk-go 独自 / alias",
		note:  "上記カテゴリに当てはまらない mk-go 独自 endpoint。backward-compat shim や alias を含む。",
	},
}

func renderMkOnlyCategorized(b *strings.Builder, routes []MkOnlyRoute) {
	bucket := make(map[MkOnlyCategory][]MkOnlyRoute, 4)
	for _, m := range routes {
		bucket[m.Category] = append(bucket[m.Category], m)
	}
	for _, sec := range mkOnlyCategoryOrder {
		items := bucket[sec.cat]
		fmt.Fprintf(b, "### %s (%d)\n\n", sec.title, len(items))
		fmt.Fprintf(b, "%s\n\n", sec.note)
		if len(items) == 0 {
			b.WriteString("(なし)\n\n")
			continue
		}
		b.WriteString("| Method | Path |\n|--------|------|\n")
		for _, m := range items {
			fmt.Fprintf(b, "| %s | `%s` |\n", m.Method, m.Path)
		}
		b.WriteString("\n")
	}
}

func renderMarkdown(r Report, tsVersion, mkVersion string) string {
	var b strings.Builder

	tsTotal := len(r.Both) + len(r.TSOnly)
	var coverage string
	if tsTotal > 0 {
		coverage = fmt.Sprintf("%.1f%%", 100.0*float64(len(r.Both))/float64(tsTotal))
	} else {
		coverage = "n/a"
	}

	fmt.Fprintf(&b, "# API Compatibility Matrix\n\n")
	fmt.Fprintf(&b, "<!-- AUTO-GENERATED by `make apicompat`. Do not edit manually. -->\n\n")
	fmt.Fprintf(&b, "- Misskey (TS) version: `%s`\n", tsVersion)
	fmt.Fprintf(&b, "- mk-go version: `%s`\n", mkVersion)
	fmt.Fprintf(&b, "- TS endpoints (POST `/api/*`): **%d**\n", tsTotal)
	fmt.Fprintf(&b, "- mk-go implemented (TS の subset): **%d**\n", len(r.Both))
	fmt.Fprintf(&b, "- mk-go coverage of TS: **%s**\n", coverage)
	fmt.Fprintf(&b, "- TS only (mk-go 未実装): **%d**\n", len(r.TSOnly))
	fmt.Fprintf(&b, "- mk-go only (TS spec 外): **%d**\n\n", len(r.MkOnly))

	fmt.Fprintf(&b, "## TS 側に存在するが mk-go で未実装 (%d)\n\n", len(r.TSOnly))
	if len(r.TSOnly) == 0 {
		b.WriteString("(なし)\n\n")
	} else {
		b.WriteString("| Method | Path |\n|--------|------|\n")
		for _, e := range r.TSOnly {
			fmt.Fprintf(&b, "| %s | `%s` |\n", e.Method, e.Path)
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## mk-go 側にしかない endpoint (%d)\n\n", len(r.MkOnly))
	if len(r.MkOnly) == 0 {
		b.WriteString("(なし)\n\n")
	} else {
		renderMkOnlyCategorized(&b, r.MkOnly)
	}

	fmt.Fprintf(&b, "## 両方に存在する endpoint (%d)\n\n", len(r.Both))
	if len(r.Both) == 0 {
		b.WriteString("(なし)\n\n")
	} else {
		b.WriteString("<details>\n<summary>展開する</summary>\n\n")
		b.WriteString("| Method | Path |\n|--------|------|\n")
		for _, rt := range r.Both {
			fmt.Fprintf(&b, "| %s | `%s` |\n", rt.Method, rt.Path)
		}
		b.WriteString("\n</details>\n")
	}

	return b.String()
}
