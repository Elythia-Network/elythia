package entitycompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bundledAssetsArg is the build arg that pins the fork's frontend assets image.
const bundledAssetsArg = "MISSKEY_ASSETS_IMAGE"

var (
	// bundledAssetsArgRe captures the default value of `ARG MISSKEY_ASSETS_IMAGE=`.
	//
	// **検出は広く取る。** Dockerfile の命令は case-insensitive で、1 つの `ARG` に
	// 複数の名前を並べるのも正規の記法 (`ARG FOO=1 MISSKEY_ASSETS_IMAGE=...`)。
	// 狭く書くとその Dockerfile が**黙って検査対象から外れる** — allowlist にも
	// 載らないので gate は鳴らないまま検査だけが減る (兄弟の
	// `pluginbuild_dockerfile_test.go` と同じ判断)。
	//
	// **ただし 1 行の中に閉じる。** 空白を `\s` で書くと**改行にもマッチする**ので、
	// 「どこかに `ARG ` 行があれば、その後ろの任意の行の `MISSKEY_ASSETS_IMAGE=` を
	// ARG の既定値として読む」挙動になる。`RUN echo MISSKEY_ASSETS_IMAGE=$...` を
	// 1 行足しただけで落ち、しかも診断は無関係な `ARG` 行を指す。
	bundledAssetsArgRe = regexp.MustCompile(
		`(?mi)^[^\S\n]*ARG[^\S\n]+(?:[A-Za-z_][A-Za-z0-9_]*(?:=\S*)?[^\S\n]+)*?` + bundledAssetsArg + `=(\S+)`)

	// bundledAssetsRefRe matches an expansion of the build arg.
	//
	// `${MISSKEY_ASSETS_IMAGE:-fallback}` も参照なので前方一致で見る。`\b` は
	// `$MISSKEY_ASSETS_IMAGE_OTHER` を別物として外すために要る。
	bundledAssetsRefRe = regexp.MustCompile(`\$\{?` + bundledAssetsArg + `\b`)
)

