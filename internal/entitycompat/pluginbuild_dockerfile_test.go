package entitycompat

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// pluginbuildExemptDockerfiles lists Dockerfiles that build cmd/misskey but are
// allowed not to embed plugins, with the reason each is safe.
//
// **理由付きの allowlist にするのが要点。** 「プラグインを組み込まない
// Dockerfile」は正当に存在しうるので単純な全件強制はできない。一方で
// 素通しにすると #2940 と同じことが起きる — `Dockerfile.bundled` は
// pluginbuild を呼んでおらず、**プラグインを指定してビルドしても入らない
// image が黙って出来ていた**。エラーにならないので運営者は気付けない。
// 新しい Dockerfile を足したとき、ここに理由を書くか pluginbuild を呼ぶかを
// **選ばせる**形にしてある。
var pluginbuildExemptDockerfiles = map[string]string{
	"tests/federation/common/Dockerfile.mkgo": "連合 e2e 用。plugins/ を持たない素の mk-go 同士を突き合わせるので組み込む対象が無い",
}

// mkgoBuildVerbs and mkgoBuildTargets identify a command that compiles mk-go.
//
// **どちらも 1 つの文字列に頼らない。** `./cmd/misskey` だけを探す形は module path
// (`github.com/shiroha-a/mk/cmd/misskey`) やワイルドカード (`./cmd/...`) で書かれた
// Dockerfile を builder 集合から黙って落とす。動詞側も同じで、`go install` に
// 変えるだけで検査対象から外れる。**落ちたものは allowlist にも載らないので
// gate は鳴らないまま検査が減る** (「1 つも拾えなかったら落とす」は全部消えた
// ときしか効かない)。
var (
	mkgoBuildVerbs   = []string{"go build", "go install"}
	mkgoBuildTargets = []string{"cmd/misskey", "cmd/..."}
)

// TestDockerfilesEmbedPlugins checks that every Dockerfile building the mk-go
// binary also runs the plugin generator, before the binary is compiled.
//
// プラグインはビルド時組み込みで、生成物は gitignore されている。生成を
// 走らせない Dockerfile は「plugins/ に置いたのに入っていない」image を作る。
func TestDockerfilesEmbedPlugins(t *testing.T) {
	root := repoRoot(t)

	out, err := exec.Command("git", "-C", root, "ls-files").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	type builder struct {
		path string
		// full keeps shell comments; exec drops them.
		full, exec string
	}
	var builders []builder

	for _, path := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		// **判定は `looksLikeDockerfile` と共有する。** 接頭辞一致だけだと
		// `bundled.Dockerfile` や `Containerfile` を黙って検査対象から落とす
		// (#3011 でそちらの gate を書いたときに実測した)。現在の集合は
		// 17 ファイルで両者一致する。
		if !looksLikeDockerfile(path) {
			continue
		}
		// **builder の判定にはシェルのコメントを落とさない body を使う。**
		// 行末 `#` の除去は「落としすぎる」側に倒してあるので、`go build` の
		// 行にたまたま ` #` があるとその Dockerfile ごと検査対象から消える。
		// 検出は広く、実行されるかの判定は狭く、と分けてある。
		full := foldContinuations(stripDockerfileComments(readRepoFile(t, path)))
		if !buildsMkGo(full) {
			continue
		}
		builders = append(builders, builder{path: path, full: full, exec: stripShellLineComments(full)})
	}

	// **拾えなかったら落とす。** 命名や配置が変わって空振りすると、検査して
	// いないのに緑になる (compose-check / mdtable-check と同じ判断)。
	if len(builders) == 0 {
		t.Fatal("cmd/misskey をビルドする Dockerfile を 1 つも見つけられませんでした")
	}

	used := map[string]bool{}
	for _, b := range builders {
		// 実行されるかは行末コメントを落とした body で見る。シェルは
		// 「空白 + #」以降をコメントとして扱うので、そこに置かれた
		// pluginbuild は「書いてあるのに実行されない」(#2856 が wiring-check で
		// `/* */` に対して踏んだのと同型)。
		if gen := strings.Index(b.exec, "tools/pluginbuild"); gen >= 0 {
			if _, ok := pluginbuildExemptDockerfiles[b.path]; ok {
				t.Errorf("%s は pluginbuild を実行しているので pluginbuildExemptDockerfiles から外してください", b.path)
			}
			// **順序も見る。** 生成が go build の後だと、binary には
			// プラグインが入らないまま生成物だけが残る。RUN を整理した
			// ときに自然に起きる形で、しかもビルドは成功する。
			//
			// **基準にするのは「動詞と対象を同時に含むコマンド」だけ。**
			// 同じ RUN の中に無関係な `go build` / `go install` があると、
			// 行継続を畳んだあとではそちらが基準点を前へ引っ張り、
			// **正しい Dockerfile が順序違反で落ちる** (しかも診断が事実と
			// 逆を指す)。対象が変数に入っている等で特定できないときは
			// -1 が返り、順序判定だけを飛ばす (存在の検査は続く)。
			//
			// 見ているのは最初の出現同士の前後関係だけ。**別 stage に
			// 置かれた場合は存在判定・順序判定のどちらも素通りする** —
			// 存在判定はファイル全体を見るため。
			if bi := firstMkGoBuildIndex(b.exec); bi >= 0 && gen > bi {
				t.Errorf("%s は pluginbuild を go build より後に実行しています。"+
					"生成物が binary に入らないので、プラグイン無しの image が黙って出来ます", b.path)
			}
			continue
		}
		reason, ok := pluginbuildExemptDockerfiles[b.path]
		if !ok {
			// 書いてあるのに実行されない形とそもそも無い形で、疑う場所が違う。
			if strings.Contains(b.full, "tools/pluginbuild") {
				t.Errorf("%s の pluginbuild はシェルのコメント (行末の ` #`) に入っていて実行されません。"+
					"コメントの位置を見直してください", b.path)
				continue
			}
			t.Errorf("%s は cmd/misskey をビルドしますが pluginbuild を実行していません。"+
				"plugins/ に置いたプラグインが入らない image が黙って出来ます。"+
				"組み込まないなら pluginbuildExemptDockerfiles に理由付きで登録してください", b.path)
			continue
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("%s の pluginbuildExemptDockerfiles の理由が空です", b.path)
		}
		used[b.path] = true
	}

	// 一覧が腐るのを防ぐ。削除された Dockerfile や、後から pluginbuild を
	// 呼ぶようになったものが residue として残らないようにする。
	for path := range pluginbuildExemptDockerfiles {
		if !used[path] {
			t.Errorf("pluginbuildExemptDockerfiles の %s は cmd/misskey をビルドする Dockerfile として見つかりませんでした", path)
		}
	}
}

