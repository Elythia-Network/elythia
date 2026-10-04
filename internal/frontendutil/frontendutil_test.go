package frontendutil

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Dir functions ---

func TestFrontendDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIR", "")
	assert.Equal(t, filepath.Join("frontend", "built", "_frontend_vite_"), FrontendDir())
}

func TestFrontendDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIR", "/custom/frontend")
	assert.Equal(t, "/custom/frontend", FrontendDir())
}

func TestFrontendDistDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIST_DIR", "")
	assert.Equal(t, filepath.Join("frontend", "built", "_frontend_dist_"), FrontendDistDir())
}

func TestFrontendDistDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIST_DIR", "/custom/dist")
	assert.Equal(t, "/custom/dist", FrontendDistDir())
}

func TestSwDistDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_SW_DIST_DIR", "")
	t.Setenv("MISSKEY_FRONTEND_DIR", "")
	assert.Equal(t, filepath.Join("frontend", "built", "_sw_dist_"), SwDistDir())
}

func TestSwDistDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_SW_DIST_DIR", "/custom/sw")
	assert.Equal(t, "/custom/sw", SwDistDir())
}

func TestClientAssetsDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_CLIENT_ASSETS_DIR", "")
	assert.Equal(t, filepath.Join("frontend", "packages", "frontend", "assets"), ClientAssetsDir())
}

func TestClientAssetsDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_CLIENT_ASSETS_DIR", "/custom/client")
	assert.Equal(t, "/custom/client", ClientAssetsDir())
}

func TestTwemojiDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_TWEMOJI_DIR", "")
	// upstream 2026.5.2 #17381 で twemoji の serve path が
	// @discordapp/twemoji/dist/svg → @misskey-dev/emoji-assets/built/twemoji に
	// 移行した (#1164 Phase A)。
	assert.Equal(t, filepath.Join("frontend", "node_modules", "@misskey-dev", "emoji-assets", "built", "twemoji"), TwemojiDir())
}

func TestTwemojiDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_TWEMOJI_DIR", "/custom/twemoji")
	assert.Equal(t, "/custom/twemoji", TwemojiDir())
}

func TestFluentEmojiDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_FLUENT_EMOJI_DIR", "")
	// upstream 2026.5.2 #17381 で fluent-emoji の serve path が
	// fluent-emojis/dist (submodule) → @misskey-dev/emoji-assets/built/fluent-emoji
	// に移行した (twemoji と同 commit、#1167)。
	assert.Equal(t, filepath.Join("frontend", "node_modules", "@misskey-dev", "emoji-assets", "built", "fluent-emoji"), FluentEmojiDir())
}

func TestFluentEmojiDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_FLUENT_EMOJI_DIR", "/custom/fluent")
	assert.Equal(t, "/custom/fluent", FluentEmojiDir())
}

func TestStaticDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_STATIC_DIR", "")
	assert.Equal(t, filepath.Join("frontend", "assets"), StaticDir())
}

func TestStaticDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_STATIC_DIR", "/custom/static")
	assert.Equal(t, "/custom/static", StaticDir())
}

func TestRepoAssetsDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_REPO_ASSETS_DIR", "")
	assert.Equal(t, filepath.Join("frontend", "repo-assets"), RepoAssetsDir())
}

func TestRepoAssetsDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_REPO_ASSETS_DIR", "/custom/assets")
	assert.Equal(t, "/custom/assets", RepoAssetsDir())
}

// --- DetectClientEntry ---

func TestDetectClientEntry_NoManifest(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIR", t.TempDir())
	info := DetectClientEntry()
	assert.Empty(t, info.Script)
	assert.Nil(t, info.CSS)
}