// TestBundledAssetsPinMatchesDoc checks that every Dockerfile pinning the fork's
// frontend assets image uses the tag the submodule is pinned to.
//
// **配る bundled image に古い frontend が載るのを止める。**
//
// `Dockerfile.bundled` の `MISSKEY_ASSETS_IMAGE` は fork が publish する assets
// image を指す。ここが submodule の tag から遅れても**古い tag の image は
// 問題なくビルドできるので CI は落ちない**。落ちるのは配った先だけで、気付く
// 手段が無い。実測では `1.3.0` のリリース時点で既に 2 世代 (submodule が
// `2026.9.0-mk.2` に対して pin は `mk.0`)、develop では 29 世代ずれていた
// (#3011。pin されていた `mk.0` から数えた間隔で、数字付きの tag 30 個から 1 を
// 引いた値。英字付きを含めると 62 個から 1 を引いて 61)。
//
// **表に載せる運用では再発した。** `docs/upstream-catch-up.md` は
// 「`Dockerfile.bundled` は表に無かったせいで実際に 23 世代遅れた」(#2877) と
// 自分で書いているのに、表へ載せた後に同じことが起きている。
//
// **突き合わせ先は doc の pin 行にする。** submodule の working tree
// (`git -C third_party/misskey describe`) を truth にすると checkout が要り、
// `make gates` の前提 (サーバー / ブラウザ / Docker / ネットワーク不要) を
// 満たせない。pin 行なら親リポだけで完結する。
//
// **doc 内で閉じる輪と、CI が閉じる輪は別物。** ここと
// `TestSubmodulePinTagMatchesTable` が繋ぐのは「Dockerfile の tag == pin 行の
// tag == §4-2 の表の最終行」、`TestSubmodulePinMatchesDoc` が繋ぐのは
// 「pin 行の SHA == gitlink」。**pin 行の tag とその SHA が同じ commit を指すか
// は誰も見ていない** — それを閉じるのは CI の `build` job の
// `Check submodule commit is pushed` (`git ls-remote` で tag → commit を解く)
// で、ネットワークが要るので `make gates` には載せられない。
//
// **「その tag の assets image が publish されているか」も見ない** (#3011 で
// 対象外と決めた)。同じくネットワークが要る。実際に publish に失敗している
// tag は前例がある (`2026.7.0-mk.18`) ので、tag を上げたら
// `docs/upstream-catch-up.md` の手順で workflow の success を確認すること。
//
// **リテラル参照は ARG の有無と切り離して見る (2 パス)。** ARG を宣言した
// ファイルの中だけを見る形だと、assets image を `FROM <repo>:<古い tag>` と
// 直接書いた Dockerfile が素通りする。repository は ARG の値から集めるので、
// image 名をここにハードコードはしない。
//
// **検出ロジックそのものは `TestAssetsPinScanners` が合成入力で固定する。**
// 実ファイルは常に「正しい pin が 1 つ」なので、実ファイルだけを見ていると
// 判定の枝がほとんど実行されない (#2792 の secretfield gate と同じ判断)。
//
// **実ファイルに対する変異検証は 19 形**。Dockerfile / doc 側が 17 — 同じ
// ファイルを変えるものが 12 (pin を古い tag に戻す / 存在しない tag にする /
// doc の pin 行の tag だけ変える / ARG 行を消す / ARG 行をコメントアウトする /
// tag を外す (暗黙 latest) / digest だけの pin にする / tag を変数参照にする / pin 行の
// 書式例を同じ行に足す / 別の行に足す / `FROM` をリテラルの古い tag に書き換える
// (ARG の参照は残す) / ARG を宣言したまま参照しない)、別の tracked Dockerfile を
// 足すものが 5 (`arg` 小文字 / 1 命令に複数名 / `bundled.Dockerfile` /
// `Containerfile` / **ARG を持たず `FROM <repo>:<古い tag>` と直接書く**)。
// ゲート側が 2 — 正規表現を空振りさせる / 走査対象の判定を潰す。全て検出する
// ことを実測した。
//
// **既知の取りこぼし**: (a) ファイル名が `dockerfile` / `containerfile` そのもの
// でも、その拡張子でも、その接頭辞でもない pin、(b) untracked な Dockerfile
// (`git ls-files` で見るため)、(c) image 名そのもの — repository が変わっても
// 「乖離」ではなく意図的な変更として通る、(d)
// `docker build --build-arg MISSKEY_ASSETS_IMAGE=<古い tag>` による実行時の
// 上書き (静的にしか見ない。リポジトリ内でこれを渡している箇所は無い)。
//
// **(b) の理由は兄弟ゲートと逆向きなので、文を写して使わないこと。**
// `gate_run_patterns_test.go` / `migration_count_doc_test.go` は「ファイルが
// 存在すること」が合格条件なので、ディスク走査にすると `git add` 忘れが手元で
// 素通りし CI で落ちる。こちらはファイルが**検査される側**なので向きが反転し、
// ディスク走査にすると**手元だけが untracked な pin を検査して CI では見ない**
// (実測で確認した)。`git ls-files` を選ぶ結論は同じだが、理由は「手元と CI が
// 同じ集合を見て結果が再現すること」。
func TestBundledAssetsPinMatchesDoc(t *testing.T) {
	root := repoRoot(t)
	docTag := divergencePinTag(t)

	cmd := exec.Command("git", "-C", root, "ls-files")
	// 呼び出し元の git 環境を持ち込まない (別リポジトリを指す GIT_DIR が立って
	// いると列挙が空になり、診断が「書式を直せ」という実態とずれたものになる)。
	cmd.Env = filterGitEnv(os.Environ())
	out, err := cmd.Output()
	require.NoErrorf(t, err, "git ls-files に失敗した (git リポジトリではない?): %v", err)

	// **`Dockerfile.bundled` を名指ししない。** assets image を pin する
	// Dockerfile が増えたとき、名指しだと新しい方を黙って検査対象から落とす。
	type dockerfile struct{ path, body string }
	var files []dockerfile
	for _, path := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !looksLikeDockerfile(path) {
			continue
		}
		// コメント行の pin は拾わない。コメントアウトして残すのは消すのと
		// 同じで、それで検査対象が減るのは正しい (1 つも残らなければ下で落ちる)。
		files = append(files, dockerfile{
			path: path,
			body: foldContinuations(stripDockerfileComments(readRepoFile(t, path))),
		})
	}

	// pass 1: ARG の既定値を見る。あわせて repository を集める。
	found := 0
	repoSet := map[string]bool{}
	for _, f := range files {
		refs := scanAssetsArgRefs(f.body)
		if len(refs) == 0 {
			continue
		}
		// **1 件目で止めない。** 複数のファイルが同時にドリフトしたとき、
		// 最初の 1 つだけ直して「まだ落ちる」を繰り返すことになる
		// (`TestDockerfilesEmbedPlugins` と同じ方針)。
		for _, ref := range refs {
			found++
			repo, tag, ok := splitImageRef(ref)
			if !assert.Truef(t, ok, "%s の %s (%s) から repository と tag を読めない。\n"+
				"doc の pin (tag) と突き合わせるので `<image>:<tag>@sha256:<digest>` の形で書くこと\n"+
				"(tag を持たない digest だけの pin や変数のままだと対応が取れない)", f.path, bundledAssetsArg, ref) {
				continue
			}
			assertAssetsTag(t, f.path, docTag, tag, "ARG "+bundledAssetsArg)
			// **digest も要求する。** tag は付け替えられるので、tag だけだと
			// fork 側で同じ tag を publish し直したときに焼き込む中身が黙って
			// 変わる。digest が tag と対応しているかはネットワークが要るので
			// ここでは見ない (docker.yml の build-and-push-bundled が見る)。
			assert.Truef(t, hasImageDigest(ref),
				"%s の %s (%s) に digest が無い。\n"+
					"`<image>:<tag>@sha256:<digest>` の形で書くこと (取り方は docs/upstream-catch-up.md)",
				f.path, bundledAssetsArg, ref)
			repoSet[repo] = true
		}

		// **宣言しただけで使っていない pin を通さない。** ARG が誰からも参照
		// されていなければ、その Dockerfile が焼き込む frontend は別の経路で
		// 決まっていて、ここを揃えても意味が無い (#2762 の wiring-check が
		// 「列と admin 公開はあるが読み取り経路に配線されていない」を塞いだのと
		// 同じ形。ここでは ARG が宣言、`FROM` が配線にあたる)。
		assert.Truef(t, referencesAssetsArg(f.body),
			"%s は %s を宣言しているのに、どこからも参照していない。\n"+
				"`FROM ${%s}` のように使うか、宣言ごと消すこと",
			f.path, bundledAssetsArg, bundledAssetsArg)
	}

	// **拾えなかったら落とす。** 書式や配置が変わって空振りすると、検査して
	// いないのに緑になる (compose-check / mdtable-check と同じ判断)。
	require.NotZerof(t, found, "%s を pin する Dockerfile を 1 つも見つけられませんでした。\n"+
		"書式は `ARG %s=<image>:<tag>` で、変えるならこのゲートも直すこと",
		bundledAssetsArg, bundledAssetsArg)

	repos := make([]string, 0, len(repoSet))
	for repo := range repoSet {
		repos = append(repos, repo)
	}
	sort.Strings(repos) // 失敗の診断を実行ごとに変えない

	// pass 2: 同じ repository を指すリテラル参照。**ARG を持たないファイルも
	// 見る。** `FROM ${MISSKEY_ASSETS_IMAGE}` をリテラルの古い tag に書き換え
	// られる (ARG を正しいまま残せる) ので、ARG だけを見る形では**この gate が
	// 防ぐはずの事故がそのまま素通りする**。
	for _, f := range files {
		for _, repo := range repos {
			for _, tag := range scanAssetsLiteralTags(f.body, repo) {
				assertAssetsTag(t, f.path, docTag, tag, repo+" のリテラル参照")
			}
		}
	}
}

