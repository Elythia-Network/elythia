package frontendutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/labstack/echo/v4"
)

const frontendBase = "third_party/misskey"

// FrontendDir returns the path to built frontend assets.
// 環境変数 MISSKEY_FRONTEND_DIR で上書き可能。
func FrontendDir() string {
	if v := os.Getenv("MISSKEY_FRONTEND_DIR"); v != "" {
		return v
	}
	return filepath.Join(frontendBase, "built", "_frontend_vite_")
}

// FrontendEmbedDir returns the path to built embed frontend assets.
// 環境変数 MISSKEY_FRONTEND_EMBED_DIR で上書き可能。
//
// embed は通常の SPA とは別の vite build で、bundle も entry も別物
// (`/embed_vite/` 配下で配信し、entry は src/boot.ts)。
//
// 既定値を FrontendDir の sibling として解決するのは SwDistDir と同じ理由。
// deploy 側は MISSKEY_FRONTEND_DIR しか指していないことが多く (本番 compose /
// federation Dockerfile ともにそう)、embed だけ別の環境変数を要求すると
// **設定漏れに気付けないまま /embed_vite/* が 404 になる** (以前は dev server
// proxy へ落ちて 502 だった。今は dev モードでしか proxy しない)。
// 実際 Playwright を通すまでこの状態に気付けなかった。
func FrontendEmbedDir() string {
	if v := os.Getenv("MISSKEY_FRONTEND_EMBED_DIR"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(FrontendDir()), "_frontend_embed_vite_")
}

// FrontendDistDir returns the path to _frontend_dist_ assets (locales, fonts).
// 環境変数 MISSKEY_FRONTEND_DIST_DIR で上書き可能。
func FrontendDistDir() string {
	if v := os.Getenv("MISSKEY_FRONTEND_DIST_DIR"); v != "" {
		return v
	}
	return filepath.Join(frontendBase, "built", "_frontend_dist_")
}

// SwDistDir returns the path to _sw_dist_ assets (service worker sw.js).
// 環境変数 MISSKEY_SW_DIST_DIR で上書き可能。Frontend build の
// sibling ディレクトリに出力されるのでデフォルトは FrontendDir の親 +
// "_sw_dist_"。
func SwDistDir() string {
	if v := os.Getenv("MISSKEY_SW_DIST_DIR"); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(FrontendDir()), "_sw_dist_")
}

// ClientAssetsDir returns the path to frontend client assets (game images, etc.).
// 環境変数 MISSKEY_CLIENT_ASSETS_DIR で上書き可能。
func ClientAssetsDir() string {
	if v := os.Getenv("MISSKEY_CLIENT_ASSETS_DIR"); v != "" {
		return v
	}
	return filepath.Join(frontendBase, "packages", "frontend", "assets")
}

// TwemojiDir returns the path to twemoji SVG files.
// 環境変数 MISSKEY_TWEMOJI_DIR で上書き可能。pnpm の hoisted node_modules は
// packages/backend 配下に配置されるため、そちらを参照する。
//
// upstream 2026.5.2 #17381 で `@discordapp/twemoji/dist/svg` から
// `@misskey-dev/emoji-assets/built/twemoji` に asset 配信元が移行した。
// drop-in 互換のため mk-go も同 path を参照する (旧 path は submodule bump 後
// に node_modules から消えるため falls back しない)。
func TwemojiDir() string {
	if v := os.Getenv("MISSKEY_TWEMOJI_DIR"); v != "" {
		return v
	}
	return filepath.Join(frontendBase, "packages", "backend", "node_modules", "@misskey-dev", "emoji-assets", "built", "twemoji")
}

