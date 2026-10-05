package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/frontendutil"
)

// writeViteManifest lays down a minimal built-asset tree and points the
// frontend dir env vars at it, so DetectClientEntry / DetectEmbedEntry resolve
// a non-empty entry. 「ビルド成果物がある」状態を作るためのヘルパ。
func writeViteManifest(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	spa := filepath.Join(root, "_frontend_vite_")
	embed := filepath.Join(root, "_frontend_embed_vite_")
	require.NoError(t, os.MkdirAll(spa, 0o755))
	require.NoError(t, os.MkdirAll(embed, 0o755))

	write := func(dir, entryKey string) {
		m := map[string]map[string]any{
			entryKey: {"file": "assets/entry.js", "isEntry": true},
		}
		b, err := json.Marshal(m)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644))
	}
	write(spa, "src/_boot_.ts")
	write(embed, "src/boot.ts")

	// FrontendEmbedDir は FrontendDir の sibling として解決されるので、
	// SPA 側だけ指定すれば両方に効く。
	t.Setenv("MISSKEY_FRONTEND_DIR", spa)
	// **loader キャッシュはプロセスに 1 つ。** fixture は `t.TempDir()` なので、
	// 捨てないと消えたディレクトリの内容が居座る (#2795)。
	frontendutil.ResetLoaderCacheForTest()
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)
}

func TestIsDev(t *testing.T) {
	assert.False(t, isDev(nil), "cfg が無い経路は本番扱いにする")
	assert.False(t, isDev(&config.Config{}))
	assert.True(t, isDev(&config.Config{Dev: true}))
}

// **dev ではビルド成果物があっても使わない (#2477)。** 以前のビルドが残って
// いるだけで dev server に繋がらず HMR に入れない、という状態を防ぐ。
func TestClientEntryFor_DevIgnoresBuiltAssets(t *testing.T) {
	writeViteManifest(t)

	// 前提: dev でなければ manifest が解決される。
	prod := clientEntryFor(&config.Config{})
	require.NotEmpty(t, prod.Script, "ビルド成果物があれば production 扱い")

	dev := clientEntryFor(&config.Config{Dev: true})
	assert.Empty(t, dev.Script, "dev では manifest を無視する")
}

func TestEmbedEntryFor_DevIgnoresBuiltAssets(t *testing.T) {
	writeViteManifest(t)

	prod := embedEntryFor(&config.Config{})
	require.NotEmpty(t, prod.Script)

	dev := embedEntryFor(&config.Config{Dev: true})
	assert.Empty(t, dev.Script)
}

// ビルド成果物が無い場合は dev フラグに関わらず空 (= 既存の暗黙の挙動)。
// dev フラグの導入で従来の動作が変わっていないことの確認。
func TestClientEntryFor_NoBuiltAssets(t *testing.T) {
	t.Setenv("MISSKEY_FRONTEND_DIR", filepath.Join(t.TempDir(), "absent"))
	frontendutil.ResetLoaderCacheForTest()
	t.Cleanup(frontendutil.ResetLoaderCacheForTest)

	assert.Empty(t, clientEntryFor(&config.Config{}).Script)
	assert.Empty(t, clientEntryFor(&config.Config{Dev: true}).Script)
	assert.Empty(t, embedEntryFor(&config.Config{}).Script)
	assert.Empty(t, embedEntryFor(&config.Config{Dev: true}).Script)
}

func TestLogDevModeBanner(t *testing.T) {
	// 非 dev では何も出さない。panic しないことの確認も兼ねる。
	logDevModeBanner(nil)
	logDevModeBanner(&config.Config{})
	logDevModeBanner(&config.Config{Dev: true})
}

// 設定ダンプに dev の実効値と警告が出ること。**本番で有効になっていると
// frontend が丸ごと落ちる**ので、診断から辿れる必要がある。
func TestBuildConfigDump_DevSurfacesEffectiveValueAndWarning(t *testing.T) {
	cfg := secretBearingConfig()
	cfg.Dev = true

	d := BuildConfigDump(cfg, config.RoleBoth)

	var found bool
	for _, e := range d.Effective {
		if e.Key == "frontend 配信元" {
			found = true
			assert.Contains(t, e.Value, viteDevServerURL)
		}
	}
	assert.True(t, found, "実効値に frontend の配信元が出る")

	var warned bool
	for _, w := range d.Warnings {
		if strings.Contains(w, "dev") {
			warned = true
		}
	}
	assert.True(t, warned, "dev が有効なら警告が出る")
}

func TestBuildConfigDump_NonDevReportsBuiltAssets(t *testing.T) {
	d := BuildConfigDump(secretBearingConfig(), config.RoleBoth)

	var found bool
	for _, e := range d.Effective {
		if e.Key == "frontend 配信元" {
			found = true
			assert.Equal(t, "ビルド成果物", e.Value)
		}
	}
	assert.True(t, found)

	for _, w := range d.Warnings {
		assert.NotContains(t, w, "dev が有効", "dev 無効時に dev の警告は出さない")
	}
}