// TestAssetsPinScanners pins the scanners' behaviour with synthetic sources.
//
// **実ファイルだけでは検出の枝が実行されない。** リポジトリの pin は常に
// 「正しいものが 1 つ」なので、肯定側 (正当な書き方を落とさない) も否定側
// (壊れた書き方を拾う) も実ファイルからは確かめられない。検出を広げると
// 正当な書き方を落とす方向に倒れるので、**肯定側こそ固定する意味がある**
// (#2942 の dockerignore gate と同じ判断)。
func TestAssetsPinScanners(t *testing.T) {
	const repo = "ghcr.io/example/misskey-ts-assets"

	t.Run("ARG の既定値", func(t *testing.T) {
		tests := []struct {
			name string
			body string
			want []string
		}{
			{"plain ARG", "ARG " + bundledAssetsArg + "=" + repo + ":v1\n", []string{repo + ":v1"}},
			{"lowercase arg", "arg " + bundledAssetsArg + "=" + repo + ":v1\n", []string{repo + ":v1"}},
			{"indented", "  ARG " + bundledAssetsArg + "=" + repo + ":v1\n", []string{repo + ":v1"}},
			{"several names in one ARG", "ARG FOO=1 " + bundledAssetsArg + "=" + repo + ":v1\n", []string{repo + ":v1"}},
			{"double quoted", "ARG " + bundledAssetsArg + `="` + repo + ":v1\"\n", []string{repo + ":v1"}},
			{"single quoted", "ARG " + bundledAssetsArg + "='" + repo + ":v1'\n", []string{repo + ":v1"}},
			// 行をまたいで拾うと、無関係な行を ARG の既定値として読む。
			{"ENV on a later line", "ARG FOO=1\nENV " + bundledAssetsArg + "=" + repo + ":v1\n", nil},
			{"RUN echoing the name", "ARG FOO=1\nRUN echo " + bundledAssetsArg + "=" + repo + ":v1\n", nil},
			{"different arg name", "ARG OTHER_" + bundledAssetsArg + "=" + repo + ":v1\n", nil},
			{"not an ARG line", "ENV " + bundledAssetsArg + "=" + repo + ":v1\n", nil},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				require.Equal(t, tt.want, scanAssetsArgRefs(tt.body))
			})
		}
	})

	t.Run("リテラル参照", func(t *testing.T) {
		tests := []struct {
			name string
			body string
			want []string
		}{
			{"FROM literal", "FROM " + repo + ":v1 AS assets\n", []string{"v1"}},
			{"inside a default expansion", "FROM ${" + bundledAssetsArg + ":-" + repo + ":v1}\n", []string{"v1"}},
			{"quoted", `LABEL x="` + repo + ":v1\"\n", []string{"v1"}},
			{"several", "FROM " + repo + ":v1\nFROM " + repo + ":v2\n", []string{"v1", "v2"}},
			{"another repository", "FROM ghcr.io/example/other:v1\n", nil},
			{"variable, not a literal", "FROM ${" + bundledAssetsArg + "}\n", nil},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				require.Equal(t, tt.want, scanAssetsLiteralTags(tt.body, repo))
			})
		}
	})

	t.Run("ARG の参照", func(t *testing.T) {
		tests := []struct {
			name string
			body string
			want bool
		}{
			{"braced", "FROM ${" + bundledAssetsArg + "}\n", true},
			{"bare", "FROM $" + bundledAssetsArg + "\n", true},
			{"with a default", "FROM ${" + bundledAssetsArg + ":-scratch}\n", true},
			{"a longer name", "FROM ${" + bundledAssetsArg + "_OTHER}\n", false},
			{"declared only", "ARG " + bundledAssetsArg + "=x:v1\n", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				require.Equal(t, tt.want, referencesAssetsArg(tt.body))
			})
		}
	})

	t.Run("image ref の分解", func(t *testing.T) {
		tests := []struct {
			name, ref, repo, tag string
			ok                   bool
		}{
			{name: "repo and tag", ref: repo + ":v1", repo: repo, tag: "v1", ok: true},
			{name: "registry port", ref: "localhost:5000/x:v1", repo: "localhost:5000/x", tag: "v1", ok: true},
			{name: "no tag", ref: repo},
			{name: "port but no tag", ref: "localhost:5000/x"},
			{name: "digest pin", ref: repo + "@sha256:abc"},
			{name: "tag and digest", ref: repo + ":v1@sha256:" + strings.Repeat("a", 64), repo: repo, tag: "v1", ok: true},
			{name: "digest only", ref: repo + "@sha256:" + strings.Repeat("a", 64)},
			{name: "tag and malformed digest", ref: repo + ":v1@sha256:abc"},
			{name: "variable tag", ref: repo + ":${TAG}"},
			{name: "variable repo", ref: "${REGISTRY}/x:v1"},
			{name: "empty tag", ref: repo + ":"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				gotRepo, gotTag, ok := splitImageRef(tt.ref)
				require.Equal(t, tt.ok, ok)
				require.Equal(t, tt.repo, gotRepo)
				require.Equal(t, tt.tag, gotTag)
			})
		}
	})

	t.Run("digest の有無", func(t *testing.T) {
		digest := "sha256:" + strings.Repeat("0", 64)
		tests := []struct {
			name, ref string
			want      bool
		}{
			{"tag and digest", repo + ":v1@" + digest, true},
			{"tag only", repo + ":v1", false},
			{"malformed digest", repo + ":v1@sha256:abc", false},
			{"uppercase hex", repo + ":v1@sha256:" + strings.Repeat("A", 64), false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				require.Equal(t, tt.want, hasImageDigest(tt.ref))
			})
		}
	})

	t.Run("走査対象のファイル名", func(t *testing.T) {
		tests := []struct {
			path string
			want bool
		}{
			{"Dockerfile", true},
			{"Dockerfile.bundled", true},
			{"deploy/uds/Dockerfile.mkgo", true},
			{"bundled.Dockerfile", true},
			{"Containerfile", true},
			{"containerfile.dev", true},
			// Go / md のソースを混ぜると、doc やテストに書いた例を pin として
			// 読んでしまう (実測で `pluginbuild_dockerfile_test.go` が入った)。
			{"internal/entitycompat/pluginbuild_dockerfile_test.go", false},
			{"docs/dockerfile-notes.md", false},
			{"Makefile", false},
		}
		for _, tt := range tests {
			t.Run(tt.path, func(t *testing.T) {
				require.Equal(t, tt.want, looksLikeDockerfile(tt.path))
			})
		}
	})
}

