package server

import (
	"log/slog"
	"os"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/config"
	"github.com/shiroha-a/mk/internal/frontendutil"
)

// viteDevServerURL / viteEmbedDevServerURL are the Vite dev server origins the
// frontend requests are proxied to when running in dev mode. They match the
// `port` values in the fork's vite.config.ts.
const (
	viteDevServerURL      = "http://localhost:5173"
	viteEmbedDevServerURL = "http://localhost:5174"
)

// clientEntryFor resolves the SPA shell's Vite entry, forcing dev-server mode
// when cfg.Dev is set.
//
// **dev 判定を 1 箇所に集約するための関数 (#2477)。** 元々の判定は「ビルド成果物
// が存在するか」という暗黙のもので、静的配信の分岐 (router.go)、SPA シェルの
// CLIENT_ENTRY (frontend.go)、embed シェル (embed.go) の 3 箇所に散っていた。
// ビルド済みディレクトリが残っていると dev server を立てても HMR に入らず、
// しかも本番ではそのディレクトリを bind-mount しているため状態が紛らわしい。
//
// 空の ClientEntryInfo を返すと、描画側は `@vite/client` を読む dev 用の分岐に
// 入る (frontend.go / embed.go の `Script == ""` 判定)。
func clientEntryFor(cfg *config.Config) frontendutil.ClientEntryInfo {
	if isDev(cfg) {
		return frontendutil.ClientEntryInfo{}
	}
	return frontendutil.DetectClientEntry()
}

// embedEntryFor is the embed-bundle counterpart of clientEntryFor.
func embedEntryFor(cfg *config.Config) frontendutil.ClientEntryInfo {
	if isDev(cfg) {
		return frontendutil.ClientEntryInfo{}
	}
	return frontendutil.DetectEmbedEntry()
}

// isDev reports whether the instance runs in frontend dev mode. nil cfg is
// treated as production; 呼び出し側が cfg を持たない経路を作ってしまった場合に
// 黙って dev に落ちる方が危険なため。
func isDev(cfg *config.Config) bool {
	return cfg != nil && cfg.Dev
}

// logDevModeBanner emits the startup diagnostic for dev mode.
//
// **本番で誤って有効化すると frontend が即死する。** `/vite/*` が起動していない
// dev server へ流れる一方、ビルド済みファイルは存在するため「ファイルはあるのに
// 配信されない」という原因の分かりにくい壊れ方をする。起動ログで必ず気付ける
// ようにしておく。
func logDevModeBanner(cfg *config.Config) {
	if !isDev(cfg) {
		return
	}
	slog.Warn("dev モードが有効です。frontend はビルド成果物ではなく Vite dev server から配信されます",
		"viteDevServer", viteDevServerURL,
		"embedDevServer", viteEmbedDevServerURL,
		"注意", "本番では無効にしてください (dev server が無いと frontend が配信されません)")
}

// registerFrontendAssets serves prefix (`/vite` or `/embed_vite`) from the
// built assets in dir, or proxies it to the Vite dev server at devServer when
// cfg enables dev mode. Outside dev mode a missing build answers 404.
//
// **dev server への proxy は dev モードのときだけにする。** 以前は「ビルド成果物が
// 無ければ proxy」だったので、本番でビルド出力が欠けている (bind-mount の
// 付け忘れ、ビルド途中で消えている窓 #2885 など) と、`/vite/*` が**認証なしで**
// `localhost:5173` へ reverse proxy されていた。そのポートで何が待ち受けて
// いるかは mk-go の管理外で、同じホストの別サービスへ外から任意のパスを
// 送り込める。dev モードは `dev: true` / `MK_DEV=1` の明示的な指定でしか
// 入らない (`make dev` はビルド成果物が無いときにそれを立てる)。
//
// **404 を明示的に登録する。** 何も登録しないと SPA の catchall が HTML を 200 で
// 返し、「JS のはずが HTML」という原因から遠い壊れ方になる。
func registerFrontendAssets(e *echo.Echo, cfg *config.Config, prefix, dir, devServer string) {
	if isDev(cfg) {
		e.Any(prefix+"/*", newViteProxy(devServer))
		return
	}
	_, err := os.Stat(dir)
	if err == nil {
		e.Static(prefix, dir)
		return
	}
	slog.Warn("frontend のビルド成果物が見つからないので 404 を返します。開発中なら dev: true (MK_DEV=1) で Vite dev server を使ってください",
		"prefix", prefix, "dir", dir, "error", err)
	e.Any(prefix+"/*", func(echo.Context) error {
		return echo.ErrNotFound
	})
}

// loaderAssetsFor returns the bootloader assets to inline into the SPA shell.
//
// **dev では埋め込まない (#2477 と同じ理由)。** ビルド成果物が残っているだけの
// 状態で埋め込むと、dev server が返す新しい loader ではなく古い built を読んで
// しまう。空を返せば描画側は従来どおり `/vite/loader/*` を参照し、それが dev
// server にプロキシされる。
func loaderAssetsFor(cfg *config.Config) frontendutil.LoaderAssets {
	if isDev(cfg) {
		return frontendutil.LoaderAssets{}
	}
	return frontendutil.BootLoaderAssets()
}

// embedLoaderAssetsFor is loaderAssetsFor for the embed bundle.
func embedLoaderAssetsFor(cfg *config.Config) frontendutil.LoaderAssets {
	if isDev(cfg) {
		return frontendutil.LoaderAssets{}
	}
	return frontendutil.EmbedLoaderAssets()
}
