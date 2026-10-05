// Command pluginbuild discovers plugins under plugins/ and generates the files
// that compile them into mk-go (#2480).
//
// # なぜビルド時なのか
//
// mk-go は CGO_ENABLED=0 + `-tags nodynamic` の完全静的バイナリ (#619) で、
// runtime stage は distroless (#621) のためシェルもインタプリタも持たない。
// Go の `-buildmode=plugin` は cgo と動的リンクを要求するので使えず、外部
// インタプリタの exec もできない。
//
// ビルド時に組み込む方式の利点は、内部の変更でプラグインが壊れたときに
// **ビルドが落ちて即座に分かる**こと。実行時読み込みだと本番で静かに壊れる。
//
// # 生成物
//
//	go.work                           plugins/* をワークスペースに含める
//	cmd/elythia/plugins_generated.go  各プラグインを import して Register する
//
// どちらも gitignore 済み。プラグインが 1 つも無ければ**何も生成しない**ので、
// 素の `go build ./...` はそのまま通る。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/shiroha-a/mk/plugin"
)

// markerFile marks a directory as an intentional plugin.
//
// `go.mod` の有無だけで判定すると、置き忘れた作業用ディレクトリを巻き込んで
// ビルドが落ちる。意図の宣言を別途要求する。
const markerFile = "mk-plugin.yml"

// generatedFile is written into the main package.
const generatedFile = "cmd/elythia/plugins_generated.go"

// legacyGeneratedFile is where generatedFile lived before the executables were
// merged into cmd/elythia (#3394).
//
// 古い checkout には gitignore された生成物だけが cmd/misskey/ に残る。main 関数の
// 無い package main になるので、残したままだと `go build ./...` が落ちる。
const legacyGeneratedFile = "cmd/misskey/plugins_generated.go"

// generatedHeaderPrefix starts every Go file this tool writes.
const generatedHeaderPrefix = "// Code generated"

// frontendEntry is the plugin-relative path of its frontend entry point.
// 存在しなければ backend だけのプラグインとして扱う。
const frontendEntry = "frontend/index.ts"

// Frontend generation targets, relative to the repository root.
const (
	frontendGeneratedTS   = "frontend/packages/frontend/src/server-plugins.generated.ts"
	frontendManifestJSON  = "frontend/packages/frontend/mk-plugins.generated.json"
	frontendAliasPrefix   = "@mkplugin/"
	frontendSrcRelToFront = "frontend/packages/frontend"
)

// manifest is the marker file's content.
type manifest struct {
	// Name is informational; the authoritative name is the Go Definition's.
	Name string `yaml:"name"`
	// APIVersion lets us fail before compiling, with a message that says what
	// is wrong. これが無いと Go のコンパイルエラーか起動時 panic になり、
	// 「プラグインが古い」ことが読み取れない。
	APIVersion int `yaml:"apiVersion"`
	// Disabled excludes the plugin from the build entirely.
	//
	// 設定ファイルの `enabled: false` (実行時スキップ) と違い、バイナリにも
	// フロントのバンドルにも入らない。同梱サンプルの既定値や、今はビルド
	// できないプラグインの一時退避に使う。
	Disabled bool `yaml:"disabled"`
}

// discovered is one plugin found under plugins/.
type discovered struct {
	dir        string
	modulePath string
	name       string
	// hasFrontend reports whether frontend/index.ts exists.
	hasFrontend bool
	// disabled records the marker's disabled flag (only reachable when an
	// include option covers the plugin; otherwise it never gets this far).
	disabled bool
}

// include controls which disabled plugins are still built.
type include struct {
	// all includes every disabled plugin. CI がサンプルを検証し続けるための
	// 経路 (make plugins-all)。
	all bool
	// dir includes only the plugin at this repo-relative directory.
	// plugin-dev が**監視対象だけ**を含めるための経路。all にしてしまうと、
	// disabled で退避中の壊れたプラグインまで巻き込んで、無関係な開発の
	// 生成・ビルドが止まる。
	dir string
}

// covers reports whether a disabled plugin at rel should be included anyway.
func (in include) covers(rel string) bool {
	return in.all || (in.dir != "" && filepath.Clean(in.dir) == filepath.Clean(rel))
}

func main() {
	root, dir, inc := parseArgs(os.Args[1:])
	if err := run(root, dir, inc); err != nil {
		fmt.Fprintln(os.Stderr, "pluginbuild:", err)
		os.Exit(1)
	}
}

