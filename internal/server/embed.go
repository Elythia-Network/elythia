package server

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"strings"

	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/frontendutil"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/labstack/echo/v4"
)

// embedCacheControl mirrors upstream's `public, max-age=3600` on embed pages.
const embedCacheControl = "public, max-age=3600"

// embedContext is the payload serialized into #misskey_embedCtx. The embed
// bundle reads it instead of calling the API, so the field names must match
// upstream (`note` / `user` / `clip`).
type embedContext struct {
	Note any `json:"note,omitempty"`
	User any `json:"user,omitempty"`
	Clip any `json:"clip,omitempty"`
}

// EmbedDeps carries the repositories and packers the embed pages need.
//
// embed は **認証なしで誰でも読める** 経路なので、pack は viewer なし
// (viewerID = "") で行う。viewer を渡してしまうと、埋め込み経由で viewer 依存の
// フィールド (isFollowing 等) が漏れる。
type EmbedDeps struct {
	NoteRepo repository.NoteRepository
	UserRepo repository.UserRepository
	ClipRepo repository.ClipRepository

	PackNote func(c echo.Context, note *model.Note) (any, error)
	PackUser func(c echo.Context, user *model.User) (any, error)
	PackClip func(c echo.Context, clip *model.Clip) (any, error)
}

// embedHandlers renders the embed shell for the four upstream embed routes.
type embedHandlers struct {
	cfg                  *config.Config
	metaRepo             repository.MetaRepository
	proxyAccountResolver meta.ProxyAccountResolver
	chunkedUpload        meta.ChunkedUploadCapability
	entry                frontendutil.ClientEntryInfo
	deps                 EmbedDeps
}

// newEmbedHandlers builds the embed route handlers.
//
// entry は起動時に 1 度だけ manifest から解決する (通常シェルと同じ方針)。
func newEmbedHandlers(
	cfg *config.Config,
	metaRepo repository.MetaRepository,
	proxyAccountResolver meta.ProxyAccountResolver,
	chunkedUpload meta.ChunkedUploadCapability,
	deps EmbedDeps,
) *embedHandlers {
	return &embedHandlers{
		cfg:                  cfg,
		metaRepo:             metaRepo,
		proxyAccountResolver: proxyAccountResolver,
		chunkedUpload:        chunkedUpload,
		entry:                embedEntryFor(cfg),
		deps:                 deps,
	}
}

// Note renders /embed/notes/:note.
//
// 可視性ゲートは upstream ClientServerService と同一にする。埋め込みは認証を
// 伴わないので、ここを緩めるとそのまま IDOR になる。
//
//   - ノートが存在しない        -> 文脈なしのシェル
//   - visibility が specified   -> 文脈なしのシェル (宛先限定)
//   - visibility が followers   -> 文脈なしのシェル (フォロワー限定)
//   - userHost != nil (リモート) -> 文脈なしのシェル
//
// upstream は該当時に何も返さず fastify の既定 (空応答) に落ちるが、mk-go は
// 常にシェルを返す。「ノートが無い」と「非公開」を応答の形で区別させないため。
func (h *embedHandlers) Note(c echo.Context) error {
	// upstream は `relations: { user: true, reply: true, renote: true }` で引く。
	// FindByID だと author が空のまま pack され、埋め込みに投稿者名が出ない。
	note, err := h.deps.NoteRepo.FindByIDWithRelations(c.Param("note"))
	if err != nil || note == nil || !embedNoteIsPublic(note) {
		return h.render(c, nil)
	}
	packed, err := h.deps.PackNote(c, note)
	if err != nil {
		return h.render(c, nil)
	}
	return h.render(c, &embedContext{Note: packed})
}

// embedNoteIsPublic reports whether a note may be embedded anonymously.
func embedNoteIsPublic(note *model.Note) bool {
	switch note.Visibility {
	case "specified", "followers":
		return false
	}
	// リモートノートは埋め込まない。upstream と同じ制限で、自分が権威で
	// ないコンテンツを自分のドメインで配らないという線引き。
	return note.UserHost == nil || *note.UserHost == ""
}

// User renders /embed/user-timeline/:user.
//
// リモートユーザーは埋め込まない (upstream と同じ)。
func (h *embedHandlers) User(c echo.Context) error {
	user, err := h.deps.UserRepo.FindByID(c.Param("user"))
	if err != nil || user == nil || (user.Host != nil && *user.Host != "") {
		return h.render(c, nil)
	}
	packed, err := h.deps.PackUser(c, user)
	if err != nil {
		return h.render(c, nil)
	}
	return h.render(c, &embedContext{User: packed})
}

