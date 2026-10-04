package entitycompat

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// testComposeUntrackedSources lists bind-mount sources in the test compose
// files that are not tracked by git, keyed by repo-relative path.
//
// 理由の欄には「なぜ許すか」ではなく「どの経路で作られるか」を書く。
// 使われなくなった項目は TestTestComposeUntrackedSourcesAreUsed が落とす。
//
// 今は空。結果の出力先 (`results/`) は中に追跡済みの `.gitignore` / `.gitkeep` を
// 置いているので、ここに足さなくても通る。
var testComposeUntrackedSources = map[string]string{}

// testComposePattern matches compose file names (base and overlays).
var testComposePattern = regexp.MustCompile(`^(docker-)?compose(\.[^/]+)?\.ya?ml$`)

// TestTestComposeFilesAreSelfContained guards the test compose files under
// tests/ (#3373).
//
// 検証用の compose は**ファイルの場所を基準に**相対パスを書く (`--project-directory`
// で基準を固定する案は採らなかった。付け忘れると bind mount が空のディレクトリとして
// 作られ、壊れ方が分かりにくい)。直下から tests/ へ移すと、書き換え漏れがそのまま
// この形で壊れるので、docker を使わずに次の 3 つを見る。
//
//   - top-level の `name:` があり、`mk` ではない。無いと project 名がディレクトリ名に
//     なり、直下で起動すれば本番の project `mk` に合流しうる (CLAUDE.md Section 0)
//   - overlay でない compose 同士で `name:` が重ならない。重なると片方の `down -v` が
//     もう片方の volume まで消す
//   - 同じディレクトリに `compose.yml` があるとき、他の `compose.*.yml` (overlay) の
//     `name:` はそれと同じ。違うと、どちらの値が勝つかを compose の実装に委ねる
//   - build context・dockerfile・bind mount の相対パスが、git で追跡されたファイルか
//     ディレクトリを指している
func TestTestComposeFilesAreSelfContained(t *testing.T) {
	root := repoRoot(t)
	tracked := gitTrackedSet(t, root)

	var files []string
	for p := range tracked {
		if strings.HasPrefix(p, "tests/") && testComposePattern.MatchString(path.Base(p)) {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	// **拾えなかったら落とす。** 実在する対象を名指しで要求する。
	for _, must := range []string{"tests/upstream-e2e/compose.yml", "tests/queue-bench/docker-compose.queue-bench.yml"} {
		require.Containsf(t, files, must, "検証用の compose の列挙が %s を拾えていない", must)
	}

	names := map[string]string{}
	for _, f := range files {
		doc := parseTestCompose(t, root, f)
		require.NotEmptyf(t, doc.Name, "%s に top-level の name: が無い", f)
		require.NotEqualf(t, "mk", doc.Name, "%s の name: が本番の project 名 mk と同じ", f)
		names[f] = doc.Name

		dir := path.Dir(f)
		for svc, s := range doc.Services {
			srcs, err := s.relativeSources()
			require.NoErrorf(t, err, "%s の service %s の build を読めない", f, svc)
			for _, src := range srcs {
				p := path.Clean(path.Join(dir, src))
				if _, ok := testComposeUntrackedSources[p]; ok {
					continue
				}
				require.Truef(t, isTrackedPath(tracked, p),
					"%s の service %s が参照する %s (%s) は git で追跡されていない", f, svc, src, p)
			}
		}
	}

	baseOf := map[string]string{}
	for _, f := range files {
		base := path.Join(path.Dir(f), "compose.yml")
		baseName, ok := names[base]
		if f == base || !ok || !strings.HasPrefix(path.Base(f), "compose.") {
			if prev, dup := baseOf[names[f]]; dup {
				t.Errorf("%s と %s が同じ name: %q を持つ", prev, f, names[f])
			}
			baseOf[names[f]] = f
			continue
		}
		require.Equalf(t, baseName, names[f], "overlay %s の name: がベース %s と違う", f, base)
	}
}

// TestTestComposeUntrackedSourcesAreUsed fails when an allowlist entry is no
// longer referenced by any test compose file.
func TestTestComposeUntrackedSourcesAreUsed(t *testing.T) {
	root := repoRoot(t)
	tracked := gitTrackedSet(t, root)
	used := map[string]bool{}
	for p := range tracked {
		if !strings.HasPrefix(p, "tests/") || !testComposePattern.MatchString(path.Base(p)) {
			continue
		}
		doc := parseTestCompose(t, root, p)
		for _, s := range doc.Services {
			srcs, _ := s.relativeSources()
			for _, src := range srcs {
				used[path.Clean(path.Join(path.Dir(p), src))] = true
			}
		}
	}
	for p := range testComposeUntrackedSources {
		require.Truef(t, used[p], "testComposeUntrackedSources の %s はどの compose からも参照されていない", p)
	}
}

type testComposeDoc struct {
	Name     string                        `yaml:"name"`
	Services map[string]testComposeService `yaml:"services"`
}

type testComposeService struct {
	Build   yaml.Node   `yaml:"build"`
	Volumes []yaml.Node `yaml:"volumes"`
}

// relativeSources returns the relative paths a service reads from the host:
// the build context, the dockerfile (relative to the context) and short or
// long-syntax bind mounts. Named volumes and absolute paths are skipped.
func (s testComposeService) relativeSources() ([]string, error) {
	var out []string
	switch s.Build.Kind {
	case yaml.ScalarNode:
		// overlay は `build: !reset null` でベースの build を外すことがある。
		if s.Build.Tag != "!!null" && s.Build.Tag != "!reset" && s.Build.Value != "" {
			out = append(out, s.Build.Value)
		}
	case yaml.MappingNode:
		var b struct {
			Context    string `yaml:"context"`
			Dockerfile string `yaml:"dockerfile"`
		}
		if err := s.Build.Decode(&b); err != nil {
			return nil, err
		}
		ctx := b.Context
		if ctx == "" {
			ctx = "."
		}
		out = append(out, ctx)
		if b.Dockerfile != "" {
			out = append(out, path.Join(ctx, b.Dockerfile))
		}
	}
	for _, v := range s.Volumes {
		var src string
		switch v.Kind {
		case yaml.ScalarNode:
			src, _, _ = strings.Cut(v.Value, ":")
		case yaml.MappingNode:
			var m struct {
				Type   string `yaml:"type"`
				Source string `yaml:"source"`
			}
			if err := v.Decode(&m); err == nil && m.Type == "bind" {
				src = m.Source
			}
		}
		if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") || src == "." || src == ".." {
			out = append(out, src)
		}
	}
	return out, nil
}

func parseTestCompose(t *testing.T, root, rel string) testComposeDoc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	require.NoErrorf(t, err, "read %s", rel)
	var doc testComposeDoc
	require.NoErrorf(t, yaml.Unmarshal(raw, &doc), "parse %s", rel)
	require.NotEmptyf(t, doc.Services, "%s から service を 1 つも拾えない", rel)
	return doc
}