// FluentEmojiDir returns the path to fluent-emoji PNG files. frontend の
// `ACHIEVEMENT_BADGES` (= 実績バッジ) や notification icon が `/fluent-emoji/
// <hex>.png` の URL で参照する。環境変数 `MISSKEY_FLUENT_EMOJI_DIR` で上書き
// 可能。
//
// upstream 2026.5.2 #17381 (twemoji と同 commit) で fluent-emoji の serve path
// が `fluent-emojis/dist` (submodule) から
// `node_modules/@misskey-dev/emoji-assets/built/fluent-emoji` (= 自前 npm
// パッケージ) に移行した。drop-in 互換のため mk-go も同 path を参照する
// (旧 submodule path は将来 upstream で削除予定)。
//
// 既知の broken: 新 path には keycap 系 (`0031-20e3.png` = 1️⃣ 等) が含まれない
// ため `passedSinceAccountCreated1/2/3` achievement の bronze/silver/gold バッジ
// icon が 404 になる。これは upstream Misskey TS 2026.5.4 でも同じ状態で、
// upstream achievements.ts 側の bug。本家 fix を待つか、shiroha-a/misskey-ts
// fork で代替 emoji に rename する patch を入れる follow-up (#1167) を別途
// 計画する。
func FluentEmojiDir() string {
	if v := os.Getenv("MISSKEY_FLUENT_EMOJI_DIR"); v != "" {
		return v
	}
	return filepath.Join(frontendBase, "packages", "backend", "node_modules", "@misskey-dev", "emoji-assets", "built", "fluent-emoji")
}

// StaticDir returns the path to static assets (icons, splash, favicon etc.).
// 環境変数 MISSKEY_STATIC_DIR で上書き可能。本家 TS では packages/backend/assets
// が /static-assets として配信される。
func StaticDir() string {
	if v := os.Getenv("MISSKEY_STATIC_DIR"); v != "" {
		return v
	}
	return filepath.Join(frontendBase, "packages", "backend", "assets")
}

// RepoAssetsDir returns the path to the top-level repository assets directory
// (ai.png, banner images, etc.).
// 環境変数 MISSKEY_REPO_ASSETS_DIR で上書き可能。
func RepoAssetsDir() string {
	if v := os.Getenv("MISSKEY_REPO_ASSETS_DIR"); v != "" {
		return v
	}
	return filepath.Join(frontendBase, "assets")
}

// ClientEntryInfo holds the Vite entry point script, CSS dependencies, and
// module preloads collected from the manifest by walking the entry's import chain.
type ClientEntryInfo struct {
	Script         string
	CSS            []string
	ModulePreloads []string
}

// DetectClientEntry reads the Vite manifest to find the entry script and all
// CSS files (= entry の直接 CSS + entry が import する全 chunk の CSS を再帰的に
// 集めたもの)。upstream TS の HtmlTemplateService#collectViteAssetFiles と同じ動作。
// Vite は SFC `<style scoped>` を chunk 単位の CSS file に切り出すため、entry が
// 同期 import する chunk の CSS も最初に link しないと、それらの component (例:
// MkCustomEmoji / MkMention) のスタイルが当たらない。ビルド済みアセットが存在し
// ない場合は空値を返す (dev mode)。
func DetectClientEntry() ClientEntryInfo {
	return detectEntry(FrontendDir(), "src/_boot_.ts")
}

// DetectEmbedEntry is the embed-bundle counterpart of DetectClientEntry.
//
// embed は別の vite build なので manifest も entry key も別 (`src/boot.ts`)。
// CSS の再帰収集は通常版と同じ理由で必要 — vite は SFC の `<style scoped>` を
// chunk 単位の CSS に切り出すため、entry が同期 import する chunk の CSS も
// link しないとスタイルが当たらない。
func DetectEmbedEntry() ClientEntryInfo {
	return detectEntry(FrontendEmbedDir(), "src/boot.ts")
}

// detectEntry reads a Vite manifest under dir and resolves entryKey.
func detectEntry(dir, entryKey string) ClientEntryInfo {
	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return ClientEntryInfo{}
	}
	type manifestChunk struct {
		File    string   `json:"file"`
		IsEntry bool     `json:"isEntry"`
		CSS     []string `json:"css"`
		Imports []string `json:"imports"`
	}
	var manifest map[string]manifestChunk
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ClientEntryInfo{}
	}
	entry, ok := manifest[entryKey]
	if !ok {
		return ClientEntryInfo{}
	}

	seenChunks := map[string]struct{}{}
	seenCSS := map[string]struct{}{}
	var cssFiles []string
	var modulePreloads []string

	addCSS := func(files []string) {
		for _, f := range files {
			if _, dup := seenCSS[f]; dup {
				continue
			}
			seenCSS[f] = struct{}{}
			cssFiles = append(cssFiles, f)
		}
	}
	addCSS(entry.CSS)

	var walk func(imports []string, recursive bool)
	walk = func(imports []string, recursive bool) {
		for _, id := range imports {
			if _, dup := seenChunks[id]; dup {
				continue
			}
			seenChunks[id] = struct{}{}
			chunk, ok := manifest[id]
			if !ok {
				continue
			}
			addCSS(chunk.CSS)
			if len(chunk.Imports) > 0 {
				walk(chunk.Imports, true)
			}
			// modulePreloads は entry が同期 import する 1 hop の chunk だけ
			// (= recursive=false の呼び出し時のみ)。upstream と同じ挙動。
			if !recursive {
				modulePreloads = append(modulePreloads, chunk.File)
			}
		}
	}
	walk(entry.Imports, false)

	return ClientEntryInfo{
		Script:         entry.File,
		CSS:            cssFiles,
		ModulePreloads: modulePreloads,
	}
}