func TestDetectClientEntry_ValidManifest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	manifest := `{"src/_boot_.ts":{"file":"assets/boot.abc.js","isEntry":true,"css":["assets/style.def.css"]}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0644))

	info := DetectClientEntry()
	assert.Equal(t, "assets/boot.abc.js", info.Script)
	assert.Equal(t, []string{"assets/style.def.css"}, info.CSS)
	assert.Nil(t, info.ModulePreloads)
}

// Verify that DetectClientEntry walks the entry's import chain recursively and
// aggregates CSS from all imported chunks (upstream HtmlTemplateService#collectViteAssetFiles parity).
// Without this, lazy-loaded SFC `<style scoped>` chunks (e.g. MkCustomEmoji /
// MkMention) would be missing at first paint and the components fall back to
// the browser's native <img> sizing (= 通常より明らかに大きい症状).
func TestDetectClientEntry_WalksImports(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	manifest := `{
		"src/_boot_.ts": {
			"file": "scripts/boot.js",
			"isEntry": true,
			"css": ["assets/entry.css"],
			"imports": ["_chunk-a.js", "_chunk-b.js"]
		},
		"_chunk-a.js": {
			"file": "scripts/chunk-a.js",
			"css": ["assets/chunk-a.css"],
			"imports": ["_chunk-c.js"]
		},
		"_chunk-b.js": {
			"file": "scripts/chunk-b.js",
			"css": ["assets/chunk-b.css"]
		},
		"_chunk-c.js": {
			"file": "scripts/chunk-c.js",
			"css": ["assets/chunk-c.css"]
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0644))

	info := DetectClientEntry()
	assert.Equal(t, "scripts/boot.js", info.Script)
	// Entry CSS + chunk-a / chunk-b (= entry の直接 import) + chunk-c (= recursive)
	// が全て収集される。順序は entry → entry.imports[0] → entry.imports[0] の sub-import
	// → entry.imports[1] (深さ優先)。
	assert.Equal(t, []string{
		"assets/entry.css",
		"assets/chunk-a.css",
		"assets/chunk-c.css",
		"assets/chunk-b.css",
	}, info.CSS)
	// modulePreloads は entry の直接 import のみ (= 1 hop)。
	assert.Equal(t, []string{
		"scripts/chunk-a.js",
		"scripts/chunk-b.js",
	}, info.ModulePreloads)
}

// Cycles or duplicate imports must not cause infinite recursion or duplicate
// CSS / preload entries. seenChunks は recursive walk 全体で共有されるため、
// chunk-b は chunk-a の sub-import として先に visit され、top-level の visit
// で skip される (= modulePreloads には入らない)。upstream の
// collectViteAssetFiles と同じ挙動。
func TestDetectClientEntry_DedupAndCycle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	manifest := `{
		"src/_boot_.ts": {
			"file": "scripts/boot.js",
			"isEntry": true,
			"css": ["assets/entry.css"],
			"imports": ["_chunk-a.js", "_chunk-b.js"]
		},
		"_chunk-a.js": {
			"file": "scripts/chunk-a.js",
			"css": ["assets/shared.css"],
			"imports": ["_chunk-b.js"]
		},
		"_chunk-b.js": {
			"file": "scripts/chunk-b.js",
			"css": ["assets/shared.css"],
			"imports": ["_chunk-a.js"]
		}
	}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0644))

	info := DetectClientEntry()
	assert.Equal(t, []string{
		"assets/entry.css",
		"assets/shared.css",
	}, info.CSS)
	assert.Equal(t, []string{
		"scripts/chunk-a.js",
	}, info.ModulePreloads)
}

func TestDetectClientEntry_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{invalid"), 0644))

	info := DetectClientEntry()
	assert.Empty(t, info.Script)
}

func TestDetectClientEntry_NoBootEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)
	manifest := `{"src/other.ts":{"file":"other.js","isEntry":true}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0644))

	info := DetectClientEntry()
	assert.Empty(t, info.Script)
}

// --- AssetsHandler ---

func setupAssetsHandler(t *testing.T) (primary, fallback string) {
	t.Helper()
	primary = t.TempDir()
	fallback = t.TempDir()
	return
}

func doAssetsRequest(handler echo.HandlerFunc, path string) (*httptest.ResponseRecorder, error) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/assets/"+path, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("*")
	c.SetParamValues(path)
	err := handler(c)
	return rec, err
}

func TestAssetsHandler_ServeFromPrimary(t *testing.T) {
	primary, fallback := setupAssetsHandler(t)
	require.NoError(t, os.WriteFile(filepath.Join(primary, "style.css"), []byte("body{}"), 0644))

	h := AssetsHandler(primary, fallback)
	rec, err := doAssetsRequest(h, "style.css")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "body{}")
}

func TestAssetsHandler_FallbackToSecondary(t *testing.T) {
	primary, fallback := setupAssetsHandler(t)
	require.NoError(t, os.WriteFile(filepath.Join(fallback, "ai.png"), []byte("PNG"), 0644))

	h := AssetsHandler(primary, fallback)
	rec, err := doAssetsRequest(h, "ai.png")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "PNG")
}

func TestAssetsHandler_PrimaryTakesPrecedence(t *testing.T) {
	primary, fallback := setupAssetsHandler(t)
	require.NoError(t, os.WriteFile(filepath.Join(primary, "dup.txt"), []byte("primary"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(fallback, "dup.txt"), []byte("fallback"), 0644))

	h := AssetsHandler(primary, fallback)
	rec, err := doAssetsRequest(h, "dup.txt")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "primary")
}

func TestAssetsHandler_NotFoundInEither(t *testing.T) {
	primary, fallback := setupAssetsHandler(t)

	h := AssetsHandler(primary, fallback)
	_, err := doAssetsRequest(h, "missing.txt")
	assert.Equal(t, echo.ErrNotFound, err)
}

