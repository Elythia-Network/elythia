package entitycompat

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// frontend/packages/elythia-js は Elythia 独自のエンドポイントの型を持つ (#3417)。
// 型は手で書くので、Go にエンドポイントを足して型を書き忘れる、型だけ残して
// エンドポイントを消す、のどちらも静かに起きる。Go のルートと型のキーを
// 突き合わせて、片方だけの変更を落とす。
//
// Go 側の一覧は docs/api-compat.md の「mk-go 側にしかない endpoint」から取る。
// あの表は TestAPICompatDoc_MatchesRouter が router.go と一致することを保証して
// いるので、ルーターを読み直さずに済む。GET の別名 (本家にもあるエンドポイント
// を GET で呼べるようにしたもの) は misskey-js のクライアントが使わないので
// 対象にしない。

var (
	elythiaJSDir = filepath.Join("..", "..", "frontend", "packages", "elythia-js")

	// apiCompatPostRowRe matches `| POST | `/api/<path>` |` rows.
	apiCompatPostRowRe = regexp.MustCompile("^\\| POST \\| `/api/([^`]+)` \\|")
	// elythiaAPICompatAnyRowRe matches any `| <METHOD> | `/api/<path>` |` row.
	elythiaAPICompatAnyRowRe = regexp.MustCompile("^\\| ([A-Z]+) \\| `/api/([^`]+)` \\|")
	// elythiaEndpointKeyRe matches the top-level keys of ElythiaEndpoints
	// (`\t'<path>': {`).
	elythiaEndpointKeyRe = regexp.MustCompile(`^\t'([^']+)': \{`)
)

// elythiaOnlyPostEndpoints returns the POST endpoints that exist only in
// Elythia, read from docs/api-compat.md.
func elythiaOnlyPostEndpoints(t *testing.T) []string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join("..", "..", "docs", "api-compat.md"))
	if err != nil {
		t.Fatalf("read docs/api-compat.md: %v", err)
	}
	var paths []string
	inOnly, inCategory := false, false
	for _, line := range strings.Split(string(blob), "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			inOnly = strings.Contains(line, "側にしかない endpoint")
			inCategory = false
		case strings.HasPrefix(line, "### "):
			// GET の別名の節は対象にしない (本家にもあるエンドポイント)
			inCategory = inOnly && !strings.Contains(line, "GET variant")
		case inCategory:
			if m := apiCompatPostRowRe.FindStringSubmatch(line); m != nil {
				paths = append(paths, m[1])
			} else if m := elythiaAPICompatAnyRowRe.FindStringSubmatch(line); m != nil {
				// GET の別名の節の外に POST 以外が現れたら、独自の GET 専用の
				// エンドポイントが足されたということ。misskeyApiGet も
				// Elythia.Endpoints を使うので型が要るが、このゲートは POST しか
				// 突き合わせていない。黙って見逃さず、ゲートの拡張を促す
				t.Errorf("Elythia 独自の %s のエンドポイント /api/%s がある。このゲートは POST しか突き合わせないので、GET も扱えるように直す", m[1], m[2])
			}
		}
	}
	// 表の書式が変わって 1 件も拾えないと、下の突き合わせは「型も pending も
	// 空」で緑になる。実在するものを名指しで要求して空振りを落とす
	if len(paths) == 0 || !contains(paths, "admin/federation/rules/list") {
		t.Fatalf("docs/api-compat.md から Elythia 独自の POST のエンドポイントを読めない (%d 件)", len(paths))
	}
	return paths
}

// elythiaJSTypedEndpoints returns the keys of ElythiaEndpoints.
func elythiaJSTypedEndpoints(t *testing.T) []string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(elythiaJSDir, "src", "endpoints.ts"))
	if err != nil {
		t.Fatalf("read elythia-js/src/endpoints.ts: %v", err)
	}
	src := string(blob)
	start := strings.Index(src, "export type ElythiaEndpoints = {")
	if start < 0 {
		t.Fatal("elythia-js/src/endpoints.ts に `export type ElythiaEndpoints = {` が無い")
	}
	var keys []string
	for _, line := range strings.Split(src[start:], "\n")[1:] {
		if line == "};" {
			return keys
		}
		if m := elythiaEndpointKeyRe.FindStringSubmatch(line); m != nil {
			keys = append(keys, m[1])
		}
	}
	t.Fatal("elythia-js/src/endpoints.ts の ElythiaEndpoints の終わり (`};`) が見つからない")
	return nil
}

// elythiaJSPendingEndpoints returns the endpoints listed in
// pending-endpoints.txt (not typed yet).
func elythiaJSPendingEndpoints(t *testing.T) []string {
	t.Helper()
	blob, err := os.ReadFile(filepath.Join(elythiaJSDir, "pending-endpoints.txt"))
	if err != nil {
		t.Fatalf("read elythia-js/pending-endpoints.txt: %v", err)
	}
	var paths []string
	for _, line := range strings.Split(string(blob), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		paths = append(paths, line)
	}
	return paths
}

// TestElythiaJS_EndpointsMatchRouter asserts that every Elythia-only POST
// endpoint is either typed in elythia-js or listed as pending, and that
// elythia-js does not type or list anything the server does not have.
func TestElythiaJS_EndpointsMatchRouter(t *testing.T) {
	server := elythiaOnlyPostEndpoints(t)
	typed := elythiaJSTypedEndpoints(t)
	pending := elythiaJSPendingEndpoints(t)

	count := func(list []string) map[string]int {
		m := map[string]int{}
		for _, p := range list {
			m[p]++
		}
		return m
	}
	inServer, inTyped, inPending := count(server), count(typed), count(pending)

	var missing, typedOnly, pendingOnly, both, dup []string
	for p := range inServer {
		if inTyped[p] == 0 && inPending[p] == 0 {
			missing = append(missing, p)
		}
	}
	for p, n := range inTyped {
		if inServer[p] == 0 {
			typedOnly = append(typedOnly, p)
		}
		if inPending[p] > 0 {
			both = append(both, p)
		}
		if n > 1 {
			dup = append(dup, p)
		}
	}
	for p, n := range inPending {
		if inServer[p] == 0 {
			pendingOnly = append(pendingOnly, p)
		}
		if n > 1 {
			dup = append(dup, p)
		}
	}
	report := func(list []string, msg string) {
		if len(list) == 0 {
			return
		}
		sort.Strings(list)
		t.Errorf("%s:\n  %s", msg, strings.Join(list, "\n  "))
	}
	report(missing, "Elythia 独自のエンドポイントが elythia-js に無い (ElythiaEndpoints に型を書くか、pending-endpoints.txt に足す)")
	report(typedOnly, "ElythiaEndpoints に、サーバーに無いエンドポイントの型がある")
	report(pendingOnly, "pending-endpoints.txt に、サーバーに無いエンドポイントがある")
	report(both, "型を書いたエンドポイントが pending-endpoints.txt に残っている (外す)")
	report(dup, "elythia-js に同じエンドポイントが 2 回以上ある")
}
