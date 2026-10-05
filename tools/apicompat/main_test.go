package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/upstreamsrc"
)

func TestReadMkRoutesFromStdin(t *testing.T) {
	dr := &DumpedRoutes{
		MisskeyVersion: "2026.5.4",
		MkGoVersion:    "0.9.1",
		Routes: []DumpedRoute{
			{Method: "POST", Path: "/api/meta"},
		},
	}
	raw, err := json.Marshal(dr)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	got, err := readMkRoutes("", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("readMkRoutes from stdin: %v", err)
	}
	if got.MisskeyVersion != "2026.5.4" || len(got.Routes) != 1 {
		t.Errorf("decoded routes mismatch: %+v", got)
	}
}

func TestReadMkRoutesFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "routes.json")
	mustWrite(t, p, `{"misskeyVersion":"x","mkGoVersion":"y","routes":[{"method":"POST","path":"/api/foo"}]}`)
	got, err := readMkRoutes(p, strings.NewReader(""))
	if err != nil {
		t.Fatalf("readMkRoutes from file: %v", err)
	}
	if got.MkGoVersion != "y" || got.Routes[0].Path != "/api/foo" {
		t.Errorf("decoded file routes mismatch: %+v", got)
	}
}

func TestReadMkRoutesFileMissing(t *testing.T) {
	if _, err := readMkRoutes(filepath.Join(t.TempDir(), "missing.json"), nil); err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestReadMkRoutesInvalidJSON(t *testing.T) {
	if _, err := readMkRoutes("", strings.NewReader("not json")); err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

// runEnv builds the per-test directory layout used by the run-level tests:
//   - <tmp>/ts/meta.ts                  : single Misskey TS endpoint
//   - <tmp>/ApiServerService.ts         : fastify 直登録 endpoint の抽出元
//   - <tmp>/routes.json                 : DumpedRoutes payload
//   - <tmp>/out.md                      : expected markdown output target
func runEnv(t *testing.T, routes *DumpedRoutes) (tsDir, directPath, routesPath, outPath string) {
	t.Helper()
	root := t.TempDir()
	tsDir = filepath.Join(root, "ts")
	mustWrite(t, filepath.Join(tsDir, "meta.ts"), "")
	directPath = filepath.Join(root, "ApiServerService.ts")
	mustWrite(t, directPath, "fastify.post<{ Body: { code: string; } }>('/signup-pending', h);\n")
	routesPath = filepath.Join(root, "routes.json")
	raw, err := json.Marshal(routes)
	if err != nil {
		t.Fatalf("marshal routes: %v", err)
	}
	if err := os.WriteFile(routesPath, raw, 0o644); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	outPath = filepath.Join(root, "out.md")
	return tsDir, directPath, routesPath, outPath
}

func TestRunWritesMarkdownToOutFile(t *testing.T) {
	tsDir, directPath, routesPath, outPath := runEnv(t, &DumpedRoutes{
		MisskeyVersion: "ver-ts",
		MkGoVersion:    "ver-mk",
		Routes: []DumpedRoute{
			{Method: "POST", Path: "/api/meta"},
		},
	})

	err := run(
		[]string{"-ts-endpoints-dir", tsDir, "-ts-api-server-service", directPath, "-mk-routes", routesPath, "-out", outPath},
		nil,
		io.Discard,
		io.Discard,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	b, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	out := string(b)
	for _, want := range []string{
		"Misskey (TS) version: `ver-ts`",
		"mk-go version: `ver-mk`",
		"/api/meta",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("matrix missing %q\nfull:\n%s", want, out)
		}
	}
}

func TestRunWritesToStdoutWhenOutNotGiven(t *testing.T) {
	tsDir, directPath, routesPath, _ := runEnv(t, &DumpedRoutes{
		MisskeyVersion: "x",
		MkGoVersion:    "y",
		Routes:         []DumpedRoute{{Method: "POST", Path: "/api/meta"}},
	})

	var stdout bytes.Buffer
	if err := run(
		[]string{"-ts-endpoints-dir", tsDir, "-ts-api-server-service", directPath, "-mk-routes", routesPath},
		nil,
		&stdout,
		io.Discard,
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "API Compatibility Matrix") {
		t.Errorf("stdout missing matrix header:\n%s", stdout.String())
	}
}

func TestRunUsesStdinForRoutes(t *testing.T) {
	root := t.TempDir()
	tsDir := filepath.Join(root, "ts")
	mustWrite(t, filepath.Join(tsDir, "meta.ts"), "")
	directPath := filepath.Join(root, "ApiServerService.ts")
	mustWrite(t, directPath, "fastify.post('/signup', h);\n")
	raw, err := json.Marshal(&DumpedRoutes{
		MisskeyVersion: "x", MkGoVersion: "y",
		Routes: []DumpedRoute{{Method: "POST", Path: "/api/meta"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var stdout bytes.Buffer
	if err := run(
		[]string{"-ts-endpoints-dir", tsDir, "-ts-api-server-service", directPath},
		bytes.NewReader(raw),
		&stdout,
		io.Discard,
	); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "/api/meta") {
		t.Errorf("stdout missing /api/meta:\n%s", stdout.String())
	}
}

func TestRunErrorsOnMissingTSDir(t *testing.T) {
	root := t.TempDir()
	routesPath := filepath.Join(root, "routes.json")
	mustWrite(t, routesPath, `{"routes":[]}`)
	directPath := filepath.Join(root, "ApiServerService.ts")
	mustWrite(t, directPath, "fastify.post('/signup', h);\n")
	err := run(
		[]string{"-ts-endpoints-dir", filepath.Join(root, "does-not-exist"), "-ts-api-server-service", directPath, "-mk-routes", routesPath},
		nil,
		io.Discard,
		io.Discard,
	)
	if err == nil {
		t.Fatal("expected error for missing ts dir, got nil")
	}
	// 本家が無いときは、取得の案内を出して読む前に落とす (#3378)。
	if !errors.Is(err, upstreamsrc.ErrNotFetched) || !strings.Contains(err.Error(), "make upstream-fetch") {
		t.Errorf("error should be ErrNotFetched with the fetch hint, got: %v", err)
	}
}

func TestRunErrorsOnMissingRoutesFile(t *testing.T) {
	tsDir := t.TempDir()
	directPath := filepath.Join(t.TempDir(), "ApiServerService.ts")
	mustWrite(t, directPath, "fastify.post('/signup', h);\n")
	err := run(
		[]string{"-ts-endpoints-dir", tsDir, "-ts-api-server-service", directPath, "-mk-routes", filepath.Join(t.TempDir(), "missing.json")},
		nil,
		io.Discard,
		io.Discard,
	)
	if err == nil {
		t.Fatal("expected error for missing routes file, got nil")
	}
	if !strings.Contains(err.Error(), "read mk-go routes") {
		t.Errorf("error should mention read mk-go routes, got: %v", err)
	}
}

func TestRunErrorsOnInvalidOutPath(t *testing.T) {
	tsDir, directPath, routesPath, _ := runEnv(t, &DumpedRoutes{
		Routes: []DumpedRoute{{Method: "POST", Path: "/api/meta"}},
	})
	// 存在しないディレクトリ下への出力は os.Create が失敗する。
	badOut := filepath.Join(t.TempDir(), "no-such-dir", "out.md")
	err := run(
		[]string{"-ts-endpoints-dir", tsDir, "-ts-api-server-service", directPath, "-mk-routes", routesPath, "-out", badOut},
		nil,
		io.Discard,
		io.Discard,
	)
	if err == nil {
		t.Fatal("expected error for unwritable out path, got nil")
	}
	if !strings.Contains(err.Error(), "open out") {
		t.Errorf("error should mention open out, got: %v", err)
	}
}

func TestRunErrorsOnBadFlag(t *testing.T) {
	// 未知の flag を渡すと flag.ContinueOnError + Parse error として返る。
	err := run([]string{"-no-such-flag"}, nil, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "parse args") {
		t.Errorf("error should mention parse args, got: %v", err)
	}
}

func TestRenderMkOnlyCategorizedEmptyCategory(t *testing.T) {
	// CatChatExt と CatOther のみで CatGETVariant が空 → 空 section も
	// "(なし)" 表記で出力されることを担保 (renderMkOnlyCategorized の
	// `if len(items) == 0` 分岐をカバーする)。
	var b strings.Builder
	renderMkOnlyCategorized(&b, []MkOnlyRoute{
		{Method: "POST", Path: "/api/chat/msg", Category: CatChatExt},
	})
	out := b.String()
	for _, want := range []string{
		"### GET variant 追加 (0)",
		"(なし)",
		"### cherrypick 系 chat 拡張 (1)",
		"/api/chat/msg",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}
