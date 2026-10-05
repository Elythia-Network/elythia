// Command apicompat compares mk-go's HTTP route surface against the Misskey
// TS endpoint set and emits a markdown compatibility matrix.
//
// Usage:
//
//	apicompat \
//	  -ts-endpoints-dir .cache/misskey/<version>/packages/backend/src/server/api/endpoints \
//	  -mk-routes /tmp/mk-routes.json \
//	  -out docs/api-compat.md
//
// Misskey TS の endpoint は file path = API path 規約 (`admin/foo.ts` →
// `/api/admin/foo`) で扱う。`ApiServerService.ts` が fastify 直登録する
// auth 系 path は filename-derived の集合に乗らないので、comparator 側で
// `tsRouterDirectPOSTPaths` を hardcode で補っている。
// mk-go 側は `elythia dump-routes` が出す JSON を入力に取り、`/api/*`
// prefix の route のみを対象にする。POST は両側比較、それ以外の method は
// "GET variant" 等の mk-go only 拡張として分類する。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shiroha-a/mk/internal/upstreamsrc"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "apicompat: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable entry point. It parses args off a fresh FlagSet so
// repeated invocations from tests don't interfere with the global flag.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("apicompat", flag.ContinueOnError)
	// flag.Parse はエラー時 usage を fs.Output() に書く。test 中の noisy
	// 出力を避けるため stderr に向ける (本番では os.Stderr が渡る)。
	fs.SetOutput(stderr)
	// 本家は .cache/misskey/<版> から読む (#3378)。
	up, _ := upstreamsrc.Dir(".")
	tsDir := fs.String("ts-endpoints-dir", filepath.Join(up, "packages/backend/src/server/api/endpoints"), "path to Misskey TS endpoint .ts files")
	tsDirectFile := fs.String("ts-api-server-service", filepath.Join(up, "packages/backend/src/server/api/ApiServerService.ts"), "path to Misskey TS ApiServerService.ts (fastify 直登録 endpoint の抽出元)")
	mkRoutesPath := fs.String("mk-routes", "", "path to JSON output of `elythia dump-routes` (default: stdin)")
	outPath := fs.String("out", "", "output markdown path (default: stdout)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse args: %w", err)
	}
	if err := upstreamsrc.Check(*tsDir); err != nil {
		return err
	}

	tsEndpoints, err := collectTSEndpoints(*tsDir)
	if err != nil {
		return fmt.Errorf("collect TS endpoints: %w", err)
	}

	mkRoutes, err := readMkRoutes(*mkRoutesPath, stdin)
	if err != nil {
		return fmt.Errorf("read mk-go routes: %w", err)
	}

	// endpoints/ の file-walk では拾えない fastify 直登録 endpoint を
	// ApiServerService.ts から抽出して補う。
	tsDirect, err := collectTSDirectRoutes(*tsDirectFile)
	if err != nil {
		return fmt.Errorf("collect TS direct routes: %w", err)
	}

	report := compare(tsEndpoints, tsDirect, mkRoutes)
	md := renderMarkdown(report, mkRoutes.MisskeyVersion, mkRoutes.MkGoVersion)

	out := stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			return fmt.Errorf("open out: %w", err)
		}
		defer f.Close()
		out = f
	}
	if _, err := io.WriteString(out, md); err != nil {
		return fmt.Errorf("write out: %w", err)
	}
	return nil
}

// readMkRoutes decodes a DumpedRoutes JSON from the file at path. If path is
// empty, stdin is used instead.
func readMkRoutes(path string, stdin io.Reader) (*DumpedRoutes, error) {
	r := stdin
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	var dr DumpedRoutes
	if err := json.NewDecoder(r).Decode(&dr); err != nil {
		return nil, fmt.Errorf("decode mk-routes JSON: %w", err)
	}
	return &dr, nil
}

// DumpedRoute mirrors internal/server.DumpedRoute. Duplicated here so the
// tool stays a single-binary CLI without importing the server package
// (which would pull in DB / queue / echo dependencies as transitive deps).
//
// 注意: schema が server.DumpedRoute と drift するとツール側が silent に
// default value 化する。両 type は同じ field を保つこと。
type DumpedRoute struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// DumpedRoutes mirrors internal/server.DumpedRoutes.
type DumpedRoutes struct {
	MisskeyVersion string        `json:"misskeyVersion"`
	MkGoVersion    string        `json:"mkGoVersion"`
	Routes         []DumpedRoute `json:"routes"`
}

// stripQueryString strips a trailing query string fragment (`?a=1`) from a
// path. Echo の Routes() 出力には通常 query は含まれないが、外部 source の
// JSON を食わせる用途で扱えると堅い。なお path param normalize (`:id` ↔
// `{id}`) は Phase 1 のスコープ外で、Phase 2 (api.json ベース比較) で扱う。
func stripQueryString(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}