// Clip renders /embed/clips/:clip.
func (h *embedHandlers) Clip(c echo.Context) error {
	clip, err := h.deps.ClipRepo.FindByID(c.Param("clip"))
	if err != nil || clip == nil {
		return h.render(c, nil)
	}
	// 非公開クリップは埋め込まない。upstream は clip の存在だけを見ているが、
	// mk-go では公開フラグも見る。埋め込みは無認証なので、本人だけが見える
	// はずのクリップを配らない方が正しい (divergence として意図的)。
	if !clip.IsPublic {
		return h.render(c, nil)
	}
	packed, err := h.deps.PackClip(c, clip)
	if err != nil {
		return h.render(c, nil)
	}
	return h.render(c, &embedContext{Clip: packed})
}

// Fallback renders /embed/* for paths without a dedicated handler.
func (h *embedHandlers) Fallback(c echo.Context) error {
	return h.render(c, nil)
}

// render writes the embed HTML shell with the optional context payload.
func (h *embedHandlers) render(c echo.Context, ctx *embedContext) error {
	instanceName := config.DisplayName
	// upstream base-embed.tsx:46-47 と同じ fallback (SPA shell と共通)。
	iconURL := "/favicon.ico"
	appleTouchIconURL := "/apple-touch-icon.png"
	themeColor := defaultThemeColor
	metaJSON := "{}"

	// CSP の media origin は object storage (meta 依存) から来るので、
	// meta を取れたかどうかを外に持ち出す (#2789)。
	var cspMeta *model.Meta

	if m, err := h.metaRepo.Fetch(); err == nil && m != nil {
		cspMeta = m
		if m.Name != nil && *m.Name != "" {
			instanceName = *m.Name
		}
		if m.IconURL != nil && *m.IconURL != "" {
			iconURL = *m.IconURL
		}
		if m.App512IconURL != nil && *m.App512IconURL != "" {
			appleTouchIconURL = *m.App512IconURL
		}
		if m.ThemeColor != nil && *m.ThemeColor != "" {
			themeColor = *m.ThemeColor
		}
		metaJSON = buildMetaJSON(h.cfg, m, h.proxyAccountResolver, h.chunkedUpload)
	}

	embedCtxJSON := ""
	if ctx != nil {
		if b, err := json.Marshal(ctx); err == nil {
			embedCtxJSON = string(b)
		}
	}

	// **変数名に html を使わない。** 標準の `html` パッケージを shadow して、
	// 以降で html.EscapeString が呼べなくなる。
	shell, inlineScripts := h.buildHTML(instanceName, iconURL, appleTouchIconURL, themeColor, metaJSON, embedCtxJSON)

	// embed にも SPA shell と同じ CSP を付ける (#2789)。**embed は
	// `X-Frame-Options` の除外対象** = 第三者のページに iframe で埋め込まれる
	// 唯一の経路なので、ここで script が注入されると埋め込み先ではなく
	// **こちらの origin** で動く。
	//
	// `frame-ancestors` は入れない。`middleware/frameguard.go` が
	// `X-Frame-Options` 側で `/embed/` の除外を持っており、CSP に重ねると
	// 除外を二重管理することになる (`frontendCSPDirectives` のコメントも参照)。
	//
	// captcha の origin は足さない。embed はサインアップ経路を持たないので、
	// 使っていない host を CSP に載せる理由が無い。
	cspExtra := cspExtras{
		Media:  cspMediaExtras(h.cfg, cspMeta),
		Script: cspScriptHashes(inlineScripts...),
	}
	applyFrontendCSP(c, h.cfg.FrontendContentSecurityPolicy, cspExtra)

	c.Response().Header().Set(echo.HeaderCacheControl, embedCacheControl)
	return c.HTML(http.StatusOK, shell)
}