// parseArgs parses the command-line flags. Invalid flags and -h exit the
// process, as the default flag.CommandLine does.
//
// main から切り出してあるのはテストのため。main はテストから呼べないので、
// Go 1.27 でカバレッジのブロックが細かく数えられるようになると、小さい
// このパッケージでは main だけで閾値 (90%) を割った。
func parseArgs(args []string) (root, dir string, inc include) {
	fs := flag.NewFlagSet("pluginbuild", flag.ExitOnError)
	rootFlag := fs.String("root", ".", "リポジトリのルート")
	dirFlag := fs.String("dir", "plugins", "プラグインを探すディレクトリ")
	includeDisabled := fs.Bool("include-disabled", false, "disabled: true のプラグインも含める (CI がサンプルを検証するための経路)")
	includeDisabledDir := fs.String("include-disabled-dir", "", "このディレクトリのプラグインだけは disabled でも含める (plugin-dev が監視対象を動かすための経路)")
	_ = fs.Parse(args) // ExitOnError なので失敗時は Parse の中で終了する
	return *rootFlag, *dirFlag, include{all: *includeDisabled, dir: *includeDisabledDir}
}

func run(root, pluginDir string, inc include) error {
	found, err := discover(root, pluginDir, inc)
	if err != nil {
		return err
	}
	if err := removeLegacyGenerated(root); err != nil {
		return err
	}

	genPath := filepath.Join(root, generatedFile)
	workPath := filepath.Join(root, "go.work")

	if len(found) == 0 {
		// 前回のビルドの残骸を消す。残すと、消したはずのプラグインが
		// 組み込まれたままになる。
		if err := removeIfExists(genPath); err != nil {
			return err
		}
		if err := removeIfExists(workPath); err != nil {
			return err
		}
		// フロント側の生成物も戻す。ここを飛ばすと、消したはずのプラグインを
		// import したままの TS が残ってビルドが落ちる。
		if err := writeFrontend(root, nil); err != nil {
			return err
		}
		fmt.Println("pluginbuild: プラグインはありません")
		return nil
	}

	goVersion, err := goDirective(filepath.Join(root, "go.mod"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(workPath, []byte(renderWorkspace(found, goVersion)), 0o644); err != nil {
		return fmt.Errorf("go.work を書けません: %w", err)
	}
	if err := os.WriteFile(genPath, []byte(renderRegistration(found)), 0o644); err != nil {
		return fmt.Errorf("%s を書けません: %w", generatedFile, err)
	}

	if err := writeFrontend(root, found); err != nil {
		return err
	}

	for _, p := range found {
		fmt.Println(formatDiscovered(p))
	}
	return nil
}

// formatDiscovered renders one line of the plugin summary.
//
// **dir を出すのが要点。** name は mk-plugin.yml の `name:` で、置いたディレクトリ
// 名とは限らない。呼び出し側 (build-with-plugins workflow) は「要求したプラグインが
// 実際に組み込まれたか」を突き合わせるので、運営者が指定できる唯一の識別子である
// ディレクトリを出す必要がある。
//
// **dir を name より前に置く。** name は無検証の YAML 文字列で括弧も改行も入れられる
// ので、後ろに置くと `name: "x dir=plugins/victim "` のように別プラグインの行を
// 偽装でき、**無効化されたプラグインが組み込まれたと判定される**。
func formatDiscovered(p discovered) string {
	note := ""
	if p.disabled {
		note = ", disabled だが明示指定で含めた"
	}
	return fmt.Sprintf("pluginbuild: dir=%s name=%s (%s, frontend=%t%s)",
		p.dir, p.name, p.modulePath, p.hasFrontend, note)
}

// writeFrontend generates the files the frontend's Vite build reads.
//
// frontend を持たないプラグインしか無い場合でも、生成物は「空」の形で書く。
// frontend はこのファイルを import するので、無いとビルドが落ちる。生成物は
// 追跡しない (#3379。追跡すると `make plugins` のたびに作業ツリーが dirty になり、
// 運営者の `git pull` が止まりうる)。
func writeFrontend(root string, found []discovered) error {
	withFrontend := make([]discovered, 0, len(found))
	for _, p := range found {
		if p.hasFrontend {
			withFrontend = append(withFrontend, p)
		}
	}

	tsPath := filepath.Join(root, frontendGeneratedTS)

	// frontend/ が無い木 (Go だけを取り出した build context など) でも Go だけの
	// ビルドは通したい。ただし frontend を持つプラグインがあるのに書けない場合は
	// **黙って落とさない** — 機能が片肺で組み込まれ、動かない理由が分からなくなる。
	if _, err := os.Stat(filepath.Dir(tsPath)); os.IsNotExist(err) {
		if len(withFrontend) == 0 {
			return nil
		}
		return fmt.Errorf(
			"frontend を持つプラグインがありますが %s がありません (frontend/ を含む checkout で実行してください)",
			filepath.Dir(frontendGeneratedTS))
	}

	if err := os.WriteFile(tsPath, []byte(renderFrontendList(withFrontend)), 0o644); err != nil {
		return fmt.Errorf("%s を書けません: %w", frontendGeneratedTS, err)
	}

	manifestPath := filepath.Join(root, frontendManifestJSON)
	if len(withFrontend) == 0 {
		// alias も fs.allow も要らないので、生成物ごと消す。vite.config は
		// 不在なら空として扱う。
		return removeIfExists(manifestPath)
	}

	m := struct {
		Aliases map[string]string `json:"aliases"`
		Allow   []string          `json:"allow"`
	}{Aliases: map[string]string{}, Allow: []string{}}

	// vite.config からの**相対**パスで書く。絶対パスを書くと、生成した環境でしか
	// ビルドできない成果物になる (docker build は別の場所に展開される)。
	// vite.config 側が __dirname で絶対パスへ解決する。
	//
	// **エントリファイルまで指す。** ディレクトリを指すと rolldown が index を
	// 自動解決せず "Is a directory" で落ちる (tsconfig の paths は解決するので、
	// 型チェックだけでは気付けない)。
	for _, p := range withFrontend {
		rel, err := filepath.Rel(frontendSrcRelToFront, filepath.Join(p.dir, frontendEntry))
		if err != nil {
			return fmt.Errorf("plugin %s の frontend パスを解決できません: %w", p.name, err)
		}
		m.Aliases[frontendAliasPrefix+p.name] = filepath.ToSlash(rel)
	}
	// dev server が配信を許す範囲。plugins/ をまとめて許可する。
	m.Allow = append(m.Allow, filepath.ToSlash(mustRel(frontendSrcRelToFront, "plugins")))

	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("frontend manifest を組み立てられません: %w", err)
	}
	if err := os.WriteFile(manifestPath, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("%s を書けません: %w", frontendManifestJSON, err)
	}
	return nil
}

// mustRel is filepath.Rel with the error folded into the result. base / target
// はどちらもリポジトリ相対の固定値なので、実行時に失敗しない。
func mustRel(base, target string) string {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return rel
}

// renderFrontendList builds server-plugins.generated.ts.
func renderFrontendList(found []discovered) string {
	var b strings.Builder
	b.WriteString("/*\n * SPDX-FileCopyrightText: syuilo and misskey-project\n")
	b.WriteString(" * SPDX-License-Identifier: AGPL-3.0-only\n */\n\n")
	b.WriteString("/*\n * Code generated by mk-go tools/pluginbuild. DO NOT EDIT.\n")
	b.WriteString(" *\n * plugins/ にサーバープラグインが置かれるとここが書き換わる (mk-go #2479)。\n")
	b.WriteString(" * 既定は空なので、プラグインを使わないビルドでは何も読み込まれない。\n */\n\n")
	b.WriteString("import type { PluginDefinition } from '@/plugin-api.js';\n")
	for i, p := range found {
		fmt.Fprintf(&b, "import p%d from '%s%s';\n", i, frontendAliasPrefix, p.name)
	}
	b.WriteString("\nexport const serverPlugins: PluginDefinition[] = [")
	for i := range found {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "p%d", i)
	}
	b.WriteString("];\n")
	return b.String()
}