// scanAssetsArgRefs returns the image references pinned by `ARG MISSKEY_ASSETS_IMAGE=`.
func scanAssetsArgRefs(body string) []string {
	var refs []string
	for _, m := range bundledAssetsArgRe.FindAllStringSubmatch(body, -1) {
		refs = append(refs, strings.Trim(m[1], `"'`))
	}
	return refs
}

// scanAssetsLiteralTags returns every tag body pins for repo by writing it out.
func scanAssetsLiteralTags(body, repo string) []string {
	// OCI の tag は `[A-Za-z0-9_][A-Za-z0-9._-]*`。許可リストで書いておくと
	// `${VAR:-repo:tag}` の閉じ括弧や引用符で自然に止まる (除外リストだと
	// `}` を書き忘れて「tag は同じなのに違うと言う」診断になる)。
	re := regexp.MustCompile(regexp.QuoteMeta(repo) + `:([A-Za-z0-9_][A-Za-z0-9._-]*)`)

	var tags []string
	for _, m := range re.FindAllStringSubmatch(body, -1) {
		tags = append(tags, m[1])
	}
	return tags
}

// referencesAssetsArg reports whether body expands the build arg.
func referencesAssetsArg(body string) bool {
	return bundledAssetsRefRe.MatchString(body)
}

