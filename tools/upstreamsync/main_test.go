package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		path string
		want class
	}{
		{"package.json", classImport},
		{"pnpm-workspace.yaml", classImport},
		{".node-version", classImport},
		{"LICENSE", classImport},
		{".gitignore", classImport},
		{"packages/frontend/src/main.ts", classImport},
		{"packages/misskey-js/src/api.ts", classImport},
		{"packages-private/changelog-checker/package.json", classImport},
		{"locales/ja-JP.yml", classImport},
		{"scripts/build-assets.mjs", classImport},
		{"patches/foo.patch", classImport},
		{"packages/backend/assets/favicon.ico", classBackendAssets},
		{"packages/backend/src/server/api/endpoints/meta.ts", classSkip},
		{"packages/backend/package.json", classSkip},
		{"assets/ai.png", classRepoAssets},
		{"pnpm-lock.yaml", classLock},
		{".github/workflows/lint.yml", classSkip},
		{".config/example.yml", classSkip},
		{"Dockerfile", classSkip},
		{"README.md", classSkip},
		{"compose.local-db.yml", classSkip},
		{"compose_example.yml", classSkip},
		{"CLAUDE.md", classSkip},
		{"idea/README.md", classSkip},
		// 本体の .gitignore が深さに関係なく外しているので追跡していない。
		{"packages/frontend/.vscode/settings.json", classSkip},
		{".vscode/settings.json", classSkip},
		// 本家が直下に新しく足したものは、黙って落とさずに止める。
		{"newtool.config.ts", classUnknown},
		{"cypress/e2e/basic.cy.ts", classUnknown},
		// 接頭辞だけ似たパスを取り違えない。
		{"packagesx/a.ts", classUnknown},
		{"assetsx/a.png", classUnknown},
		{"composer.json", classUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			assert.Equal(t, tt.want, classify(tt.path))
		})
	}
}

func TestClassString(t *testing.T) {
	// 分類の一覧に出す名前。区別できないと、どの区分が何件か読めない。
	seen := map[string]bool{}
	for _, c := range []class{classImport, classBackendAssets, classRepoAssets, classLock, classSkip, classUnknown} {
		s := c.String()
		assert.NotEmpty(t, s)
		assert.Falsef(t, seen[s], "%q が重複している", s)
		seen[s] = true
	}
}

// upstreamV1 / upstreamV2 are the two upstream releases used by the run tests.
var (
	binV1 = []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0x02, 0xff}
	binV2 = []byte{0x89, 'P', 'N', 'G', 0x00, 0x09, 0x08, 0xfe, 0x00}

	upstreamV1 = map[string][]byte{
		"package.json":                    []byte("{\"name\":\"misskey\",\"version\":\"1.0.0\"}\n"),
		"packages/frontend/a.ts":          lines("a", 10),
		"packages/frontend/old.ts":        []byte("old\n"),
		"packages/frontend/moved.ts":      lines("m", 20),
		"packages/backend/src/x.ts":       []byte("backend v1\n"),
		"packages/backend/assets/fav.png": binV1,
		"assets/ai.png":                   binV1,
		"pnpm-lock.yaml":                  []byte("lock v1\n"),
		"README.md":                       []byte("readme v1\n"),
		".github/workflows/lint.yml":      []byte("on: push\n"),
	}
	upstreamV2 = map[string][]byte{
		"package.json":             []byte("{\"name\":\"misskey\",\"version\":\"2.0.0\"}\n"),
		"packages/frontend/a.ts":   replaceLine(lines("a", 10), 1, "a1 upstream"),
		"packages/frontend/new.ts": []byte("new\n"),
		// 内容を変えずに移したファイル。rename として扱うと 1 件の変更に見える。
		"packages/frontend/sub/moved.ts":  lines("m", 20),
		"packages/backend/src/x.ts":       []byte("backend v2\n"),
		"packages/backend/assets/fav.png": binV2,
		"assets/ai.png":                   binV2,
		"pnpm-lock.yaml":                  []byte("lock v2\n"),
		"README.md":                       []byte("readme v2\n"),
		".github/workflows/lint.yml":      []byte("on: pull_request\n"),
	}
)