func TestAssetsHandler_DirectoryIgnored(t *testing.T) {
	primary, fallback := setupAssetsHandler(t)
	require.NoError(t, os.MkdirAll(filepath.Join(primary, "subdir"), 0755))

	h := AssetsHandler(primary, fallback)
	_, err := doAssetsRequest(h, "subdir")
	assert.Equal(t, echo.ErrNotFound, err)
}

func TestAssetsHandler_PathTraversalBlocked(t *testing.T) {
	primary, fallback := setupAssetsHandler(t)
	h := AssetsHandler(primary, fallback)
	_, err := doAssetsRequest(h, "../../../etc/passwd")
	assert.Equal(t, echo.ErrNotFound, err)
}

// **参照のままにすると loader の更新が閲覧側に届かない (#2551)。** 読めたものは
// 埋め込み、読めなければ空を返して呼び出し側が URL 参照に落とす。
func TestReadLoaderAssets(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "style.css"),
		[]byte("#splash{opacity:1}"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "boot.js"),
		[]byte("console.log('boot')"), 0o644))

	got := readLoaderAssets(dir)
	assert.Equal(t, "#splash{opacity:1}", got.CSS)
	assert.Equal(t, "console.log('boot')", got.JS)
}

func TestReadLoaderAssets_MissingFilesYieldEmpty(t *testing.T) {
	got := readLoaderAssets(t.TempDir())
	assert.Empty(t, got.CSS)
	assert.Empty(t, got.JS)
}

// **閉じタグに化ける並びは埋め込まない。** `<style>` / `<script>` の中身は
// エスケープが効かず、`</script` ひとつで後続が HTML として解釈される。
func TestReadInlinableAsset_RejectsTerminators(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"script.js":  `var a = "</script><img src=x onerror=alert(1)>"`,
		"style.css":  `/* </style><script>alert(1)</script> */`,
		"upper.js":   `var a = "</SCRIPT>"`,
		"comment.js": `<!-- x`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
			assert.Empty(t, readInlinableAsset(path), "埋め込まずに参照へ落とす")
		})
	}
}

// 読み込みは 1 回だけ。**再ビルド後は再起動が要る** (client entry と同じ前提)。
func TestBootLoaderAssets_ReadsOnce(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "style.css"),
		[]byte("first"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_DIR", dir)

	first := BootLoaderAssets()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "style.css"),
		[]byte("second"), 0o644))
	assert.Equal(t, first, BootLoaderAssets(), "2 回目もキャッシュを返す")
}

func TestFrontendEmbedDir_Default(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIR", "")
	t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", "")
	assert.Equal(t,
		filepath.Join("frontend", "built", "_frontend_embed_vite_"),
		FrontendEmbedDir())
}

func TestFrontendEmbedDir_EnvOverride(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", "/custom/embed")
	assert.Equal(t, "/custom/embed", FrontendEmbedDir())
}

// embed は通常の SPA とは別 build なので entry も別物 (#2389)。
func TestDetectEmbedEntry(t *testing.T) {
	t.Run("no manifest", func(t *testing.T) {
		t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", t.TempDir())
		info := DetectEmbedEntry()
		assert.Empty(t, info.Script)
	})

	t.Run("valid manifest", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", dir)
		manifest := `{"src/boot.ts":{"file":"assets/embed.abc.js","isEntry":true,"css":["assets/embed.def.css"]}}`
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644))

		info := DetectEmbedEntry()
		assert.Equal(t, "assets/embed.abc.js", info.Script)
		assert.Equal(t, []string{"assets/embed.def.css"}, info.CSS)
	})
}

// embed 側の loader も同じく 1 回だけ読む (#2551)。
func TestEmbedLoaderAssets(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "loader", "boot.js"),
		[]byte("embed-boot"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_EMBED_DIR", dir)
	ResetLoaderCacheForTest()

	assert.Equal(t, "embed-boot", EmbedLoaderAssets().JS)
	assert.Empty(t, EmbedLoaderAssets().CSS, "無いものは空のまま")
}

// **キャッシュを捨てられないと、ディレクトリを差し替える test 同士が干渉する。**
func TestResetLoaderCacheForTest(t *testing.T) {
	first := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(first, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(first, "loader", "style.css"),
		[]byte("first"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_DIR", first)
	ResetLoaderCacheForTest()
	require.Equal(t, "first", BootLoaderAssets().CSS)

	second := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(second, "loader"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(second, "loader", "style.css"),
		[]byte("second"), 0o644))
	t.Setenv("MISSKEY_FRONTEND_DIR", second)
	assert.Equal(t, "first", BootLoaderAssets().CSS, "捨てるまではキャッシュのまま")

	ResetLoaderCacheForTest()
	assert.Equal(t, "second", BootLoaderAssets().CSS)
}
