// Command permspec regenerates the golden per-endpoint permission-level
// snapshot used by the entitycompat permission drift gate (TestPermissionDrift).
//
// Misskey declares each endpoint's access requirement in its meta:
// requireAdmin / requireModerator / requireCredential. This tool reduces those
// to a single level (public < auth < moderator < admin) per endpoint and writes
// it to internal/entitycompat/testdata/golden_permissions.json. The gate then
// checks that mk-go's router middleware is never LOOSER than this level
// (a privilege-escalation / missing-auth guard). Regenerate whenever the
// UPSTREAM_MISSKEY_VERSION is bumped (run `make upstream-fetch` first).
//
// Usage:
//
//	go run ./tools/permspec
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

	"github.com/shiroha-a/mk/internal/upstreamsrc"
)

// meta のフラグは行頭インデント 1 つの位置にしか現れない。**行頭を固定しないと
// コメント内の記述を拾う** — upstream の `i/revoke-token` は
// 「ApiCallService が requireCredential:true かつ kind なしの」という日本語コメントを
// 持っており、素の部分一致だと requireCredential を宣言していないのに auth と判定される。
var (
	requireAdminRe     = regexp.MustCompile(`(?m)^\trequireAdmin:\s*true`)
	requireModeratorRe = regexp.MustCompile(`(?m)^\trequireModerator:\s*true`)
	requireCredRe      = regexp.MustCompile(`(?m)^\trequireCredential:\s*true`)
	// kind は meta の直下 (タブ 1 つ) にだけ現れる。paramDef など入れ子の
	// `kind:` を拾わないよう行頭のインデントまで含めて照合する。
	// **クォートは 2 種類ある。** upstream の `endpoints/i.ts` だけが
	// `kind: "read:account"` とダブルクォートで書かれており、シングル限定にすると
	// **最も広く叩かれる `/api/i` が丸ごと検査対象から外れる** (実測で scope を
	// 変異させても gate が緑のままだった)。
	kindRe = regexp.MustCompile(`(?m)^\tkind:\s*['"]([^'"]+)['"]`)
)

// level reduces an endpoint's meta to its access level. The hierarchy is
// admin > moderator > auth > public; admin/moderator imply credential.
func level(s string) string {
	switch {
	case requireAdminRe.MatchString(s):
		return "admin"
	case requireModeratorRe.MatchString(s):
		return "moderator"
	case requireCredRe.MatchString(s):
		return "auth"
	default:
		return "public"
	}
}

func main() {
	// 本家は .cache/misskey/<版> から読む (#3378)。版のファイルが
	// 読めなければ up は空になり、下の Check が取得の案内を出して落ちる。
	up, _ := upstreamsrc.Dir(".")
	epDir := flag.String("endpoints", filepath.Join(up, "packages/backend/src/server/api/endpoints"), "path to Misskey backend endpoints dir")
	out := flag.String("out", "internal/entitycompat/testdata/golden_permissions.json", "golden snapshot output path")
	kindsOut := flag.String("kinds-out", "internal/entitycompat/testdata/golden_oauth_kinds.json", "OAuth kind golden output path")
	flag.Parse()
	if err := upstreamsrc.Check(*epDir); err != nil {
		fmt.Fprintln(os.Stderr, "permspec:", err)
		os.Exit(1)
	}

	perms := map[string]string{}
	kinds := map[string]string{}
	err := filepath.WalkDir(*epDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".ts") {
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
		body := string(data)
		perms[ep] = level(body)
		if m := kindRe.FindStringSubmatch(body); m != nil {
			kinds[ep] = m[1]
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "permspec: walk: %v\n", err)
		os.Exit(1)
	}
	if len(perms) == 0 {
		fmt.Fprintln(os.Stderr, "permspec: ERROR no endpoints parsed (wrong path or upstream format change?)")
		os.Exit(1)
	}

	buf, err := json.MarshalIndent(perms, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "permspec: marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(buf, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "permspec: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("permspec: wrote %d endpoints -> %s\n", len(perms), *out)

	if len(kinds) == 0 {
		fmt.Fprintln(os.Stderr, "permspec: ERROR no OAuth kinds parsed (upstream format change?)")
		os.Exit(1)
	}
	kbuf, err := json.MarshalIndent(kinds, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "permspec: marshal kinds: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*kindsOut, append(kbuf, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "permspec: write kinds: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("permspec: wrote %d OAuth kinds -> %s\n", len(kinds), *kindsOut)
}