// buildHTML assembles the embed shell, mirroring upstream views/base-embed.tsx.
//
// The second return value lists the inline scripts embedded in the shell, so the
// caller can derive their CSP hashes.
//
// **hash の材料は HTML に入れる文字列そのものを返す。** 別々に組み立てると、
// 片方だけ変えたときに CSP を有効にしている運用者の画面が真っ白になる
// (#2786 で SPA shell 側が踏みかけた形)。
func (h *embedHandlers) buildHTML(instanceName, iconURL, appleTouchIconURL, themeColor, metaJSON, embedCtxJSON string) (string, []string) {
	var head strings.Builder
	// ビルド済み bundle が無い場合 (dev) は vite client を読ませる。upstream の
	// `frontendEmbedViteFiles == null` 分岐と同じ。
	if h.entry.Script == "" {
		head.WriteString(`<script type="module" src="/embed_vite/@vite/client"></script>` + "\n")
	}
	for _, css := range h.entry.CSS {
		head.WriteString(fmt.Sprintf(`<link rel="stylesheet" href="/embed_vite/%s">`, html.EscapeString(css)) + "\n")
	}

	// CLIENT_ENTRY は **manifest の値をそのまま** 渡す。embed の loader は
	//
	//     `/embed_vite/${CLIENT_ENTRY.replace('scripts', lang)}`
	//
	// と組み立てるので (built/_frontend_embed_vite_/loader/boot.js)、ここで
	// `/embed_vite/` を前置すると `/embed_vite//embed_vite/<lang>/...` になって
	// 404 する。`scripts` を言語コードに差し替える前提なので、path の形も
	// manifest のまま保つ必要がある。
	clientEntry := "null"
	if h.entry.Script != "" {
		if b, err := json.Marshal(h.entry.Script); err == nil {
			clientEntry = string(b)
		}
	}

	// LANGS は embed の boot loader が参照する。定義しないと
	// `LANGS is not defined` で初期化が落ち、iframe に
	// "Failed to initialize Elythia" だけが出る (HTML 自体は 200 なので
	// status を見る検査では捕まらない)。通常シェルと同じ値にしてある。
	//
	// JSON は <script type="application/json"> にそのまま入れる。閉じタグの
	// 早期終了だけを潰せばよく、HTML エスケープすると JSON.parse が壊れる。
	metaBlock := ""
	if metaJSON != "" {
		metaBlock = fmt.Sprintf(
			`<script type="application/json" id="misskey_meta">%s</script>`,
			escapeJSONForScript(metaJSON),
		)
	}
	ctxBlock := ""
	if embedCtxJSON != "" {
		ctxBlock = fmt.Sprintf(
			`<script type="application/json" id="misskey_embedCtx">%s</script>`,
			escapeJSONForScript(embedCtxJSON),
		)
	}

	// SPA シェルと同じく loader は埋め込む (#2551)。embed も同じファイル名 (ハッシュ
	// 無し) を参照していたので、同じ形で古い版が居座る。
	loader := embedLoaderAssetsFor(h.cfg)
	loaderCSSTag := inlineOrLinkCSS(loader.CSS, "/embed_vite/loader/style.css")
	loaderJSTag := inlineOrLinkJS(loader.JS, "/embed_vite/loader/boot.js")

	bootGlobals := fmt.Sprintf(
		"\nconst VERSION = %s;\nconst CLIENT_ENTRY = %s;\nconst LANGS = [\"ja-JP\",\"en-US\"];\n",
		jsStringLiteral(h.cfg.Version), clientEntry)

	// loader は inline のときだけ hash が要る (外部参照なら 'self' で通る)。
	inlineScripts := []string{bootGlobals, loader.JS}

	return fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<meta name="application-name" content="Misskey">
<meta name="referer" content="origin">
<meta name="theme-color" content="%s">
<meta name="theme-color-orig" content="%s">
<meta property="og:site_name" content="%s">
<meta property="instance_url" content="%s">
<meta name="viewport" content="width=device-width, initial-scale=1, minimum-scale=1, maximum-scale=1, user-scalable=no, viewport-fit=cover">
<meta name="format-detection" content="telephone=no,date=no,address=no,email=no,url=no">
<meta name="robots" content="noindex">
<link rel="icon" href="%s">
<link rel="apple-touch-icon" href="%s">
<title>%s</title>
%s%s<script>%s</script>
%s
%s
%s
</head>
<body>
<noscript><p>JavaScriptを有効にしてください<br>Please turn on your JavaScript</p></noscript>
</body></html>`,
		html.EscapeString(themeColor),
		html.EscapeString(themeColor),
		html.EscapeString(instanceName),
		html.EscapeString(h.cfg.URL),
		html.EscapeString(iconURL),
		html.EscapeString(appleTouchIconURL),
		html.EscapeString(instanceName),
		head.String(),
		loaderCSSTag,
		bootGlobals,
		metaBlock,
		ctxBlock,
		loaderJSTag,
	), inlineScripts
}

// escapeJSONForScript neutralises sequences that would terminate the enclosing
// <script> element early.
//
// JSON 内に `</script>` を含む文字列があると、そこで script 要素が閉じて以降が
// HTML として解釈される (= XSS)。ノート本文もユーザー名も攻撃者が自由に
// 書けるので、これは実在の経路。
//
// \u003c 等は JSON としてデコードすれば元の文字に戻るので、JSON.parse の
// 結果は変わらない。
//
// 現状の呼び出し元はどちらも encoding/json 由来で、Go の json.Marshal は
// 既定で `<` `>` `&` を escape する。つまり二重の防御だが、JSON の生成方法が
// 変わったときに黙って穴が開かないよう、埋め込む側でも明示的に潰しておく。
func escapeJSONForScript(s string) string {
	return strings.NewReplacer(
		"<", `\u003c`,
		">", `\u003e`,
		"&", `\u0026`,
	).Replace(s)
}

// jsStringLiteral renders s as a JavaScript string literal that is safe to
// place inside an inline <script> element.
//
// html.EscapeString は <script> の中では効かない (実体参照がデコードされない)
// ので、`"` や `\` を含む値でリテラルから抜けられる。JSON の文字列は JS の
// 文字列リテラルとしてそのまま読めるので、json.Marshal で quote したうえで
// `</script>` 対策を重ねる。
func jsStringLiteral(s string) string {
	b, _ := json.Marshal(s)
	return escapeJSONForScript(string(b))
}
