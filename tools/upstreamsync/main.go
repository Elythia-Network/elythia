// Command upstreamsync applies the frontend part of an upstream Misskey
// release diff to frontend/ (design D4 in docs/design/project-restructure.md,
// #3379).
//
// frontend/ is a snapshot of the upstream monorepo without packages/backend,
// carrying mk-go's own changes. Following a new upstream release means taking
// the upstream diff between two release tags, keeping only the paths that
// frontend/ imports, remapping the two asset directories, and applying it with
// a 3-way merge so that mk-go's changes survive and conflicts are left as
// conflict markers per file.
//
// Usage (normally via `make upstream-sync TO=<version>`):
//
//	go run ./tools/upstreamsync -from 2026.10.0 -to 2026.11.0 -source .cache/misskey/mirror.git
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
)

// class is how one upstream path is treated when following a release.
type class int

const (
	// classImport is applied to frontend/<same path>.
	classImport class = iota
	// classBackendAssets is packages/backend/assets/, applied to frontend/assets/ (D3).
	classBackendAssets
	// classRepoAssets is the upstream root assets/, applied to frontend/repo-assets/ (D3).
	classRepoAssets
	// classLock is pnpm-lock.yaml, regenerated instead of applied (D4).
	classLock
	// classSkip is not imported into frontend/ at all (D1).
	classSkip
	// classUnknown matches none of the above and stops the sync.
	classUnknown
)

func (c class) String() string {
	switch c {
	case classImport:
		return "取り込む (frontend/)"
	case classBackendAssets:
		return "付け替える (frontend/assets/)"
	case classRepoAssets:
		return "付け替える (frontend/repo-assets/)"
	case classLock:
		return "作り直す (pnpm-lock.yaml)"
	case classSkip:
		return "取り込まない"
	default:
		return "分類できない"
	}
}

// D1 の表 (docs/design/project-restructure.md) をそのまま写したもの。表を変えたら
// ここも変える。
var (
	importFiles = map[string]bool{
		"package.json": true, "pnpm-workspace.yaml": true, ".node-version": true,
		"LICENSE": true, "COPYING": true, ".gitattributes": true, ".gitignore": true,
	}
	importDirs = []string{"patches/", "scripts/", "locales/", "packages/", "packages-private/"}
	skipFiles  = map[string]bool{
		"Dockerfile": true, "Dockerfile.assets": true, "healthcheck.sh": true, "Procfile": true,
		".dockerignore": true, ".dockleignore": true, ".gitmodules": true,
		"CHANGELOG.md": true, "CONTRIBUTING.md": true, "README.md": true, "CODE_OF_CONDUCT.md": true,
		"ROADMAP.md": true, "SECURITY.md": true, "AGENTS.md": true, "CLAUDE.md": true,
		".editorconfig": true, ".vsls.json": true, ".coderabbit.yaml": true,
		"codecov.yml": true, "crowdin.yml": true, "renovate.json5": true,
	}
	skipDirs = []string{".github/", ".config/", ".agents/", ".claude/", ".vscode/", ".devcontainer/", "idea/"}
	// compose.local-db.yml / compose_example.yml のような直下の compose。
	skipComposeRe = regexp.MustCompile(`^compose[^/]*\.ya?ml$`)
	// 版は tag 名としてそのまま git に渡すので、オプションや refspec として
	// 解釈されうる文字を通さない。
	versionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
)

// classify returns how the upstream path p (relative to the upstream root) is
// treated.
//
// **どれにも当たらないパスは classUnknown にする。** 「取り込む」で絞るだけだと、
// 本家が直下に新しく足したファイルが黙って落ちる (D4)。
func classify(p string) class {
	switch {
	case strings.Contains("/"+p, "/.vscode/"):
		// 本体の .gitignore が深さに関係なく .vscode/ を外しているので、frontend/ の
		// 中のもの (packages/frontend/.vscode/ など) も追跡していない。当てようとすると
		// 「index に無い」で pass ごと失敗する。
		return classSkip
	case strings.HasPrefix(p, "packages/backend/assets/"):
		return classBackendAssets
	case strings.HasPrefix(p, "packages/backend/"):
		return classSkip
	case strings.HasPrefix(p, "assets/"):
		return classRepoAssets
	case p == "pnpm-lock.yaml":
		return classLock
	case importFiles[p], hasAnyPrefix(p, importDirs):
		return classImport
	case skipFiles[p], hasAnyPrefix(p, skipDirs), skipComposeRe.MatchString(p):
		return classSkip
	}
	return classUnknown
}

func hasAnyPrefix(p string, prefixes []string) bool {
	for _, pre := range prefixes {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// change is one path in the upstream diff.
type change struct {
	path string
	// gitlink は submodule の pointer の変更。frontend/ へ当てると gitlink が
	// 入ってしまうので、分類に関わらず止める。
	gitlink bool
}

// pass is one `git apply` invocation: the upstream paths it covers and how the
// paths are rewritten into frontend/.
type pass struct {
	name      string
	paths     []string
	strip     int
	directory string
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "upstreamsync:", err)
		os.Exit(1)
	}
}

