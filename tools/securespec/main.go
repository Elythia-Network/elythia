// Command securespec regenerates the golden list of Misskey endpoints marked
// `secure: true` in their meta, used by the entitycompat secure drift gate
// (TestSecureDrift). A secure endpoint must be reachable only via the user's
// native session token, not a third-party app / MiAuth access token. The gate
// checks mk-go applies middleware.RequireSecure to each. Regenerate whenever the
// UPSTREAM_MISSKEY_VERSION is bumped (run `make upstream-fetch` first).
//
// Usage:
//
//	go run ./tools/securespec
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/shiroha-a/mk/internal/upstreamsrc"
)

var secureRe = regexp.MustCompile(`\bsecure:\s*true`)

func main() {
	// 本家は .cache/misskey/<版> から読む (#3378)。版のファイルが
	// 読めなければ up は空になり、下の Check が取得の案内を出して落ちる。
	up, _ := upstreamsrc.Dir(".")
	epDir := flag.String("endpoints", filepath.Join(up, "packages/backend/src/server/api/endpoints"), "path to Misskey backend endpoints dir")
	out := flag.String("out", "internal/entitycompat/testdata/golden_secure_endpoints.json", "golden snapshot output path")
	flag.Parse()
	if err := upstreamsrc.Check(*epDir); err != nil {
		fmt.Fprintln(os.Stderr, "securespec:", err)
		os.Exit(1)
	}

	var secure []string
	err := filepath.WalkDir(*epDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".ts") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !secureRe.Match(data) {
			return nil
		}
		rel, err := filepath.Rel(*epDir, path)
		if err != nil {
			return err
		}
		secure = append(secure, strings.TrimSuffix(filepath.ToSlash(rel), ".ts"))
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "securespec: walk: %v\n", err)
		os.Exit(1)
	}
	if len(secure) == 0 {
		fmt.Fprintln(os.Stderr, "securespec: ERROR no secure endpoints parsed (wrong path or upstream format change?)")
		os.Exit(1)
	}
	sort.Strings(secure)

	buf, err := json.MarshalIndent(secure, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "securespec: marshal: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(buf, '\n'), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "securespec: write: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("securespec: wrote %d secure endpoints -> %s\n", len(secure), *out)
}
