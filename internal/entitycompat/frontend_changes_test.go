package entitycompat

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	frontendWorkflow          = ".github/workflows/frontend.yml"
	frontendChangesStepName   = "Detect frontend changes"
	frontendChangesZeroCommit = "0000000000000000000000000000000000000000"
)

// TestFrontendChangesStepDetectsRelevantPaths runs the change detection step of
// frontend.yml against a real git history.
//
// frontend は required check なので paths で絞らず毎回起動し、関係しない PR では
// lint と test を skip する (#3379)。**判定が false に倒れると、frontend を壊す PR が
// required を緑のまま通る。** 正規表現の書き損じ (エスケープの抜け、パスの綴り) は
// 目で見ても気付きにくいので、step の `run:` を bash でそのまま実行し、実際の差分で
// 判定を確かめる。
func TestFrontendChangesStepDetectsRelevantPaths(t *testing.T) {
	bash, err := exec.LookPath("bash")
	require.NoError(t, err, "bash が要る")
	_, err = exec.LookPath("git")
	require.NoError(t, err, "git が要る")

	wf := parseWorkflow(t, readRepoFile(t, frontendWorkflow))
	var script string
	for _, s := range wf.steps {
		if s.Name == frontendChangesStepName {
			script = s.Run
		}
	}
	require.NotEmptyf(t, script, "%s に step %q が無い", frontendWorkflow, frontendChangesStepName)

	tests := []struct {
		name  string
		event string
		files []string
		// zeroBase は push の before が全部 0 (ブランチの作成) の場合を表す。
		zeroBase bool
		want     string
		// many は frontend/ の外に足すファイルの数。一覧をパイプのバッファより
		// 大きくし、frontend のファイルを一覧の先頭に置く (git diff は名前順なので
		// 足す側を zz- の下にする)。grep -q が早く抜けて書き手が SIGPIPE になる形。
		many int
		// orphanBase は base が clone に無い commit (force-push の後) の場合を表す。
		orphanBase bool
		// moveOut は base にある frontend/moved.ts を、変更で frontend/ の外へ移す。
		moveOut bool
	}{
		{"frontend source", "pull_request", []string{"frontend/packages/frontend/src/main.ts"}, false, "true", 0, false, false},
		{"frontend lock", "pull_request", []string{"frontend/pnpm-lock.yaml"}, false, "true", 0, false, false},
		{"bundled plugin", "pull_request", []string{"plugins/status/frontend/index.ts"}, false, "true", 0, false, false},
		{"plugin api", "pull_request", []string{"plugin/plugin.go"}, false, "true", 0, false, false},
		{"pluginbuild", "pull_request", []string{"tools/pluginbuild/main.go"}, false, "true", 0, false, false},
		{"the workflow itself", "pull_request", []string{".github/workflows/frontend.yml"}, false, "true", 0, false, false},
		{"Makefile", "pull_request", []string{"Makefile"}, false, "true", 0, false, false},
		{"go.sum", "push", []string{"go.sum"}, false, "true", 0, false, false},
		{"mixed", "push", []string{"internal/core/a.go", "frontend/locales/ja-JP.yml"}, false, "true", 0, false, false},
		{"backend only", "pull_request", []string{"internal/core/a.go"}, false, "false", 0, false, false},
		{"docs only", "push", []string{"docs/ci.md"}, false, "false", 0, false, false},
		{"other workflow", "pull_request", []string{".github/workflows/ci.yml"}, false, "false", 0, false, false},
		{"lookalike prefix", "pull_request", []string{"frontendx/a.ts", "pluginsx/a.go", "Makefile.bak"}, false, "false", 0, false, false},
		{"new branch push", "push", []string{"docs/ci.md"}, true, "true", 0, false, false},
		{"manual run", "workflow_dispatch", []string{"docs/ci.md"}, false, "true", 0, false, false},
		{"emoji regex tool", "pull_request", []string{"tools/emojiregex/testdata/source.txt"}, false, "true", 0, false, false},
		{"emoji regex output", "pull_request", []string{"internal/activitypub/mfm/emoji_regex_gen.go"}, false, "true", 0, false, false},
		// 変更の多い PR: 一覧が 64KiB を超えても判定が false に倒れないこと。
		{"large diff", "pull_request", []string{"frontend/a.ts"}, false, "true", 3000, false, false},
		{"force-pushed base", "push", []string{"docs/ci.md"}, false, "true", 0, true, false},
		// frontend/ の外へ移すだけの変更。リネーム検出が効くと移動先しか出ない。
		{"move out of frontend", "pull_request", nil, false, "true", 0, false, true},
		{"japanese path", "pull_request", []string{"frontend/日本語.ts"}, false, "true", 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = repo
				cmd.Env = append(filterGitEnv(os.Environ()),
					"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
					"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
					"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
				out, err := cmd.CombinedOutput()
				require.NoErrorf(t, err, "git %v: %s", args, out)
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			require.NoError(t, os.WriteFile(filepath.Join(repo, "README.md"), []byte("base\n"), 0o644))
			if tt.moveOut {
				require.NoError(t, os.MkdirAll(filepath.Join(repo, "frontend"), 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(repo, "frontend", "moved.ts"),
					[]byte("export const a = 1;\nexport const b = 2;\n"), 0o644))
			}
			git("add", "-A")
			git("commit", "-q", "-m", "base")
			base := git("rev-parse", "HEAD")
			files := tt.files
			for i := 0; i < tt.many; i++ {
				files = append(files, fmt.Sprintf("zz-generated/a-rather-long-directory-name/file-%05d.md", i))
			}
			if tt.moveOut {
				git("mv", "frontend/moved.ts", "docs-moved.ts")
			}
			for _, f := range files {
				p := filepath.Join(repo, f)
				require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
				require.NoError(t, os.WriteFile(p, []byte("x\n"), 0o644))
			}
			git("add", "-A")
			git("commit", "-q", "-m", "change")
			if tt.zeroBase {
				base = frontendChangesZeroCommit
			}
			if tt.orphanBase {
				base = "1234567890abcdef1234567890abcdef12345678"
			}

			out := filepath.Join(t.TempDir(), "output")
			cmd := exec.Command(bash, "-c", script)
			cmd.Dir = repo
			cmd.Env = append(filterGitEnv(os.Environ()),
				"EVENT="+tt.event,
				"GITHUB_OUTPUT="+out, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
			// 実際の event と同じく、その event の変数にだけ値を入れる (取り違えを拾う)。
			switch tt.event {
			case "pull_request":
				cmd.Env = append(cmd.Env, "PR_BASE="+base, "PUSH_BEFORE=")
			case "push":
				cmd.Env = append(cmd.Env, "PR_BASE=", "PUSH_BEFORE="+base)
			default:
				cmd.Env = append(cmd.Env, "PR_BASE=", "PUSH_BEFORE=")
			}
			msg, err := cmd.CombinedOutput()
			require.NoErrorf(t, err, "step が失敗した: %s", msg)
			got, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.Equal(t, "frontend="+tt.want+"\n", string(got))
		})
	}
}
