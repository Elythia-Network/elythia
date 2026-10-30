package server

import (
	"os"
	"path/filepath"

	"github.com/elythia-network/elythia/internal/api/clips"
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/notes"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// frontendRoutesDeps holds the parts wireFrontendRoutes receives from
// setupRoutes (#3901). setupRoutes sets every field from the variable of the
// same name; TestSetupRoutesSectionsReceiveSameNamedVariables checks that no
// field is left out or filled from a different variable.
type frontendRoutesDeps struct {
	userRepo                repository.UserRepository
	noteRepo                repository.NoteRepository
	metaRepo                repository.MetaRepository
	emojiRepo               *repository.CachedEmojiRepository
	clipRepo                repository.ClipRepository
	entityPacker            *entity.Packer
	proxyAccountResolver    meta.ProxyAccountResolver
	localHost               string
	setupDetector           *meta.SetupDetector
	chunkedUploadCapability func() (int64, bool)
	frontend                echo.HandlerFunc
	notesHandler            *notes.Handler
	clipsHandler            *clips.Handler
}

// wireFrontendRoutes registers the routes outside /api that serve the
// frontend: the Vite bundles, locales and other assets, twemoji and
// fluent-emoji, the identicon, avatar and emoji redirects, the PWA manifest,
// the Service Worker, the embed pages and, last, the SPA catchall.
// setupRoutes calls it where these statements used to be, after the plugin
// routes and the /api catchall, so the registration order is unchanged. It
// binds every field of d to the name setupRoutes uses, so the moved
// statements read the same as before (#3901).
func (s *Server) wireFrontendRoutes(d frontendRoutesDeps) {
	userRepo := d.userRepo
	noteRepo := d.noteRepo
	metaRepo := d.metaRepo
	emojiRepo := d.emojiRepo
	clipRepo := d.clipRepo
	entityPacker := d.entityPacker
	proxyAccountResolver := d.proxyAccountResolver
	localHost := d.localHost
	setupDetector := d.setupDetector
	chunkedUploadCapability := d.chunkedUploadCapability
	frontend := d.frontend
	notesHandler := d.notesHandler
	clipsHandler := d.clipsHandler

	// フロントエンドアセット配信
	// dev モードなら Vite dev server へプロキシ、そうでなければビルド済みアセットを
	// 静的配信する (無ければ 404。dev server へは流さない)。
	//
	// dev モードではビルド成果物の有無を**見ない** (#2477)。見てしまうと、
	// 以前のビルドが残っているだけで dev server に繋がらず HMR に入れない。
	registerFrontendAssets(s.echo, s.config, "/vite", frontendutil.FrontendDir(), viteDevServerURL)

	// embed 専用バンドル配信 (#2389)。通常の SPA とは別 build なので別ディレクトリ・
	// 別 prefix になる (upstream ClientServerService の `/embed_vite/` と同じ)。
	registerFrontendAssets(s.echo, s.config, "/embed_vite", frontendutil.FrontendEmbedDir(), viteEmbedDevServerURL)

	// フロントエンド配布アセット (locales, fonts等) + リポジトリアセット (ai.png等)
	// Echo は同一パスに Static を 2 回登録すると上書きされるため、
	// frontendDistDir → repoAssetsDir の順にフォールバックするハンドラを使う
	frontendDistDir := frontendutil.FrontendDistDir()
	repoAssetsDir := frontendutil.RepoAssetsDir()
	s.echo.GET("/assets/*", frontendutil.AssetsHandler(frontendDistDir, repoAssetsDir))

	// twemoji SVG配信 + 30 day Cache-Control (upstream `ms('30 days')` 同等)。
	twemojiDir := frontendutil.TwemojiDir()
	if _, err := os.Stat(twemojiDir); err == nil {
		serveStaticAssetDir(s.echo, "/twemoji", twemojiDir)
	}

	// fluent-emoji PNG配信 (実績バッジ等で参照される)。upstream Misskey TS の
	// /fluent-emoji/:hex.png 経路 (ClientServerService.ts) と互換、+ 30 day
	// Cache-Control (上記 twemoji と同 helper)。
	fluentEmojiDir := frontendutil.FluentEmojiDir()
	if _, err := os.Stat(fluentEmojiDir); err == nil {
		serveStaticAssetDir(s.echo, "/fluent-emoji", fluentEmojiDir)
	}

	// client-assets配信 (バブルゲーム、フラッシュ等のフロントエンドアセット)
	clientAssetsDir := frontendutil.ClientAssetsDir()
	if _, err := os.Stat(clientAssetsDir); err == nil {
		s.echo.Static("/client-assets", clientAssetsDir)
	}

	// 静的アセット配信 (favicon, splash, icons等)
	staticDir := frontendutil.StaticDir()
	if _, err := os.Stat(staticDir); err == nil {
		s.echo.Static("/static-assets", staticDir)
		// 埋め込み先サイトが読み込むローダー。upstream も staticAssets から
		// 配っている (#2389)。
		s.echo.File("/embed.js", filepath.Join(staticDir, "embed.js"))
		s.echo.File("/favicon.ico", filepath.Join(staticDir, "favicon.ico"))
		s.echo.File("/apple-touch-icon.png", filepath.Join(staticDir, "apple-touch-icon.png"))
		// /robots.txt はここでは配らない。upstream は meta を見て動的生成する
		// (ugcVisibilityForVisitor で Disallow が変わる) ので、wireFederationRoutes の
		// webResHandler.RobotsTxt に一本化する。静的ファイルを後から登録すると
		// そちらが後勝ちして、実体が無い環境で 404 になっていた (#2345)。
	}

	// identicon — ユーザーアイコン自動生成 (meta.enableIdenticonGeneration が
	// false なら 1x1 透明 PNG にフォールバック)
	s.echo.GET("/identicon/:x", identiconHandler(metaRepo))

	// /avatar/@:acct — frontend の MkMention.vue が <img src="/avatar/@user@host">
	// で直接読みに来るので、対応する handler が無いと mention chip が
	// アイコン無しで描画される (#462)。upstream Misskey TS の同名 endpoint
	// と互換: 見つかった user の avatarUrl にリダイレクトし、未設定なら
	// identicon、未存在なら /static-assets/user-unknown.png にフォールバック。
	s.echo.GET("/avatar/@:acct", avatarHandler(entityPacker, userRepo, localHost))

	// /emoji/:path — frontend の MkCustomEmoji が `:emojiUrl` プロップを
	// 受け取らないとき (典型: 通知 reaction icon) に <img src="/emoji/<name>@<host>.webp">
	// で直接 fetch するので、handler が無いと SPA catchall に流れて画像が
	// 読めず fallback 画像になる (#468)。upstream Misskey TS の
	// ServerService.ts:153 と互換に: name + host を抜いて emoji table を
	// lookup → publicUrl に 302 redirect、見つからなければ ?fallback クエリ
	// 付きなら /static-assets/emoji-unknown.png、無ければ 404。
	s.echo.GET("/emoji/:path", emojiRedirectHandler(entityPacker, emojiRepo))

	// manifest.json — PWA用
	s.echo.GET("/manifest.json", manifestJSON(s.config, metaRepo))

	// Service Worker (sw.js) — Misskey frontend は GET /sw.js を登録しにくる。
	// SPA catchall より前に登録しないと text/html にフォールバックしてしまい
	// ブラウザが "unsupported MIME type" エラーで SW 登録を拒否する。
	swDistDir := frontendutil.SwDistDir()
	if _, err := os.Stat(filepath.Join(swDistDir, "sw.js")); err == nil {
		s.echo.File("/sw.js", filepath.Join(swDistDir, "sw.js"))
	}

	// 埋め込みページ (#2389)。SPA catchall より**前**に登録する。後だと
	// /embed/* が通常の SPA シェルに落ちて、iframe の中にフルアプリが出る
	// (エラーにならないので気付きにくい壊れ方になる)。
	//
	// pack は viewer なしで行う。埋め込みは認証を伴わないので、閲覧者依存の
	// フィールドを解決してはいけない。可視性の判断は各 handler の
	// PackForEmbed 経由で既存の CanSee / HideEmbeds に委ねる。
	embed := newEmbedHandlers(s.config, metaRepo, proxyAccountResolver, chunkedUploadCapability, setupDetector, EmbedDeps{
		NoteRepo: noteRepo,
		UserRepo: userRepo,
		ClipRepo: clipRepo,
		PackNote: func(c echo.Context, n *model.Note) (any, error) {
			return notesHandler.PackForEmbed(c.Request().Context(), n), nil
		},
		PackUser: func(_ echo.Context, u *model.User) (any, error) {
			// upstream の userEntityService.pack(user) 相当 (= UserLite)。
			return entityPacker.PackUserLite(u), nil
		},
		PackClip: func(_ echo.Context, cl *model.Clip) (any, error) {
			return clipsHandler.PackForEmbed(cl), nil
		},
	})
	s.echo.GET("/embed/notes/:note", embed.Note)
	s.echo.GET("/embed/user-timeline/:user", embed.User)
	s.echo.GET("/embed/clips/:clip", embed.Clip)
	s.echo.GET("/embed/*", embed.Fallback)

	// Frontend HTML shell — SPA catchall (最後に登録)
	// CSP violation の受け口 (#2425)。**CSP が off のときは生やさない。**
	// 認証不要の POST なので、使わない構成で晒す理由が無い。
	if mode := s.config.FrontendContentSecurityPolicy; mode == CSPModeReportOnly || mode == CSPModeEnforce {
		s.echo.POST(CSPReportPath, cspReportHandler())
	}

	s.echo.GET("/*", frontend)
}