// assertAssetsTag reports a failure unless tag equals the tag the submodule is
// pinned to. Does not stop the test, so one run lists every drifted file.
func assertAssetsTag(t *testing.T, path, docTag, tag, where string) {
	t.Helper()

	assert.Equalf(t, docTag, tag,
		"%s の assets image pin が fork frontend の pin と違う (%s)。\n"+
			"  %s : %s\n"+
			"  docs/divergence.md : %s\n"+
			"submodule の tag を上げたら、**配る image が焼き込む frontend も同じ tag に\n"+
			"揃える**こと。古い tag でも image はビルドできるので CI では気付けず、\n"+
			"配った先にだけ古い frontend が載る (#2877 / #3011)。\n"+
			"tag に対応する assets image が publish 済みかは別途確認すること\n"+
			"(このゲートはネットワークを使わないので見ていない)。",
		path, where, path, tag, docTag)
}

// looksLikeDockerfile reports whether path names a container build file.
//
// **接頭辞一致では足りず、部分一致では広すぎる。** `bundled.Dockerfile` のように
// 接尾辞で書くのも `Containerfile` (OCI / podman の綴り) も正規の命名なので
// 接頭辞だけだと黙って走査から外れるが、単純な部分一致にすると
// `pluginbuild_dockerfile_test.go` のような **Go ソースまで Dockerfile として
// 読む** (実測でそうなった)。名前そのもの / `<name>.` 始まり / `.<name>` 終わりの
// 3 通りに絞る。
func looksLikeDockerfile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	for _, name := range []string{"dockerfile", "containerfile"} {
		if base == name || strings.HasPrefix(base, name+".") || strings.HasSuffix(base, "."+name) {
			return true
		}
	}
	return false
}