// discover walks root/pluginDir and validates each candidate.
//
// **dir はリポジトリルートからの相対パスで返す。** go.work の `use` は go.work
// の位置からの相対で解決されるので、ルート込みの絶対パスを書くと別の場所で
// ビルドできない成果物になる。
func discover(root, pluginDir string, inc include) ([]discovered, error) {
	entries, err := os.ReadDir(filepath.Join(root, pluginDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s を読めません: %w", pluginDir, err)
	}

	var found []discovered
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rel := filepath.Join(pluginDir, e.Name())
		dir := filepath.Join(root, rel)

		// マーカーが無いものは**黙って飛ばす**。作業用ディレクトリを置いた
		// だけでビルドが落ちるのは扱いづらい。
		markerPath := filepath.Join(dir, markerFile)
		raw, err := os.ReadFile(markerPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s を読めません: %w", markerPath, err)
		}

		var m manifest
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("%s を解釈できません: %w", markerPath, err)
		}

		// disabled は**検証より先に**見る。apiVersion が合わなくなった等の
		// 「今はビルドできない」プラグインを、ディレクトリを消さずに外せる
		// ようにするため。後ろに置くと、無効化したのにビルドが止まる。
		if m.Disabled && !inc.covers(rel) {
			fmt.Printf("pluginbuild: %s は無効化されています (mk-plugin.yml の disabled: true)\n", e.Name())
			continue
		}

		if m.APIVersion != plugin.APIVersion {
			return nil, fmt.Errorf(
				"%s: apiVersion %d はこの mk-go (apiVersion %d) と互換がありません",
				dir, m.APIVersion, plugin.APIVersion)
		}

		// **go.mod を必須にする。** 無いと mk-go と同一モジュールになり、Go の
		// internal ルール上 internal/ が見えてしまう。plugin/ だけを公開面と
		// する設計 (#2476) がここで静かに崩れる。
		modPath, err := modulePath(filepath.Join(dir, "go.mod"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}

		name := m.Name
		if name == "" {
			name = e.Name()
		}
		_, ferr := os.Stat(filepath.Join(dir, frontendEntry))
		found = append(found, discovered{
			dir: rel, modulePath: modPath, name: name,
			hasFrontend: ferr == nil,
			disabled:    m.Disabled,
		})
	}

	// ディレクトリ名で並べる。順序が環境依存だとビルドの再現性が失われる。
	sort.Slice(found, func(i, j int) bool { return found[i].dir < found[j].dir })
	return found, nil
}