func TestRun_AppliesImportedPathsAndRemapsAssets(t *testing.T) {
	upstream := newUpstream(t, upstreamV1, upstreamV2)
	// mk 側の独自変更 (10 行目) は、本家の変更 (1 行目) と重ならない。
	mk := newMK(t, map[string][]byte{
		"frontend/packages/frontend/a.ts": replaceLine(lines("a", 10), 10, "a10 mk-go"),
	})

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut)
	require.NoErrorf(t, err, "stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())

	a := readFile(t, mk, "frontend/packages/frontend/a.ts")
	assert.Contains(t, a, "a1 upstream", "本家の変更が当たる")
	assert.Contains(t, a, "a10 mk-go", "mk-go の変更が残る")
	assert.Equal(t, "new\n", readFile(t, mk, "frontend/packages/frontend/new.ts"), "本家が足したファイル")
	assert.NoFileExists(t, filepath.Join(mk, "frontend/packages/frontend/old.ts"), "本家が消したファイル")
	// rename は削除と追加として当たる。
	assert.NoFileExists(t, filepath.Join(mk, "frontend/packages/frontend/moved.ts"))
	assert.Equal(t, string(lines("m", 20)), readFile(t, mk, "frontend/packages/frontend/sub/moved.ts"))
	assert.Contains(t, readFile(t, mk, "frontend/package.json"), "2.0.0")

	// D3 の付け替え。バイナリも 3-way で当たる。
	assert.Equal(t, string(binV2), readFile(t, mk, "frontend/assets/fav.png"))
	assert.Equal(t, string(binV2), readFile(t, mk, "frontend/repo-assets/ai.png"))

	// lock は当てずに作り直す。取り込まないものは frontend/ に現れない。
	assert.Equal(t, "lock mk-go\n", readFile(t, mk, "frontend/pnpm-lock.yaml"))
	for _, p := range []string{"frontend/packages/backend", "frontend/README.md", "frontend/.github", "frontend/assets/fav.png.orig"} {
		assert.NoFileExists(t, filepath.Join(mk, p))
	}
	assert.NoDirExists(t, filepath.Join(mk, "frontend/packages/backend"))

	assert.Contains(t, out.String(), "変更されたパス 12 件")
	assert.Contains(t, out.String(), "衝突は無い")
	// branch や tag は作らず、refs/upstream/ の下にだけ置く。
	refs := git(t, mk, "for-each-ref", "--format=%(refname)")
	assert.Contains(t, refs, "refs/upstream/1.0.0")
	assert.Contains(t, refs, "refs/upstream/2.0.0")
	assert.NotContains(t, refs, "refs/tags/")
}

func TestRun_LeavesConflictMarkers(t *testing.T) {
	upstream := newUpstream(t, upstreamV1, upstreamV2)
	// 本家と同じ 1 行目を mk-go も変えている。
	mk := newMK(t, map[string][]byte{
		"frontend/packages/frontend/a.ts": replaceLine(lines("a", 10), 1, "a1 mk-go"),
	})

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "衝突が残っている")
	a := readFile(t, mk, "frontend/packages/frontend/a.ts")
	assert.Contains(t, a, "<<<<<<<")
	assert.Contains(t, a, "a1 mk-go")
	assert.Contains(t, a, "a1 upstream")
	assert.Contains(t, out.String(), "frontend/packages/frontend/a.ts", "衝突したファイルを案内する")
	// 衝突しなかった付け替え先は当たっている。
	assert.Equal(t, string(binV2), readFile(t, mk, "frontend/assets/fav.png"))
}

// mk-go が消したファイルを本家が変えた場合。`git apply --3way` はその pass を丸ごと
// 取り消すので、確かめずに当てると、ほかの pass だけが当たった半端な状態になる。
func TestRun_AppliesNothingWhenAPassCannotApply(t *testing.T) {
	v2 := clone(upstreamV2)
	v2["packages/frontend/old.ts"] = []byte("old changed upstream\n")
	upstream := newUpstream(t, upstreamV1, v2)
	mk := newMK(t, nil)
	require.NoError(t, os.Remove(filepath.Join(mk, "frontend/packages/frontend/old.ts")))
	commitAll(t, mk, "mk-go removes old.ts")

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "何も当てていない")
	// git の error: の行と、考えられる原因を出す。check が出す「Applied patch」の行は
	// 混ぜない (1 リリース分だと 100 行を超えて error: が埋もれる)。
	assert.Contains(t, err.Error(), "error:")
	assert.Contains(t, err.Error(), "UPSTREAM_MISSKEY_VERSION")
	assert.NotContains(t, err.Error(), "Applied patch")
	assert.NotContains(t, out.String(), "衝突は無い")
	// 後ろの pass (付け替え先) も当てていない。
	assert.Equal(t, string(binV1), readFile(t, mk, "frontend/assets/fav.png"))
	assert.Empty(t, strings.TrimSpace(git(t, mk, "status", "--porcelain")))
}

