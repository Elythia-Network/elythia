package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseSpecs_Accepts(t *testing.T) {
	in := `
# コメント行は無視される
weather https://github.com/foo/mk-plugin-weather v1.2.0

np      https://example.com/bar/np.git   0123456789abcdef0123456789abcdef01234567  # 行末コメント
deep https://github.com/foo/deep release/2026.9
`
	got, err := parseSpecs(strings.NewReader(in))
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, spec{"weather", "https://github.com/foo/mk-plugin-weather", "v1.2.0"}, got[0])
	require.Equal(t, "0123456789abcdef0123456789abcdef01234567", got[1].ref)
	require.Equal(t, "release/2026.9", got[2].ref)
}

// `#` を含む url を黙って切らないこと。先に行内の `#` 以降を落とす実装だと
// url が別のものに化けたうえ、「3 つが要ります」という原因から遠いエラーになる。
// (ref 側の `#` は refRe が弾く。git の ref に `#` は使えない)
func TestParseSpecs_HashInsideFieldIsNotAComment(t *testing.T) {
	got, err := parseSpecs(strings.NewReader("a https://example.com/x#frag v1.0.0\n"))
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "https://example.com/x#frag", got[0].url)
	require.Equal(t, "v1.0.0", got[0].ref)
}

func TestParseSpecs_Empty(t *testing.T) {
	got, err := parseSpecs(strings.NewReader("\n  \n# only a comment\n"))
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestParseSpecs_Rejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// ref を省略した形。既定ブランチへ落とすと、作者のアカウントが侵害された
		// ときに次のビルドで任意コードが入る。
		{"ref 省略", "weather https://github.com/foo/w", "3 つが要ります"},
		{"フィールド過多", "a https://x/y v1 extra", "3 つが要ります"},
		{"name が大文字", "Weather https://github.com/foo/w v1", "name"},
		{"name にスラッシュ", "foo/bar https://github.com/foo/w v1", "name"},
		{"name が .. ", ".. https://github.com/foo/w v1", "name"},
		{"name がハイフン始まり", "-foo https://github.com/foo/w v1", "name"},
		{"name 重複", "a https://x/y v1\na https://x/z v2", "重複"},
		{"url が http", "a http://github.com/foo/w v1", "url"},
		{"url が ssh", "a git@github.com:foo/w.git v1", "url"},
		{"url が file", "a file:///etc v1", "url"},
		{"url が ext", "a ext::sh -c whoami v1", "3 つが要ります"},
		{"url がホスト無し", "a https:// v1", "ホストがありません"},
		{"url がスラッシュだけ", "a https:///etc/passwd v1", "ホストがありません"},
		// token が pluginresolve の出力に平文で載る経路。plugins は普通の input
		// なので GitHub の secret マスクが効かない。
		{"url に userinfo", "a https://x-access-token:ghp_secret@github.com/foo/w v1", "userinfo"},
		{"url に userinfo (パスワード無し)", "a https://evil@github.com/foo/w v1", "userinfo"},
		// 目視で github.com と区別できない形。取ってきたコードはサーバーと
		// 同じ権限で動くので、選択の明示性が唯一の防壁になる。
		{"url にゼロ幅スペース", "a https://github.com\u200b.evil.example/x v1", "非 ASCII"},
		{"url にキリル文字", "a https://gіthub.com/foo/bar v1", "非 ASCII"},
		{"url に RTL override", "a https://github.com/foo/\u202ebar v1", "非 ASCII"},
		{"url に不正な UTF-8", "a https://github.com/\xff/x v1", "非 ASCII"},
		// git の引数として解釈される値。`--upload-pack=` は任意コマンドの実行になる。
		{"ref がオプション", "a https://github.com/foo/w --upload-pack=id", "ref"},
		{"ref に空白由来の分割", "a https://github.com/foo/w v1 v2", "3 つが要ります"},
		{"ref に ..", "a https://github.com/foo/w v1/../../etc", ".."},
		{"ref にセミコロン", "a https://github.com/foo/w v1;whoami", "ref"},
		{"ref にハッシュ", "a https://github.com/foo/w v1#2", "ref"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseSpecs(strings.NewReader(tt.in))
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestValidateURL_AcceptsPlainHTTPS(t *testing.T) {
	require.NoError(t, validateURL("https://github.com/a/b"))
	require.NoError(t, validateURL("https://example.com:8443/a/b.git"))
	// path に @ があるのは userinfo ではない。
	require.NoError(t, validateURL("https://example.com/a/@scope/b"))
}

// **要求した ref の中身が置かれる**ことを確かめる。fixture が 1 コミットだと
// tag も branch も SHA も同じ tree を指すので、pin が壊れても緑になる。
func TestClone_PinsRequestedRef(t *testing.T) {
	requireGit(t)
	origin, first, second := newOriginRepo(t)

	tests := []struct {
		ref  string
		want string
	}{
		{"v1.0.0", "version: 1"},
		{first, "version: 1"},
		{"main", "version: 2"},
		{second, "version: 2"},
	}
	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, clone(root, spec{name: "p", url: origin, ref: tt.ref}))

			body, err := os.ReadFile(filepath.Join(root, "plugins", "p", "elythia-plugin.yml"))
			require.NoError(t, err)
			require.Contains(t, string(body), tt.want)

			// .git を残すとビルドコンテキストが膨らむだけなので落とす。
			_, err = os.Stat(filepath.Join(root, "plugins", "p", ".git"))
			require.True(t, os.IsNotExist(err), ".git が残っている")
		})
	}
}