// run fetches both upstream tags into the repository, classifies the diff and,
// unless -dry-run, applies it to frontend/.
func run(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("upstreamsync", flag.ContinueOnError)
	fs.SetOutput(errOut)
	from := fs.String("from", "", "the upstream version frontend/ currently follows (UPSTREAM_MISSKEY_VERSION)")
	to := fs.String("to", "", "the upstream version to follow")
	source := fs.String("source", "", "a git repository that has both upstream tags (the bare mirror)")
	root := fs.String("root", ".", "repository root that owns frontend/")
	dryRun := fs.Bool("dry-run", false, "classify the diff only; do not apply")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !versionRe.MatchString(*from) || !versionRe.MatchString(*to) {
		return fmt.Errorf("-from と -to に本家の版を指定する (from=%q to=%q)", *from, *to)
	}
	if *from == *to {
		return fmt.Errorf("-from と -to が同じ (%s)", *from)
	}
	if *source == "" {
		return errors.New("-source に本家の tag を持つリポジトリを指定する (make upstream-sync が mirror を渡す)")
	}

	g := gitRunner{dir: *root}
	// 本家の objects を本体のリポジトリへ取る。--3way は当てる前の blob を手元で
	// 探すので、差分を mirror で作るだけでは足りない。ref は branch を作らずに
	// refs/upstream/ の下へ置く (push の対象にならない)。
	fromRef, toRef := "refs/upstream/"+*from, "refs/upstream/"+*to
	for _, v := range []string{*from, *to} {
		if _, err := g.output("fetch", "--quiet", "--no-tags", *source, "+refs/tags/"+v+":refs/upstream/"+v); err != nil {
			return fmt.Errorf("本家の tag %s を %s から取れない: %w", v, *source, err)
		}
	}

	changes, err := diffChanges(g, fromRef, toRef)
	if err != nil {
		return err
	}
	byClass := map[class][]string{}
	for _, c := range changes {
		cl := classify(c.path)
		if c.gitlink {
			cl = classUnknown
		}
		byClass[cl] = append(byClass[cl], c.path)
	}
	fmt.Fprintf(out, "本家 %s → %s: 変更されたパス %d 件\n", *from, *to, len(changes))
	for _, cl := range []class{classImport, classBackendAssets, classRepoAssets, classLock, classSkip, classUnknown} {
		if n := len(byClass[cl]); n > 0 {
			fmt.Fprintf(out, "  %-36s %d 件\n", cl, n)
		}
	}
	if unknown := byClass[classUnknown]; len(unknown) > 0 {
		return fmt.Errorf("D1 の区分に当たらないパスがある (submodule の変更を含む)。区分を決めて tools/upstreamsync と設計 D1 の表に足してからやり直す:\n  %s",
			strings.Join(unknown, "\n  "))
	}
	if *dryRun {
		return nil
	}

	// 衝突をファイル単位で解くので、当てる前の frontend/ は綺麗でなければならない。
	// 手を入れている最中の変更に本家の差分が混ざると、どこまでが本家か分からなくなる。
	if dirty, err := g.output("status", "--porcelain", "--", "frontend"); err != nil {
		return err
	} else if strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("frontend/ に commit していない変更がある。commit してからやり直す:\n%s", dirty)
	}

	passes := []pass{
		{name: "frontend/", paths: byClass[classImport], strip: 1, directory: "frontend"},
		{name: "frontend/assets/", paths: byClass[classBackendAssets], strip: 4, directory: "frontend/assets"},
		{name: "frontend/repo-assets/", paths: byClass[classRepoAssets], strip: 2, directory: "frontend/repo-assets"},
	}
	// **当てる前に全 pass を確かめる。** `git apply --3way` は、衝突ではない失敗
	// (frontend/ に無いファイルへの変更など) が 1 つでもあると、その pass を丸ごと
	// 取り消す。確かめずに当てると、前の pass だけが当たった半端な状態で止まる。
	// `--check` は衝突なら成功し、衝突でない失敗のときだけ落ちる (実測)。
	for _, p := range passes {
		if len(p.paths) == 0 {
			continue
		}
		if err := applyPass(g, fromRef, toRef, p, true); err != nil {
			return fmt.Errorf("%s へ当てられない差分があるので、何も当てていない:\n  %s\n"+
				"考えられる原因:\n"+
				"  - この版を既に当てた後にやり直している (UPSTREAM_MISSKEY_VERSION を上げ忘れていないか)\n"+
				"  - mk-go が消したファイルや追跡していないファイルを本家が変えた (区分を見直すか、frontend/ を本家に揃えてからやり直す)",
				p.name, strings.Join(errorLines(err.Error()), "\n  "))
		}
	}

	var failed []string
	for _, p := range passes {
		if len(p.paths) == 0 {
			continue
		}
		if err := applyPass(g, fromRef, toRef, p, false); err != nil {
			fmt.Fprintf(errOut, "upstreamsync: %s へ当てるときに衝突した: %v\n", p.name, err)
			failed = append(failed, p.name)
		}
	}

	conflicts, err := g.output("diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "次にやること:")
	switch c := strings.TrimSpace(conflicts); {
	case c != "":
		fmt.Fprintf(out, "  1. 衝突したファイルを解いて git add する (テキストは衝突マーカー、バイナリはマーカー無しで mk-go 側が残る):\n     %s\n", strings.ReplaceAll(c, "\n", "\n     "))
	case len(failed) > 0:
		// 確かめた後に当てて失敗し、衝突も残っていない。想定していない状態なので、
		// 「衝突は無い」とは言わない。
		return fmt.Errorf("git apply が失敗したが衝突は残っていない (%s)。git status で状態を確かめる", strings.Join(failed, ", "))
	default:
		fmt.Fprintln(out, "  1. 衝突は無い。差分を目で確かめる (git diff --cached)")
	}
	fmt.Fprintln(out, "  2. make upstream-sync-lock で frontend/pnpm-lock.yaml を作り直し、package.json で変わった依存以外が動いていないか確かめる")
	fmt.Fprintf(out, "  3. UPSTREAM_MISSKEY_VERSION と config.MisskeyVersion などを %s に上げる (TestUpstreamVersionIsConsistent が揃っているかを見る)\n", *to)
	fmt.Fprintln(out, "  4. backend 側の変更を triage して Go に移植する (docs/upstream-catch-up.md)")
	if len(failed) > 0 || strings.TrimSpace(conflicts) != "" {
		return errors.New("衝突が残っている。上の 1. のファイルを解いてから先へ進む")
	}
	return nil
}

