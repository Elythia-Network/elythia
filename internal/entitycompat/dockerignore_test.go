package entitycompat

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

// excludedFromBuildContext lists paths that must never reach a build context,
// with the reason each one matters.
//
// **「配る image に入る」ではない。** mk-go をビルドする Dockerfile はどれも最終
// stage が明示パスの `COPY --from=builder` しか持たないので、context に入った
// ファイルが配布物へ出ることはない (#2942 で実測: 除外を外して build しても
// 最終 image の `drive-files` は 0 件、builder stage には 2,790 件。image の
// digest も除外の有無で変わらない)。
//
// 守っているのは **build context と builder stage の layer** で、そこが漏れる
// 経路は 2 つある。(a) `cache-to` を設定していると builder layer がキャッシュへ
// 書き出され、そのキャッシュを読める相手に中身が渡る。(b) 手元の builder cache に
// 滞留する。加えて転送量にも効く。
//
// **サイズだけの理由でここへ足さないこと。** コンテキストが太っても転送が遅く
// なるだけで、ビルドは通るし気付ける。ここは「中身が読まれると困るもの」に限る。
var excludedFromBuildContext = map[string]string{
	".git/config":                   "履歴と remote の認証情報が入る",
	"plugins/example/.git/config":   "入れ子の .git も落とす (plugins/*/ は独立リポジトリ)",
	".env":                          "運用の設定ファイル (作られていれば認証情報を含む)",
	".env.test":                     "同上。`.env.test.example` から作る手順が docs/testing.md にある",
	"drive-files/abcdef.png":        "利用者がアップロードしたファイル。既定の drive の置き場所 (internal/server/router.go)",
	".config/default.yml":           "operator-local な設定。DB / Redis のパスワードを持つ",
	"deploy/uds/config/default.yml": "同上 (本番 UDS)",
	"compose.uds.yaml":              "本番 UDS の compose。environment にパスワード類を持つ",
	".pnpm-store/v3/files/00/abc":   "pnpm のストア。実測 3.8GB でコンテキストの大半を占める",
}

// keptInBuildContext lists paths the Dockerfiles need, so that tightening an
// exclusion cannot silently break a build.
//
// **除外側だけを見る gate では書けない検査。** 「何が落ちるか」ではなく
// 「何が残るか」を固定しておかないと、`.config/*.yml` を `.config/*` に広げた
// ような変更が **COPY を壊すまで気付けない**。
//
// **判定は字句だけで、パスが実在するかも symlink の先も見ない。** 代表パスは
// 「今日そこにある」ものを選んであるが、submodule が構造を変えれば実在しなく
// なる (判定の妥当性は変わらない)。symlink については、辿らないからこそ
// **辿った先と辿る前の両方**を一覧に入れてある。
//
// root context の Dockerfile が COPY する元は実測 20 箇所ある。全部は並べず、
// **触られやすいもの** (同じディレクトリに既に除外が並んでいる / 除外を広げる
// 誘惑がある) に絞ってある。
var keptInBuildContext = map[string]string{
	".config/docker.yml.example":                              "Dockerfile / Dockerfile.bundled が /app/.config/default.yml として COPY する",
	"third_party/misskey/built/meta.json":                     "SPA の成果物。assets-local stage が built ごと COPY する",
	"third_party/misskey/packages/backend/assets/favicon.ico": "builder stage が submodule の初期化チェックに使う",
	"tests/federation/common/mkgo-entrypoint.sh":              "連合 e2e の Dockerfile が COPY する。`tests/` はここに 4 つ除外が並んでいて blanket 除外に倒れやすい",
	// **symlink 経路と実体経路の両方を持つ。** Dockerfile が COPY するのは
	// symlink 側 (`packages/backend/node_modules/...`) だが、pnpm は実体を
	// `.pnpm/` 配下に置く。判定は字句だけで symlink を辿らないので、**片方しか
	// 守らないと `**/node_modules` や `packages/*/node_modules` への変更で
	// symlink が落ち、gate は緑のまま全ビルドが壊れる** (実測)。しかもその形は
	// `.dockerignore` 自身が名指しで警告しているもので、guard のエラーは
	// 「pnpm install not run?」と事実と逆を指す。
	"third_party/misskey/packages/backend/node_modules/@misskey-dev/emoji-assets/built/twemoji/1f004.svg":                                    "Dockerfile が COPY する symlink 経路",
	"third_party/misskey/node_modules/.pnpm/@misskey-dev+emoji-assets@17.0.3/node_modules/@misskey-dev/emoji-assets/built/twemoji/1f004.svg": "同じものの実体経路。`!` の再包含が効いているかを見る",
}