// gitTrackedSet returns every path tracked by git, repo-relative with "/".
func gitTrackedSet(t *testing.T, root string) map[string]bool {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	cmd.Env = filterGitEnv(os.Environ())
	out, err := cmd.Output()
	require.NoErrorf(t, err, "git ls-files に失敗した: %v", err)
	set := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			set[p] = true
		}
	}
	require.NotEmpty(t, set, "git ls-files が空")
	return set
}

// isTrackedPath reports whether p is a tracked file or a directory that
// contains at least one tracked file.
func isTrackedPath(tracked map[string]bool, p string) bool {
	if p == "." || tracked[p] {
		return true
	}
	prefix := p + "/"
	for f := range tracked {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}

// operatorComposeFiles are the compose files distributed to operators. They
// are the only compose files allowed at the repository root.
var operatorComposeFiles = []string{
	"compose.uds.yaml.example",
	"docker-compose.image.yml",
	"docker-compose.yml",
}

// rootComposePattern matches compose files (and their templates) at the
// repository root.
var rootComposePattern = regexp.MustCompile(`^(docker-)?compose[^/]*\.ya?ml(\.example)?$`)

// rootComposeFiles returns the compose files tracked at the repository root.
func rootComposeFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	for p := range gitTrackedSet(t, root) {
		if !strings.Contains(p, "/") && rootComposePattern.MatchString(p) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// TestRootComposeFilesAreOperatorOnly pins the repository root to the
// operator-facing compose files (#3373).
//
// 検証用の compose を直下に置くと、`name:` を忘れたときに project 名が
// ディレクトリ名 (`mk`) になり、このリポジトリで動いている本番に合流しうる
// (CLAUDE.md Section 0)。検証用は tests/<スイート>/ に置き、
// TestTestComposeFilesAreSelfContained の対象にする。
func TestRootComposeFilesAreOperatorOnly(t *testing.T) {
	require.Equal(t, operatorComposeFiles, rootComposeFiles(t, repoRoot(t)),
		"リポジトリ直下には運営者向けの compose だけを置く。検証用は tests/<スイート>/ へ")
}
