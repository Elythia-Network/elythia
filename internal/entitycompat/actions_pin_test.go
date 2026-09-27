package entitycompat

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

var (
	// pinnedActionRe accepts `owner/repo[/path]@<40 hex>`.
	pinnedActionRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)

	// pinnedDockerRe accepts `docker://<image>[:tag]@sha256:<64 hex>`.
	//
	// **docker の action は image の digest が固定になる。** tag だけ
	// (`docker://alpine:3.21`) だと action の tag と同じく付け替えられる。
	pinnedDockerRe = regexp.MustCompile(`^docker://[^@\s]+@sha256:[0-9a-f]{64}$`)

	// versionCommentRe is the `# vX.Y.Z` comment required next to a SHA.
	//
	// **版のコメントも要求する。** SHA だけだと「どの版を固定したのか」が
	// 読めず、dependabot の PR も SHA の差分しか示さないので、更新時に
	// リリースノートと突き合わせられない。
	//
	// **SHA とコメントの版が対応しているかは見ない。** 対応を確かめるには
	// その repository の tag を解く (`git ls-remote`) 必要があり、ネットワークを
	// 使わない `make gates` では判定できない。コメントは読む人向けの注記で、
	// 固定そのものは SHA が担う。
	versionCommentRe = regexp.MustCompile(`^#\s*v\d+\.\d+\.\d+\S*$`)
)

// TestWorkflowActionsArePinnedToSHA checks that every third-party action and
// reusable workflow referenced from .github/workflows is pinned to a full
// commit SHA with a `# vX.Y.Z` comment (or, for `docker://`, to a digest).
//
// **tag は付け替えられる。** `actions/checkout@v7` のような参照は、その
// リポジトリ (あるいは乗っ取った第三者) が tag を別 commit へ動かした瞬間に
// 中身が変わる。`docker.yml` / `build-with-plugins.yml` は `packages: write`
// を持って GHCR へ publish するので、そこで動く action が差し替わると配布
// image そのものを書き換えられる。**GitHub 公式 (`actions/*`) も例外にしない**
// — 例外にすると「どれが例外か」を読む側が毎回判断することになる。
//
// 更新は dependabot (`.github/dependabot.yml` の `github-actions`) に任せる。
// 手で上げるときの手順は docs/ci.md。
//
// **拾えなかったら落とす。** 書式が変わって `uses:` を 1 つも拾えないと、
// 検査していないのに緑になる (compose-check と同じ判断)。
func TestWorkflowActionsArePinnedToSHA(t *testing.T) {
	root := repoRoot(t)

	cmd := exec.Command("git", "-C", root, "ls-files", "--",
		".github/workflows/*.yml", ".github/workflows/*.yaml",
		".github/actions/*/action.yml", ".github/actions/*/action.yaml")
	cmd.Env = filterGitEnv(os.Environ())
	out, err := cmd.Output()
	require.NoErrorf(t, err, "git ls-files に失敗した: %v", err)

	files := strings.Fields(string(out))
	require.NotEmpty(t, files, ".github/workflows に tracked な workflow が 1 つも無い")

	checked := 0
	for _, path := range files {
		uses, err := remoteUses(readRepoFile(t, path))
		require.NoErrorf(t, err, "%s", path)
		checked += len(uses)
		for _, u := range uses {
			if isPinnedUse(u) {
				continue
			}
			assert.Failf(t, "action が commit SHA で固定されていない",
				"%s:%d: uses: %s %s\n"+
					"`owner/repo@<40 桁の commit SHA> # vX.Y.Z` (docker は `docker://<image>@sha256:<digest>`) の形で書くこと。\n"+
					"SHA は `git ls-remote https://github.com/<owner>/<repo> 'refs/tags/<tag>*'` で解く\n"+
					"(annotated tag は `^{}` の行が commit)。手順は docs/ci.md。",
				path, u.line, u.value, u.comment)
		}
	}
	require.NotZerof(t, checked,
		"workflow から外部の `uses:` を 1 つも拾えませんでした。書式が変わったならこのゲートも直すこと")
}

// remoteUse is one `uses:` reference that points outside the repository.
type remoteUse struct {
	line    int
	value   string
	comment string
}

// remoteUses parses body as YAML and returns every `uses:` value that points
// outside the repository.
//
// **YAML パーサで読む。** 行ごとの正規表現だと flow 形式
// (`- {uses: owner/repo@v1}`) や値を次の行に置く書き方を取りこぼし、しかも
// 取りこぼしは「検査していないのに緑」になる。`uses` を全ての mapping から
// 探すので、step (`jobs.*.steps[*]`)・job 直下の reusable workflow 呼び出し・
// composite action の `runs.steps[*]` のどれも拾う。コメントアウトした例示は
// パーサが読まないので自然に外れる。
func remoteUses(body string) ([]remoteUse, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(body), &root); err != nil {
		return nil, fmt.Errorf("parse workflow: %w", err)
	}
	var out []remoteUse
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if k.Value != "uses" || v.Kind != yaml.ScalarNode {
					continue
				}
				// 同じリポジトリの reusable workflow / composite action は SHA 固定の
				// 対象外 (checkout した commit そのものを使う)。
				if strings.HasPrefix(v.Value, "./") {
					continue
				}
				comment := v.LineComment
				// `- {uses: owner/repo@<sha>} # v1.2.3` のコメントは値ではなく
				// flow mapping の側に付く。
				if comment == "" && n.Style&yaml.FlowStyle != 0 {
					comment = n.LineComment
				}
				out = append(out, remoteUse{line: v.Line, value: v.Value, comment: strings.TrimSpace(comment)})
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	return out, nil
}