// stripDockerfileComments removes `#`-leading lines.
//
// コメントアウトして残すのは消すのと同じ。行頭の空白は Dockerfile が許すので
// 落としてから判定する。
func stripDockerfileComments(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// stripShellLineComments drops ` #` to end of line.
//
// **落としすぎる側に倒してある。** 文字列リテラルの中の ` #` まで落ちるが、
// この結果を使うのは「実行されるか」の判定だけで、builder かどうかの判定には
// 使わない (落としすぎて検査対象が減るのを避けるため)。
func stripShellLineComments(body string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, " #"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// foldContinuations joins Dockerfile line continuations into one line each.
//
// **畳まないと「同じ行に書いてあるか」で判定することになる。** `go build` の
// 引数が長くて折り返された瞬間にその Dockerfile が builder 集合から消えるので、
// ldflags を 1 つ足しただけで検査が止まる。
func foldContinuations(body string) string {
	var b strings.Builder
	joined := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimRight(line, " \t")
		cont := strings.HasSuffix(trimmed, "\\")
		trimmed = strings.TrimSuffix(trimmed, "\\")
		if joined {
			b.WriteByte(' ')
			b.WriteString(strings.TrimLeft(trimmed, " \t"))
		} else {
			b.WriteString(trimmed)
		}
		joined = cont
		if !cont {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// buildsMkGo reports whether the Dockerfile compiles the mk-go binary.
//
// **検出は広く取る。** 動詞と対象が同じコマンドに現れることを要求すると、
// 対象を `ARG MK_MAIN=./cmd/misskey` のような変数に入れただけで builder 集合から
// 黙って消える。**落ちたものは allowlist にも載らないので gate は鳴らない。**
// 余計に検査する側 (対象文字列がコメント外のどこかにあるだけ) は、pluginbuild を
// 呼ぶか理由を書くかを求めるだけなので安全側に倒れる。
func buildsMkGo(body string) bool {
	return containsAnyOf(body, mkgoBuildVerbs) && containsAnyOf(body, mkgoBuildTargets)
}

func containsAnyOf(body string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(body, n) {
			return true
		}
	}
	return false
}

// firstMkGoBuildIndex returns the offset of the first command that both invokes
// a build verb and names an mk-go target, or -1 when no single command does.
//
// **順序判定はここだけ狭く取る。** 行頭ではなく動詞そのものの位置を返す —
// 行継続を畳むと 1 つの RUN が 1 行になるので、行頭を返すと「同じ RUN の中で
// pluginbuild が go build より前か」を判定できない (常に後ろと判定される)。
// あわせて**コマンド単位に区切ってから**探す。区切らないと、同じ RUN の
// 先頭に置いた無関係な `go install` の位置が基準になり、正しい Dockerfile が
// 順序違反で落ちる。
func firstMkGoBuildIndex(body string) int {
	offset := 0
	for _, line := range strings.Split(body, "\n") {
		if i := mkGoBuildIndexInLine(line); i >= 0 {
			return offset + i
		}
		offset += len(line) + 1
	}
	return -1
}

// shellCommandSeparators splits a folded RUN into individual commands.
//
// 長いものを先に並べる (`&&` を `&` より先に見つける必要がある)。
var shellCommandSeparators = []string{"&&", "||", ";", "|", "&"}

func mkGoBuildIndexInLine(line string) int {
	offset, rest := 0, line
	for {
		cut, sepLen := len(rest), 0
		for _, sep := range shellCommandSeparators {
			if i := strings.Index(rest, sep); i >= 0 && i < cut {
				cut, sepLen = i, len(sep)
			}
		}
		if i := commandBuildIndex(rest[:cut]); i >= 0 {
			return offset + i
		}
		if sepLen == 0 {
			return -1
		}
		offset += cut + sepLen
		rest = rest[cut+sepLen:]
	}
}

// commandBuildIndex returns the verb position when one command builds mk-go.
func commandBuildIndex(cmd string) int {
	verb := -1
	for _, v := range mkgoBuildVerbs {
		if i := strings.Index(cmd, v); i >= 0 && (verb < 0 || i < verb) {
			verb = i
		}
	}
	if verb < 0 {
		return -1
	}
	for _, target := range mkgoBuildTargets {
		if strings.Contains(cmd, target) {
			return verb
		}
	}
	return -1
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

// TestLooksLikeDockerfile pins which file names the Dockerfile scans pick up.
func TestLooksLikeDockerfile(t *testing.T) {
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
		// Go / md のソースを混ぜると、doc やテストに書いた例を Dockerfile として
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
}