// 後ろの pass (frontend/repo-assets/) だけが当たらない場合も、前の pass を当てない。
func TestRun_AppliesNothingWhenALaterPassCannotApply(t *testing.T) {
	upstream := newUpstream(t, upstreamV1, upstreamV2)
	mk := newMK(t, nil)
	require.NoError(t, os.Remove(filepath.Join(mk, "frontend/repo-assets/ai.png")))
	commitAll(t, mk, "mk-go removes ai.png")

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "frontend/repo-assets/")
	assert.NotContains(t, readFile(t, mk, "frontend/package.json"), "2.0.0", "前の pass も当てていない")
	assert.Empty(t, strings.TrimSpace(git(t, mk, "status", "--porcelain")))
}

// 付け替え先に変更が無い (pass が空になる) 版。空の pass を飛ばさないと、pathspec の
// 無い git diff (= 本家の全差分) を付け替え先へ当ててしまう。
func TestRun_SkipsEmptyPasses(t *testing.T) {
	v2 := clone(upstreamV2)
	v2["packages/backend/assets/fav.png"] = binV1
	v2["assets/ai.png"] = binV1
	upstream := newUpstream(t, upstreamV1, v2)
	mk := newMK(t, nil)

	var out, errOut bytes.Buffer
	require.NoErrorf(t, run([]string{"-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut),
		"stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	assert.Contains(t, readFile(t, mk, "frontend/package.json"), "2.0.0")
	assert.NoFileExists(t, filepath.Join(mk, "frontend/assets/package.json"))
	assert.NoFileExists(t, filepath.Join(mk, "frontend/repo-assets/package.json"))
	assert.NotContains(t, out.String(), "付け替える")
}

func TestErrorLines(t *testing.T) {
	assert.Equal(t, []string{"error: a", "error: b"}, errorLines("Applied patch x cleanly.\nerror: a\n  error: b\n"))
	assert.Equal(t, []string{"something else"}, errorLines("something else"))
}

func TestRun_StopsOnUnclassifiedPath(t *testing.T) {
	v2 := clone(upstreamV2)
	v2["newtool.config.ts"] = []byte("export {}\n")
	upstream := newUpstream(t, upstreamV1, v2)
	mk := newMK(t, nil)

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newtool.config.ts")
	// 何も当てない。
	assert.NotContains(t, readFile(t, mk, "frontend/package.json"), "2.0.0")
	assert.Empty(t, strings.TrimSpace(git(t, mk, "status", "--porcelain")))
}

func TestRun_StopsOnUpstreamSubmodule(t *testing.T) {
	upstream := newUpstream(t, upstreamV1, upstreamV2)
	// 2.0.0 の上に gitlink を足した 2.0.1 を作る。
	sha := strings.TrimSpace(git(t, upstream, "rev-parse", "2.0.0"))
	git(t, upstream, "update-index", "--add", "--cacheinfo", "160000,"+sha+",packages/frontend/fluent-emojis")
	git(t, upstream, "commit", "-q", "-m", "gitlink")
	git(t, upstream, "tag", "2.0.1")
	mk := newMK(t, nil)

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "2.0.0", "-to", "2.0.1", "-source", upstream, "-root", mk}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "packages/frontend/fluent-emojis")
}

func TestRun_DryRunOnlyClassifies(t *testing.T) {
	upstream := newUpstream(t, upstreamV1, upstreamV2)
	mk := newMK(t, nil)

	var out, errOut bytes.Buffer
	require.NoError(t, run([]string{"-dry-run", "-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut))
	assert.Contains(t, out.String(), "取り込む (frontend/)")
	assert.Contains(t, out.String(), "作り直す (pnpm-lock.yaml)")
	assert.NotContains(t, out.String(), "次にやること")
	assert.NotContains(t, readFile(t, mk, "frontend/package.json"), "2.0.0")
}

func TestRun_RefusesDirtyFrontend(t *testing.T) {
	upstream := newUpstream(t, upstreamV1, upstreamV2)
	mk := newMK(t, nil)
	require.NoError(t, os.WriteFile(filepath.Join(mk, "frontend/package.json"), []byte("{}\n"), 0o644))

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "1.0.0", "-to", "2.0.0", "-source", upstream, "-root", mk}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "commit していない変更")
	assert.Equal(t, "{}\n", readFile(t, mk, "frontend/package.json"))
}

func TestRun_MissingTag(t *testing.T) {
	upstream := newUpstream(t, upstreamV1, upstreamV2)
	mk := newMK(t, nil)

	var out, errOut bytes.Buffer
	err := run([]string{"-from", "1.0.0", "-to", "9.9.9", "-source", upstream, "-root", mk}, &out, &errOut)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "9.9.9")
}

func TestRun_RejectsBadArguments(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing versions", []string{"-source", "x"}, "本家の版"},
		{"option-like version", []string{"-from", "-x", "-to", "2.0.0", "-source", "x"}, "本家の版"},
		{"refspec-like version", []string{"-from", "1.0.0:refs/heads/main", "-to", "2.0.0", "-source", "x"}, "本家の版"},
		{"same versions", []string{"-from", "1.0.0", "-to", "1.0.0", "-source", "x"}, "同じ"},
		{"missing source", []string{"-from", "1.0.0", "-to", "2.0.0"}, "-source"},
		{"unknown flag", []string{"-nope"}, "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			err := run(tt.args, &out, &errOut)
			require.Error(t, err)
			assert.Contains(t, err.Error()+errOut.String(), tt.want)
		})
	}
}