// AssetsHandler returns a handler that tries to serve files from primary dir
// first, then falls back to fallback dir. This avoids Echo's limitation of
// only supporting one handler per route pattern.
func AssetsHandler(primary, fallback string) echo.HandlerFunc {
	return func(c echo.Context) error {
		name := c.Param("*")
		// primaryディレクトリから探す
		fp := filepath.Join(primary, filepath.Clean("/"+name))
		if info, err := os.Stat(fp); err == nil && !info.IsDir() {
			return c.File(fp)
		}
		// fallbackディレクトリから探す
		fp = filepath.Join(fallback, filepath.Clean("/"+name))
		if info, err := os.Stat(fp); err == nil && !info.IsDir() {
			return c.File(fp)
		}
		return echo.ErrNotFound
	}
}

// LoaderAssets holds the bootloader CSS / JS that upstream inlines into the
// HTML shell (`HtmlTemplateService.prepareFrontendAssets`).
type LoaderAssets struct {
	CSS string
	JS  string
}

var (
	loaderOnce       sync.Once
	loaderCache      LoaderAssets
	embedLoaderOnce  sync.Once
	embedLoaderCache LoaderAssets
)

// BootLoaderAssets returns the SPA bootloader assets, read once per process.
//
// **参照のままにすると loader の更新が閲覧側に届かない (#2551)。**
// `loader/style.css` と `loader/boot.js` はファイル名にハッシュが付かないので、
// 内容を変えても URL が変わらない。CDN を挟むと HTML だけが新しくなり、
// 「新しいマークアップに古い CSS が当たる」状態で表示が壊れる。
//
// 読み込みは 1 回だけ。**再ビルド後は再起動が要る**が、これは client entry
// (DetectClientEntry) が既にそうなので前提は変わらない。
func BootLoaderAssets() LoaderAssets {
	loaderOnce.Do(func() { loaderCache = readLoaderAssets(FrontendDir()) })
	return loaderCache
}

// EmbedLoaderAssets is BootLoaderAssets for the embed bundle.
func EmbedLoaderAssets() LoaderAssets {
	embedLoaderOnce.Do(func() { embedLoaderCache = readLoaderAssets(FrontendEmbedDir()) })
	return embedLoaderCache
}

func readLoaderAssets(dir string) LoaderAssets {
	return LoaderAssets{
		CSS: readInlinableAsset(filepath.Join(dir, "loader", "style.css")),
		JS:  readInlinableAsset(filepath.Join(dir, "loader", "boot.js")),
	}
}

// readInlinableAsset reads a file that will be pasted verbatim into a
// `<style>` / `<script>` element. 読めなければ空文字を返し、呼び出し側は
// 従来どおり URL 参照に落とす。
//
// **閉じタグに化ける並びを含むものは埋め込まない。** `<style>` / `<script>` の
// 中身はエスケープが効かず、`</script` ひとつで後続が HTML として解釈される。
// 自前のビルド成果物とはいえ、埋め込む先が生テキストである以上ここで見る。
func readInlinableAsset(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	s := string(b)
	lower := strings.ToLower(s)
	for _, bad := range []string{"</script", "</style", "<!--"} {
		if strings.Contains(lower, bad) {
			return ""
		}
	}
	return s
}

// ResetLoaderCacheForTest clears the once-per-process loader cache.
//
// 本番では使わない。**キャッシュは意図的に 1 回きり**なので、環境変数で
// ディレクトリを差し替える test 同士が干渉しないよう明示的に捨てる口だけ出す。
func ResetLoaderCacheForTest() {
	loaderOnce = sync.Once{}
	embedLoaderOnce = sync.Once{}
	loaderCache = LoaderAssets{}
	embedLoaderCache = LoaderAssets{}
}