// goDirective reads the `go` line out of a go.mod.
func goDirective(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s を読めません: %w", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok {
			if v := strings.TrimSpace(rest); v != "" {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("%s に go directive がありません", path)
}

// modulePath reads the module line out of a go.mod.
func modulePath(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", fmt.Errorf("go.mod がありません (プラグインは独立した Go module である必要があります)")
	}
	if err != nil {
		return "", fmt.Errorf("go.mod を読めません: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			if p := strings.TrimSpace(rest); p != "" {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("go.mod に module 行がありません")
}

// renderWorkspace builds go.work.
//
// go directive は本体の go.mod から写す。**決め打ちにしない**: go.work の方が
// 新しい版を要求すると Go はその toolchain を取りに行き、無ければ
// 「toolchain not available」でビルド全体が止まる。本体と同じ版なら必ず手元に
// ある。
func renderWorkspace(found []discovered, goVersion string) string {
	var b strings.Builder
	b.WriteString("// Code generated by tools/pluginbuild. DO NOT EDIT.\n\n")
	b.WriteString("go " + goVersion + "\n\nuse (\n\t.\n")
	for _, p := range found {
		// go.work は常に / 区切り。
		b.WriteString("\t./" + filepath.ToSlash(p.dir) + "\n")
	}
	b.WriteString(")\n")
	return b.String()
}

// renderRegistration builds the generated main-package file.
//
// import alias は連番にする。プラグイン名をそのまま識別子に使うと、ハイフンを
// 含む名前 (game-info) が Go の識別子として不正になる。
func renderRegistration(found []discovered) string {
	var b strings.Builder
	b.WriteString("// Code generated by tools/pluginbuild. DO NOT EDIT.\n\n")
	b.WriteString("package main\n\nimport (\n")
	b.WriteString("\t\"github.com/shiroha-a/mk/plugin\"\n\n")
	for i, p := range found {
		fmt.Fprintf(&b, "\tp%d %q\n", i, p.modulePath)
	}
	b.WriteString(")\n\nfunc init() {\n")
	for i := range found {
		fmt.Fprintf(&b, "\tplugin.Register(p%d.Plugin)\n", i)
	}
	b.WriteString("}\n")
	return b.String()
}

// removeLegacyGenerated deletes legacyGeneratedFile and, when that leaves
// its directory empty, the directory too.
//
// **生成物の見出しで始まるときだけ消す。** 同じ名前で利用者が手で置いたファイル
// かもしれないので、見出しが無ければ触らずに警告だけ出す。ディレクトリも、
// 他のファイルが残っていれば触らない。
func removeLegacyGenerated(root string) error {
	path := filepath.Join(root, legacyGeneratedFile)
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s を読めません: %w", legacyGeneratedFile, err)
	}
	if !strings.HasPrefix(string(body), generatedHeaderPrefix) {
		fmt.Fprintf(os.Stderr, "pluginbuild: %s は生成物の見出しで始まらないので消しません。"+
			"残っていると go build ./... が落ちるので、不要なら手で消してください\n", legacyGeneratedFile)
		return nil
	}
	if err := removeIfExists(path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		return removeIfExists(dir)
	}
	return nil
}

// removeIfExists deletes path when present.
func removeIfExists(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%s を消せません: %w", path, err)
	}
	return nil
}