// splitImageRef splits an image reference into its repository and tag.
//
// ok=false は「tag として突き合わせられない形」。tag を持たない digest pin (`<image>@sha256:...`)、
// tag 無し、registry の port (`host:5000/x`) を tag と誤読する形、変数が展開
// されないまま残っている形をまとめて弾く。**曖昧なら通さない**側に倒してある
// ので、読めない pin は呼び出し側で fail する。
func splitImageRef(ref string) (repo, tag string, ok bool) {
	// `<tag>@sha256:<digest>` の併記は tag を持つので受ける。digest だけの pin
	// (`<image>@sha256:...`) は tag を持たないので、digest を外した後の tag
	// 判定で落ちる。digest の書式が崩れているものも通さない。
	if at := strings.IndexByte(ref, '@'); at >= 0 {
		if !imageDigestRe.MatchString(ref[at+1:]) {
			return "", "", false
		}
		ref = ref[:at]
	}
	i := strings.LastIndex(ref, ":")
	if i < 0 {
		return "", "", false
	}
	repo, tag = ref[:i], ref[i+1:]
	// `host:5000/x` の `:` は port であって tag ではない。
	if repo == "" || tag == "" || strings.Contains(tag, "/") {
		return "", "", false
	}
	// `${FOO}` のまま書かれていると値が分からない。
	if strings.ContainsAny(repo, "${}") || strings.ContainsAny(tag, "${}") {
		return "", "", false
	}
	return repo, tag, true
}

// imageDigestRe matches the digest part of an image reference.
var imageDigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// hasImageDigest reports whether ref ends with a well-formed `@sha256:` digest.
func hasImageDigest(ref string) bool {
	at := strings.LastIndexByte(ref, '@')
	return at >= 0 && imageDigestRe.MatchString(ref[at+1:])
}