func TestPathspecsAreLiteral(t *testing.T) {
	assert.Equal(t, []string{":(literal)a/*.ts", ":(literal)b"}, pathspecs([]string{"a/*.ts", "./b"}))
}

func TestFilterGitEnv(t *testing.T) {
	got := filterGitEnv([]string{"GIT_DIR=/x", "GIT_WORK_TREE=/y", "GIT_INDEX_FILE=/z", "HOME=/h"})
	assert.Equal(t, []string{"HOME=/h"}, got)
}

// newUpstream creates a fake upstream repository with tags 1.0.0 and 2.0.0.
func newUpstream(t *testing.T, v1, v2 map[string][]byte) string {
	t.Helper()
	dir := newRepo(t)
	writeTree(t, dir, v1)
	commitAll(t, dir, "1.0.0")
	git(t, dir, "tag", "1.0.0")
	writeTree(t, dir, v2)
	commitAll(t, dir, "2.0.0")
	git(t, dir, "tag", "2.0.0")
	return dir
}

// newMK creates a repository whose frontend/ is upstreamV1 imported the way
// P4a did (D1 / D3), with mk-go's own changes in overrides.
func newMK(t *testing.T, overrides map[string][]byte) string {
	t.Helper()
	dir := newRepo(t)
	files := map[string][]byte{
		"go.mod":                              []byte("module example.com/mk\n"),
		"frontend/package.json":               upstreamV1["package.json"],
		"frontend/packages/frontend/a.ts":     upstreamV1["packages/frontend/a.ts"],
		"frontend/packages/frontend/old.ts":   upstreamV1["packages/frontend/old.ts"],
		"frontend/packages/frontend/moved.ts": upstreamV1["packages/frontend/moved.ts"],
		"frontend/assets/fav.png":             upstreamV1["packages/backend/assets/fav.png"],
		"frontend/repo-assets/ai.png":         upstreamV1["assets/ai.png"],
		"frontend/pnpm-lock.yaml":             []byte("lock mk-go\n"),
	}
	for k, v := range overrides {
		files[k] = v
	}
	writeTree(t, dir, files)
	commitAll(t, dir, "import")
	return dir
}

func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git が要る")
	}
	// 手元の global 設定 (commit.gpgsign など) をテストに持ち込まない。
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	return dir
}

func writeTree(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	// 前の版にあって今回の版に無いファイルを消す (本家の削除を表す)。
	out := git(t, dir, "ls-files", "-z")
	for _, p := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if p == "" {
			continue
		}
		if _, ok := files[p]; !ok {
			require.NoError(t, os.Remove(filepath.Join(dir, p)))
		}
	}
	for p, b := range files {
		full := filepath.Join(dir, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, b, 0o644))
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", msg)
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(filterGitEnv(os.Environ()), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
	return string(out)
}

func readFile(t *testing.T, dir, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, p))
	require.NoError(t, err)
	return string(b)
}

func lines(prefix string, n int) []byte {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		b.WriteString(prefix)
		b.WriteString(strings.Repeat("x", i))
		b.WriteString("\n")
	}
	return []byte(b.String())
}

func replaceLine(b []byte, n int, s string) []byte {
	ls := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	ls[n-1] = s
	return []byte(strings.Join(ls, "\n") + "\n")
}

func clone(m map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