// TestDockerignoreExcludesSecretsAndUserData checks the shared .dockerignore
// against Docker's own matcher.
//
// **自前で解釈しない。** `.dockerignore` は `**/` の前置・末尾スラッシュ・先頭
// スラッシュ・`*` を挟む形・`!` の後勝ちが絡み、文字列の正規化で近似すると必ず
// 取りこぼす (敵対的レビューで、自前実装が 8 形中 7 形を見逃すことを実測された)。
// #2857 が「Makefile を自前でパースせず `make -n` に解決させる」と結論したのと
// 同じ形で、ここも `moby/patternmatcher` (Docker 本体が使う実装) に解かせる。
//
// `ignorefile.ReadAll` と組にするのが要点 — 先頭スラッシュの除去やコメントの
// 扱いはそちらの仕事で、`patternmatcher` 単体だと `!/drive-files` を取り逃がす。
func TestDockerignoreExcludesSecretsAndUserData(t *testing.T) {
	// 一覧そのものが空になったら落とす。空でも「全部あった」として通るので、
	// 検査していないのに緑になる。
	if len(excludedFromBuildContext) == 0 || len(keptInBuildContext) == 0 {
		t.Fatal("検査対象の一覧が空です")
	}

	patterns, err := ignorefile.ReadAll(strings.NewReader(readRepoFile(t, ".dockerignore")))
	if err != nil {
		t.Fatalf(".dockerignore を解釈できません: %v", err)
	}
	if len(patterns) == 0 {
		t.Fatal(".dockerignore からパターンを 1 つも読めませんでした")
	}

	for path, reason := range excludedFromBuildContext {
		excluded, err := patternmatcher.MatchesOrParentMatches(path, patterns)
		if err != nil {
			t.Fatalf("%s の判定に失敗しました: %v", path, err)
		}
		if !excluded {
			t.Errorf("%s が build context に入ります (%s)。"+
				".dockerignore は全 build context に効くので、builder stage の layer にも載り、"+
				"cache-to を設定していればキャッシュ経由で読めます", path, reason)
		}
	}

	for path, reason := range keptInBuildContext {
		excluded, err := patternmatcher.MatchesOrParentMatches(path, patterns)
		if err != nil {
			t.Fatalf("%s の判定に失敗しました: %v", path, err)
		}
		if excluded {
			t.Errorf("%s が build context から落ちています (%s)。"+
				"除外を広げるときは COPY 元を巻き込んでいないか確認してください", path, reason)
		}
	}
}

// TestDockerignoreIsTheOnlyOne checks that no per-Dockerfile ignore file exists.
//
// BuildKit は Dockerfile の隣に `<Dockerfile名>.dockerignore` があると、**root の
// `.dockerignore` を一切見ない**。つまりファイル 1 つ置くだけで上のテストの前提が
// 静かに崩れ、それでも gate は緑のままになる (#2857 の「黙って検査が止まる」型)。
//
// **tracked なものしか見ない。** untracked のまま置かれたものは検出できないが、
// CI は clean checkout なので CI 上のビルドには影響しない (`gaterun-check` が
// `git ls-files` を選んだ理由とは向きが逆で、こちらは「手元だけの状態」を
// 意図的に対象外にしている)。
func TestDockerignoreIsTheOnlyOne(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "*.dockerignore").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	for _, path := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if path == "" || path == ".dockerignore" {
			continue
		}
		t.Errorf("%s があると、その Dockerfile では root の .dockerignore が使われません。"+
			"シークレットの除外が効かなくなるので、root の .dockerignore へ寄せてください", path)
	}
}