// diffChanges lists the paths that differ between the two upstream refs.
//
// `--no-renames` で rename を削除と追加として扱う。付け替えた先で rename の元の
// パスを探さずに済むようにするため (D4)。
func diffChanges(g gitRunner, fromRef, toRef string) ([]change, error) {
	raw, err := g.output("diff", "--raw", "-z", "--no-renames", "--no-abbrev", fromRef, toRef)
	if err != nil {
		return nil, err
	}
	// -z の --raw は「:<meta>\0<path>\0」の繰り返し。
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) < 2 {
			return nil, fmt.Errorf("git diff --raw の出力を読めない: %q", fields[i])
		}
		changes = append(changes, change{
			path:    fields[i+1],
			gitlink: meta[0] == "160000" || meta[1] == "160000",
		})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].path < changes[j].path })
	return changes, nil
}

// applyPass applies the upstream diff of p.paths to p.directory with a 3-way
// merge.
//
// `--directory` は 1 回に 1 つしか指定できないので、付け替え先ごとに分ける (D4)。
// パッチ側の `--no-renames` は念のための指定。pass の pathspec が rename の片側
// だけを含むと、git はその片側を追加か削除として出すので、壊れるのは一覧の側
// (diffChanges) で、そちらはテストが rename のケースで固定している。
// 衝突したファイルは衝突マーカー付きで作業ツリーに残り、index では unmerged に
// なる。
func applyPass(g gitRunner, fromRef, toRef string, p pass, check bool) error {
	patch, err := g.output(append([]string{"diff", "--binary", "--no-renames", fromRef, toRef, "--"}, pathspecs(p.paths)...)...)
	if err != nil {
		return err
	}
	args := []string{"apply", "--3way", fmt.Sprintf("-p%d", p.strip), "--directory=" + p.directory}
	if check {
		args = append(args, "--check")
	}
	_, err = g.run(strings.NewReader(patch), args...)
	return err
}

// errorLines picks git's `error:` lines out of a failed command's message.
//
// `git apply --check` は当たったファイルごとに「Applied patch ... cleanly」を出すので、
// 本家の 1 リリース分だと 100 行を超え、肝心の error: の行が埋もれる (レビューで実測)。
// error: の行が無ければ全体を返す。
func errorLines(msg string) []string {
	var out []string
	for _, l := range strings.Split(msg, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "error:") {
			out = append(out, strings.TrimSpace(l))
		}
	}
	if len(out) == 0 {
		return []string{msg}
	}
	return out
}

// pathspecs turns literal paths into pathspecs that git does not glob-expand.
func pathspecs(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = ":(literal)" + path.Clean(p)
	}
	return out
}

// gitRunner runs git in one repository.
type gitRunner struct {
	dir string
}

func (g gitRunner) output(args ...string) (string, error) {
	return g.run(nil, args...)
}

// run executes git with stdin and returns stdout.
//
// hook の中から呼ばれたときに export されている GIT_DIR などを外す。引き継ぐと
// -C で指したリポジトリではなく、呼び出し元のリポジトリを操作してしまう。
func (g gitRunner) run(stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...)
	// error: の行を拾う (errorLines) ので、git のメッセージを訳させない。de / fr など
	// では「Fehler:」のように接頭辞まで訳される。--porcelain と -z の出力は変わらない。
	cmd.Env = append(filterGitEnv(os.Environ()), "LC_ALL=C")
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w\n%s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func filterGitEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		switch {
		case strings.HasPrefix(kv, "GIT_DIR="),
			strings.HasPrefix(kv, "GIT_WORK_TREE="),
			strings.HasPrefix(kv, "GIT_INDEX_FILE="):
			continue
		}
		out = append(out, kv)
	}
	return out
}