// ref が git のオプションとして解釈されないこと。parseSpecs は先頭 `-` を弾くので
// 通常は届かないが、`clone` 側の `--` はその正規表現を緩めたときの二重の壁として
// 置いてある。**壁が消えたことを検出する** — `--` を外すと git は
// `--upload-pack=<cmd>` をオプションとして受け取り、任意コマンドを実行する。
func TestClone_RefIsNotTreatedAsGitOption(t *testing.T) {
	requireGit(t)
	origin, _, _ := newOriginRepo(t)
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "pwned")

	err := clone(root, spec{name: "p", url: origin, ref: "--upload-pack=touch " + marker})
	require.Error(t, err)

	_, statErr := os.Stat(marker)
	require.True(t, os.IsNotExist(statErr), "ref が git のオプションとして解釈され、任意コマンドが実行された")
}

func TestClone_RefusesExistingDirectory(t *testing.T) {
	requireGit(t)
	origin, _, _ := newOriginRepo(t)
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "plugins", "p"), 0o755))

	err := clone(root, spec{name: "p", url: origin, ref: "main"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "既に存在します")
}

// 失敗したら中途半端なディレクトリを残さない。残すと次の実行が
// 「既に存在します」で落ち、原因から遠い症状になる。
func TestClone_CleansUpAfterFailure(t *testing.T) {
	requireGit(t)
	origin, _, _ := newOriginRepo(t)
	root := t.TempDir()

	err := clone(root, spec{name: "p", url: origin, ref: "v9.9.9"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "git fetch")

	_, statErr := os.Stat(filepath.Join(root, "plugins", "p"))
	require.True(t, os.IsNotExist(statErr), "失敗した clone のディレクトリが残っている")
}

// unreachableURL は接続が即座に拒否される https URL。
//
// `run` の分岐を試すのに使う。**parseSpecs を通るので https でなければならず**、
// ローカルの fixture リポジトリ (パス指定) は渡せない。clone 成功側の挙動は
// TestClone_* が直接 clone を呼んで確かめている。
const unreachableURL = "https://127.0.0.1:1/nonexistent.git"

// -dry-run は検証だけで clone しない。ここを取り違えると、検証目的の実行が
// 実際にネットワークへ出る。
func TestRun_DryRunDoesNotClone(t *testing.T) {
	requireGit(t)
	root := t.TempDir()

	var out, errOut bytes.Buffer
	in := strings.NewReader("p " + unreachableURL + " main\n")
	require.NoError(t, run([]string{"-dry-run", "-root", root}, in, &out, &errOut))

	require.Contains(t, out.String(), "p "+unreachableURL+" main")
	_, err := os.Stat(filepath.Join(root, "plugins", "p"))
	require.True(t, os.IsNotExist(err), "-dry-run なのに clone された")
}

// 逆に、dry-run でなければ clone が実際に走る。到達できない URL なので失敗
// するが、**失敗すること自体が clone を呼んだ証拠**になる。
func TestRun_AttemptsCloneWhenNotDryRun(t *testing.T) {
	requireGit(t)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	root := t.TempDir()

	var out, errOut bytes.Buffer
	in := strings.NewReader("p " + unreachableURL + " main\n")
	err := run([]string{"-root", root}, in, &out, &errOut)
	require.Error(t, err)
	require.Contains(t, err.Error(), "git fetch")
}

func TestRun_ReportsNothingToDo(t *testing.T) {
	var out, errOut bytes.Buffer
	require.NoError(t, run([]string{"-root", t.TempDir()}, strings.NewReader("# nothing\n"), &out, &errOut))
	// **stdout は空でなければならない。** ここに案内文を出すと、workflow が
	// `-dry-run` の出力をパースしたときに 1 行目をプラグイン名として読む。
	require.Empty(t, out.String())
	require.Contains(t, errOut.String(), "指定されたプラグインはありません")
}

func TestRun_RejectsUnknownFlag(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"-nope"}, strings.NewReader(""), &out, &errOut)
	require.Error(t, err)
}

func TestRun_PropagatesParseError(t *testing.T) {
	var out, errOut bytes.Buffer
	err := run([]string{"-dry-run"}, strings.NewReader("BAD https://github.com/a/b v1\n"), &out, &errOut)
	require.Error(t, err)
	require.Contains(t, err.Error(), "name")
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git が無い")
	}
}

// newOriginRepo builds a throwaway repository with two commits so that the tag,
// the branch head and each SHA point at different content.
func newOriginRepo(t *testing.T) (dir, firstSHA, secondSHA string) {
	t.Helper()
	dir = t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return strings.TrimSpace(string(out))
	}
	write := func(version string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "elythia-plugin.yml"),
			[]byte("name: p\nversion: "+version+"\n"), 0o644))
	}

	run("init", "--quiet", "--initial-branch", "main")
	write("1")
	run("add", ".")
	run("commit", "--quiet", "-m", "first")
	run("tag", "v1.0.0")
	firstSHA = run("rev-parse", "HEAD")

	write("2")
	run("add", ".")
	run("commit", "--quiet", "-m", "second")
	secondSHA = run("rev-parse", "HEAD")

	// shallow fetch でも任意の SHA を解決できるようにする。
	run("config", "uploadpack.allowAnySHA1InWant", "true")

	return dir, firstSHA, secondSHA
}