// isPinnedUse reports whether u is pinned: an action or reusable workflow at a
// full commit SHA with a version comment, or a docker image at a digest.
func isPinnedUse(u remoteUse) bool {
	if strings.HasPrefix(u.value, "docker://") {
		return pinnedDockerRe.MatchString(u.value)
	}
	return pinnedActionRe.MatchString(u.value) && versionCommentRe.MatchString(u.comment)
}

// TestRemoteUsesPinning pins the scanner with synthetic workflows.
//
// **実ファイルは常に全件固定済み**なので、実ファイルだけを見ていると検出の枝が
// 一度も実行されない (#2792 の secretfield gate と同じ判断)。
func TestRemoteUsesPinning(t *testing.T) {
	const sha = "3d3c42e5aac5ba805825da76410c181273ba90b1"
	digest := "sha256:" + strings.Repeat("ab", 32)
	step := func(s string) string { return "jobs:\n  a:\n    steps:\n" + s + "\n" }
	tests := []struct {
		name    string
		body    string
		pinned  bool
		skipped bool
	}{
		{name: "pinned step", body: step("      - uses: actions/checkout@" + sha + " # v7.0.1"), pinned: true},
		{name: "pinned with key", body: step("      - name: x\n        uses: docker/login-action@" + sha + " # v4.6.0"), pinned: true},
		{name: "pinned sub-path", body: step("      - uses: github/codeql-action/init@" + sha + " # v4.38.2"), pinned: true},
		{name: "pinned reusable workflow", body: "jobs:\n  a:\n    uses: owner/repo/.github/workflows/x.yml@" + sha + " # v1.2.3\n", pinned: true},
		{name: "quoted", body: step(`      - uses: "actions/checkout@` + sha + `" # v7.0.1`), pinned: true},
		{name: "flow mapping", body: step("      - {uses: actions/checkout@" + sha + ", with: {x: 1}} # v7.0.1"), pinned: true},
		{name: "value on next line", body: step("      - uses:\n          actions/checkout@" + sha + " # v7.0.1"), pinned: true},
		{name: "composite action", body: "runs:\n  using: composite\n  steps:\n    - uses: actions/checkout@" + sha + " # v7.0.1\n", pinned: true},
		{name: "docker digest", body: step("      - uses: docker://alpine:3.21@" + digest), pinned: true},
		{name: "docker digest without tag", body: step("      - uses: docker://ghcr.io/o/img@" + digest), pinned: true},
		{name: "major tag", body: step("      - uses: actions/checkout@v7")},
		{name: "full tag", body: step("      - uses: actions/checkout@v7.0.1")},
		{name: "branch", body: "jobs:\n  a:\n    uses: owner/repo/.github/workflows/x.yml@main\n"},
		{name: "sha without comment", body: step("      - uses: actions/checkout@" + sha)},
		{name: "comment on the following line", body: step("      - uses: actions/checkout@" + sha + "\n        # v7.0.1")},
		{name: "short sha", body: step("      - uses: actions/checkout@3d3c42e # v7.0.1")},
		{name: "comment is not a version", body: step("      - uses: actions/checkout@" + sha + " # latest")},
		{name: "flow mapping with tag", body: step("      - {uses: actions/checkout@v7}")},
		{name: "tag on next line", body: step("      - uses:\n          actions/checkout@v7")},
		{name: "docker tag", body: step("      - uses: docker://alpine:3.21")},
		{name: "docker short digest", body: step("      - uses: docker://alpine@sha256:abcd")},
		{name: "local action", body: step("      - uses: ./.github/actions/setup"), skipped: true},
		{name: "local reusable workflow", body: "jobs:\n  a:\n    uses: ./.github/workflows/build-with-plugins.yml\n", skipped: true},
		{name: "commented example", body: "#       uses: shiroha-a/mk/.github/workflows/build-with-plugins.yml@main\njobs: {}\n", skipped: true},
		{name: "not a uses key", body: step("      - with: actions/checkout@v7"), skipped: true},
		{name: "uses inside run text", body: step("      - run: |\n          echo 'uses: actions/checkout@v7'"), skipped: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uses, err := remoteUses(tt.body)
			require.NoError(t, err)
			if tt.skipped {
				require.Empty(t, uses)
				return
			}
			require.Len(t, uses, 1)
			require.Equal(t, tt.pinned, isPinnedUse(uses[0]), "%+v", uses[0])
		})
	}

	_, err := remoteUses("jobs: [\n")
	require.Error(t, err, "壊れた YAML は検査できないので落とす")
}
