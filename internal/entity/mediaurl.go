package entity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// MediaURLContext rewrites remote-origin media URLs so they are served through
// this instance's media proxy instead of being handed to the client verbatim.
// It is the server-side counterpart of upstream Misskey's
// DriveFileEntityService.getPublicUrl / getThumbnailUrl: those run at pack time
// and never let a raw remote-origin URL reach an <img src>, which is what keeps
// a viewer's IP from leaking to the originating server (#1529).
//
// mk-go では remote attachment / remote avatar を local に cache せず link
// 形式 (model.DriveFile.IsLink, StoredInternal=false) でそのまま remote origin
// URL を保持する (federation/resolver.go upsertAttachments)。そのため upstream
// のように「remote file は /files/<accessKey> で local 配信される」前提が成り
// 立たず、pack 時に remote origin を proxy 経由 URL へ書き換えないと閲覧者の
// IP が remote server に漏れる。これが #1529 の根因。
//
// 設計上の判断: upstream getPublicUrl は externalMediaProxyEnabled のときだけ
// remote uri を proxy する分岐を持つが、internal proxy しか無い既定構成でも
// remote origin は必ず internal /proxy 経由で wrap する。config.MediaProxy は
// 既定で "<scheme>://<host>/proxy" を指す (= 常に proxy は存在する) ので、
// "external proxy 未設定" は "raw 配信" を意味しない。
type MediaURLContext struct {
	// instanceHost is the host[:port] of this instance, used to distinguish
	// local URLs (which must never be proxied) from remote origins.
	instanceHost string
	// mediaProxyBase is config.MediaProxy (e.g. "https://host/proxy" for the
	// internal proxy, or the operator's external proxy base).
	mediaProxyBase string
	// secret signs internal-proxy URLs (HMAC-SHA256) so the proxy's Authorize
	// accepts them without relying on the DB allowlist.
	secret []byte
	// externalEnabled mirrors config.ExternalMediaProxyEnabled. When true the
	// operator runs an external proxy that owns its own auth, so we do NOT
	// append our HMAC sig (it would be meaningless to a third party).
	externalEnabled bool
	// proxyRemoteFiles mirrors meta.proxyRemoteFiles (default true). When an
	// operator explicitly disables it AND no external proxy is configured, we
	// fall back to upstream's behavior of emitting raw remote URLs.
	proxyRemoteFiles bool
	// proxyRemoteFilesFn は live lookup。設定されていればこちらを優先する。
	// **起動時に焼き込むと、運営者が管理画面で切り替えてもプロセスを再起動
	// するまで反映されない。** これは閲覧者の IP がリモートへ漏れるかどうかを
	// 決める設定なので、締めたつもりで漏れ続ける形になる。
	proxyRemoteFilesFn func() bool
	// ownMediaBaseURL resolves the public base URL that our own object storage
	// serves from (meta.objectStorageBaseUrl 相当)。nil / "" は
	// オブジェクトストレージ未使用。
	ownMediaBaseURL func() string
}

// SetOwnMediaBaseURLResolver wires the resolver for the object storage public
// base URL, so files we host ourselves are never sent through the media proxy
// even when they live on a different domain (#2315).
//
// 想定は wire-time only (server.setupRoutes 経由)。HTTP リスナー起動後の
// 再代入は pack 経路の concurrent read と race する。
func (c *MediaURLContext) SetOwnMediaBaseURLResolver(f func() string) {
	if c != nil {
		c.ownMediaBaseURL = f
	}
}

// NewMediaURLContext builds a context from the resolved config + meta values.
// instanceURL is the absolute instance URL (config.URL); mediaProxy is
// config.MediaProxy. Returns a context with a parsed instance host.
func NewMediaURLContext(instanceURL, mediaProxy string, secret []byte, externalEnabled, proxyRemoteFiles bool) *MediaURLContext {
	host := ""
	if u, err := url.Parse(instanceURL); err == nil {
		host = u.Host
	}
	return &MediaURLContext{
		instanceHost:     host,
		mediaProxyBase:   strings.TrimRight(mediaProxy, "/"),
		secret:           secret,
		externalEnabled:  externalEnabled,
		proxyRemoteFiles: proxyRemoteFiles,
	}
}

// SetProxyRemoteFilesLookup wires a live lookup of meta.proxyRemoteFiles.
func (c *MediaURLContext) SetProxyRemoteFilesLookup(fn func() bool) {
	if c != nil {
		c.proxyRemoteFilesFn = fn
	}
}

// proxyRemoteFilesNow resolves the current setting.
func (c *MediaURLContext) proxyRemoteFilesNow() bool {
	if c.proxyRemoteFilesFn != nil {
		return c.proxyRemoteFilesFn()
	}
	return c.proxyRemoteFiles
}

// proxyMode selects the proxy processing mode, mirroring the bare query flags
// the proxy handler recognizes (internal/api/proxy/handler.go parseMode) and
// the canonical output filenames (isProxyFilename).
type proxyMode int

const (
	modeDefault proxyMode = iota // image.webp, no resize (pass-through)
	modeAvatar                   // avatar.webp, height 320
	modeStatic                   // static.webp, fit 498x422 (thumbnails)
	modePreview                  // preview.webp
	modeBadge                    // emoji.png, 96x96 grayscale
	modeEmoji                    // emoji.webp
)

// fileAndFlag returns the canonical output filename and the bare query-flag key
// for the mode. modeDefault emits no flag. The query flags MUST stay in sync
// with internal/api/proxy/handler.go parseMode — that is what actually drives
// proxy behavior. The filename is cosmetic (we always send ?url=, so the proxy
// never falls back to path-based isProxyFilename parsing) but we emit the
// canonical name for drop-in shape parity + CDN cache-key sanity.
func (m proxyMode) fileAndFlag() (string, string) {
	switch m {
	case modeAvatar:
		return "avatar.webp", "avatar"
	case modeStatic:
		return "static.webp", "static"
	case modePreview:
		return "preview.webp", "preview"
	case modeBadge:
		// **upstream はこの mode だけ `emoji.png` を出す** (`ServerService.ts` の
		// `/emoji/:path` の badge 枝)。`badge.webp` は proxy の isProxyFilename に
		// 入っておらず、実際に返るのも PNG (mediaproxy.processBadge) なので、
		// upstream 名の方が中身とも proxy 側の一覧とも一致する。
		return "emoji.png", "badge"
	case modeEmoji:
		return "emoji.webp", "emoji"
	default:
		return "image.webp", ""
	}
}

// ProxiedURL builds the media-proxy URL for rawURL. For the internal proxy it
// appends an HMAC sig over the RAW (pre-encoding) url string, which is exactly
// what the proxy's Authorize -> VerifyHMAC checks against the url query param
// after Echo url-decodes it. For an external proxy no sig is appended.
func (c *MediaURLContext) ProxiedURL(rawURL string, mode proxyMode) string {
	filename, flag := mode.fileAndFlag()
	q := url.Values{}
	q.Set("url", rawURL)
	if flag != "" {
		q.Set(flag, "1")
	}
	if !c.externalEnabled {
		q.Set("sig", signURL(c.secret, rawURL))
	}
	return c.mediaProxyBase + "/" + filename + "?" + q.Encode()
}

// StaticEmojiProxyURL builds the proxy URL that serves an emoji as a still
// image (#2905).
//
// context 未配線なら空文字を返す (呼び出し元は raw URL へ 302 する)。
func StaticEmojiProxyURL(rawURL string) string {
	return staticProxyURL(rawURL, modeEmoji)
}

// StaticAvatarProxyURL builds the proxy URL that serves an avatar as a still
// image (#2908).
//
// `/avatar/@acct` の redirect 先に使う。frontend は静止画設定のとき
// `getStaticImageUrl('/avatar/@u@h')` を呼び、それは
// `<mediaProxy>/static.webp?url=<instance>/avatar/@u@h&static=1` になる。
// **その URL は allowlist のどの列にも無い**ので 403 + `max-age=86400` で
// 1 日壊れる。`/avatar/` 側が static を受けて自分でプロキシ URL を組めば、
// frontend は素の `/avatar/@u@h?static=1` を出せばよくなる。
//
// context 未配線なら空文字を返す。
func StaticAvatarProxyURL(rawURL string) string {
	return staticProxyURL(rawURL, modeAvatar)
}

// BadgeEmojiProxyURL builds the proxy URL that serves an emoji as a push
// notification badge (#2909).
//
// upstream (`ServerService.ts` の `/emoji/:path`) は `badge` が来ると
// `${mediaProxy}/emoji.png?url=…&badge=1` へ飛ばす。**`emoji=1` は付けず、
// `static` も見ない** — badge は 96x96 の silhouette PNG 固定 (暗いところが
// 透明。mk-go 側も `mediaproxy.processBadge` が同じ形で返す) なので、絵文字の
// リサイズ寸法もアニメーションの有無も結果に影響しない。**元画像がほぼ単色なら
// 404 になる** (#2920) 点も upstream と同じ。
//
// 組み立ては ProxiedURL に任せる (url / flag / sig / base の付け方を 1 箇所に
// 保つ)。modeBadge のファイル名と flag は fileAndFlag が持つ。
//
// context 未配線なら空文字を返す (呼び出し元は raw URL へ 302 する)。
func BadgeEmojiProxyURL(rawURL string) string {
	c := currentMediaURLContext()
	if c == nil {
		return ""
	}
	return c.ProxiedURL(rawURL, modeBadge)
}

// staticProxyURL builds a signed proxy URL that keeps the mode's resize
// geometry while forcing a still image.
//
// **`ProxiedURL` と同じ組み立てを使うのが要点 (#2905)。** `/proxy` を手で組むと
// `sig` が付かず、`Authorize` が HMAC ではなく DB allowlist の 4 テーブル UNION に
// 落ちる。`/emoji/:path` も `/avatar/@acct` も mention chip / リアクションアイコンの
// ホットパスなので毎リクエスト DB を引くことになる。**DB の瞬断が
// 403 + `max-age=86400` で 1 日キャッシュされる**という更に悪い形は #3036 で
// 潰したが (503 + `no-store` になった)、DB を引く回数そのものは署名を
// 付けないと減らない。
//
// **mode の flag と static の両方を立てる。** proxyMode は 1 つしか選べないが、
// 静止画は「その mode のリサイズ寸法で、ただしアニメーションを止める」意味なので
// 両方が要る (upstream の `animated: !('static' in query)` と同じ考え方)。
func staticProxyURL(rawURL string, mode proxyMode) string {
	c := currentMediaURLContext()
	if c == nil {
		return ""
	}
	filename, flag := mode.fileAndFlag()
	q := url.Values{}
	q.Set("url", rawURL)
	if flag != "" {
		q.Set(flag, "1")
	}
	q.Set("static", "1")
	if !c.externalEnabled {
		q.Set("sig", signURL(c.secret, rawURL))
	}
	return c.mediaProxyBase + "/" + filename + "?" + q.Encode()
}

// shouldProxyRemote reports whether remote-origin media should be wrapped.
// True by default (proxyRemoteFiles defaults true); only false when an operator
// disabled proxyRemoteFiles and configured no external proxy.
func (c *MediaURLContext) shouldProxyRemote() bool {
	return c.externalEnabled || c.proxyRemoteFilesNow()
}

// isRemoteOrigin reports whether rawURL points at a host we do not serve from.
// Relative URLs (e.g. "/identicon/...", "/files/...") and same-origin absolute
// URLs are treated as local and never proxied, which also prevents
// double-proxying an already-local /proxy or /files URL.
//
// 「自分のホスト」は instance ドメインだけではない。オブジェクトストレージを
// 有効にすると、自分が保存したファイルの URL は CDN / R2 の公開ドメインを指す。
// instance ドメインとの比較だけで判定すると**自分のファイルまで remote 扱い**に
// なり、全ドライブトラフィックが media proxy を経由する。オブジェクトストレージ
// に逃がした意味が無くなるうえ、mk-go が全画像の再配信を背負う (#2315)。
//
// upstream は host 比較ではなく `file.isLink` で判定するのでこの問題が起きない。
// mk-go が host 比較を採るのは、remote attachment を link 形式で持つ都合で
// raw URL しか手元に無い経路 (avatar / banner 等) があるため。判定材料を
// 「自分が配信に使うホスト集合」に広げることで、フラグの設定漏れに依存せず
// 両方を満たす。
func (c *MediaURLContext) isRemoteOrigin(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, c.instanceHost) {
		return false
	}
	if h := c.ownMediaHost(); h != "" && strings.EqualFold(u.Host, h) {
		return false
	}
	return true
}

// ownMediaHost returns the host of the configured object storage public base
// URL, or "" when object storage is not in use. 設定は admin が変更しうるので
// resolver 経由で都度引く (metaRepo は cached 実装なので安価)。
func (c *MediaURLContext) ownMediaHost() string {
	if c.ownMediaBaseURL == nil {
		return ""
	}
	base := c.ownMediaBaseURL()
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return u.Host
}

// GetPublicURL mirrors DriveFileEntityService.getPublicUrl for the DriveFile
// `url` field shown to **other** users. Local files are returned unchanged;
// remote-origin files are wrapped through the proxy. A nil receiver returns the
// unproxied value (= `webpublicUrl ?? url`; preserves the pre-#1529 behavior for
// call sites / tests that have no context wired — **原本ではない**ことに注意。
// 所有者向けの raw url は `GetSelfURL`).
//
// **webpublic があればそちらを指す (upstream `file.webpublicUrl ?? file.url`)。**
// webpublic は EXIF を落とした再エンコード版なので、原本を指すと**撮影位置
// (GPS) が公開側に出る**。mk-go はここが `f.URL` 固定だった。
// 所有者自身に見せる URL は `GetSelfURL`。
func (c *MediaURLContext) GetPublicURL(f *model.DriveFile, mode proxyMode) string {
	return c.publicURL(f, mode, WebpublicOrOriginalURL(f))
}

// GetSelfURL is the `url` field shown to the file's **owner** (and to admin
// moderation views).
//
// **原本を指す** (upstream の `pack({self: true})` は `file.url` をそのまま
// 返す)。所有者には EXIF 込みの原本が要る — 落とした版しか手に入らないと
// ダウンロードが劣化コピーになる。
//
// remote origin を proxy 経由にする点だけは upstream と違う (moderator の IP
// 保護、#1529 / #1948-14 で文書化済み)。
func (c *MediaURLContext) GetSelfURL(f *model.DriveFile, mode proxyMode) string {
	return c.publicURL(f, mode, f.URL)
}

// WebpublicOrOriginalURL mirrors upstream's `file.webpublicUrl ?? file.url`:
// the URL that may be shown to anyone other than the file's owner.
//
// **原本 (`f.URL`) は所有者にしか渡さない。** webpublic は EXIF / XMP を落とした
// 再エンコード版で、`internal/core/drive` はメタデータがある画像に対してこれを
// 作り「そちらを他人に見せる」前提で組まれている (`imagemeta.go` の
// `hasStrippableMetadata`)。原本を公開経路に載せると撮影位置が漏れる。
//
// **プロキシには通さない。** 呼び出し側が保存する値にも使うため、ここで
// `ProxiedURL` を挟むと HMAC 付きの URL が DB に入り、プロキシの secret を
// 変えた瞬間に無効になる。リモート origin の包み直しは packer 側
// (`ProxyAvatarURL` ほか) が行う。
func WebpublicOrOriginalURL(f *model.DriveFile) string {
	if f.WebpublicURL != nil && *f.WebpublicURL != "" {
		return *f.WebpublicURL
	}
	return f.URL
}

// publicURL is the shared body of GetPublicURL / GetSelfURL. base is the URL
// the caller wants when no proxying applies.
func (c *MediaURLContext) publicURL(f *model.DriveFile, mode proxyMode, base string) string {
	if c == nil {
		return base
	}
	// proxy が default mode で再配信できるのは browsersafe な image だけ
	// (mediaproxy passThrough は PDF/zip 等の non-image MIME を拒否する)。
	// non-image の remote url を wrap するとダウンロードが壊れるため、url
	// フィールドの proxy 化は image に限定する。video の poster は
	// GetThumbnailURL が static image として別途 proxy 化するので、ここで
	// 非 image を raw にしても timeline 上の自動読み込み画像は漏れない。
	// non-image file は user がクリックして初めて取得され、IP 露出はその時
	// だけに限定される (proxy が任意 MIME を配信できるようになるまでの妥協)。
	if !isImageMime(f.Type) {
		return base
	}
	// remote + external proxy: upstream proxies the canonical AP uri.
	if c.externalEnabled && f.URI != nil && f.UserHost != nil {
		return c.ProxiedURL(*f.URI, mode)
	}
	if c.shouldProxyRemote() && c.isRemoteOrigin(base) {
		return c.ProxiedURL(base, mode)
	}
	return base
}

// GetWebpublicURL wraps the webpublicUrl field when it points at a remote
// origin. For mk-go link-format remote files webpublicUrl is nil, so this is a
// no-op there; it only matters if a remote webpublic ever gets stored.
func (c *MediaURLContext) GetWebpublicURL(f *model.DriveFile) *string {
	if c == nil || f.WebpublicURL == nil || *f.WebpublicURL == "" {
		return f.WebpublicURL
	}
	// webpublic は image variant 専用だが、念のため non-image は wrap しない
	// (GetPublicURL と同じく proxy の browsersafe 制約を尊重する)。
	if !isImageMime(f.Type) {
		return f.WebpublicURL
	}
	if c.shouldProxyRemote() && c.isRemoteOrigin(*f.WebpublicURL) {
		s := c.ProxiedURL(*f.WebpublicURL, modeDefault)
		return &s
	}
	return f.WebpublicURL
}

// GetThumbnailURL mirrors getThumbnailUrl. It reuses resolveThumbnailURL for
// the NULL-fallback chain (#460: image thumbnails must never be null) and then
// wraps the result through the proxy when it is a remote origin. A nil receiver
// returns the raw fallback (pre-#1529 behavior).
func (c *MediaURLContext) GetThumbnailURL(f *model.DriveFile) *string {
	if c == nil {
		return resolveThumbnailURL(f)
	}
	// video: a stored thumbnail wins (localized if remote); otherwise ask the
	// proxy to extract a static frame from the (possibly remote) source.
	if strings.HasPrefix(f.Type, "video/") {
		if f.ThumbnailURL != nil && *f.ThumbnailURL != "" {
			if c.shouldProxyRemote() && c.isRemoteOrigin(*f.ThumbnailURL) {
				s := c.ProxiedURL(*f.ThumbnailURL, modeStatic)
				return &s
			}
			return f.ThumbnailURL
		}
		// stored thumbnail が無い video は proxy に still frame 抽出を頼む。
		// これは privacy 対策ではなく機能なので、自前ホストのファイルでも
		// 通す (thumbnail 生成に失敗した local video の poster がここで出る)。
		if c.shouldProxyRemote() && c.isRemoteOrigin(f.URL) {
			s := c.ProxiedURL(f.URL, modeStatic)
			return &s
		}
		return f.ThumbnailURL
	}
	// remote + external proxy: proxy the canonical AP uri as a static thumb.
	if c.externalEnabled && f.URI != nil && f.UserHost != nil {
		s := c.ProxiedURL(*f.URI, modeStatic)
		return &s
	}
	base := resolveThumbnailURL(f)
	if base == nil {
		return nil
	}
	if c.shouldProxyRemote() && c.isRemoteOrigin(*base) {
		s := c.ProxiedURL(*base, modeStatic)
		return &s
	}
	return base
}

// proxyIfRemote wraps p through the proxy in the given mode when p is a remote
// origin, otherwise returns p unchanged. nil/empty pointers pass through.
func (c *MediaURLContext) proxyIfRemote(p *string, mode proxyMode) *string {
	if c == nil || p == nil || *p == "" {
		return p
	}
	if c.shouldProxyRemote() && c.isRemoteOrigin(*p) {
		s := c.ProxiedURL(*p, mode)
		return &s
	}
	return p
}

// ProxyAvatarURL wraps a remote-origin avatar URL through the proxy (avatar
// mode), leaving local/identicon URLs untouched. Used by IdenticonURL so every
// avatar-bearing response (UserLite, chat packUser, stream) is leak-safe.
func (c *MediaURLContext) ProxyAvatarURL(rawURL string) string {
	if c == nil {
		return rawURL
	}
	if c.shouldProxyRemote() && c.isRemoteOrigin(rawURL) {
		return c.ProxiedURL(rawURL, modeAvatar)
	}
	return rawURL
}

// ProxyBannerURL wraps a remote-origin banner URL through the proxy. Banners
// are not resized (modeDefault / pass-through), matching upstream's
// getPublicUrl(banner) with no mode.
func (c *MediaURLContext) ProxyBannerURL(p *string) *string {
	return c.proxyIfRemote(p, modeDefault)
}

// ProxyMediaURL wraps a raw media URL string through the proxy (modeDefault)
// when it is a remote origin, otherwise returns it unchanged. Exported for
// callers outside this package (e.g. channel banner packing) that already have
// the resolved URL string and just need the leak-safe rewrite (#1529).
func ProxyMediaURL(rawURL string) string {
	c := currentMediaURLContext()
	if c == nil {
		return rawURL
	}
	if c.shouldProxyRemote() && c.isRemoteOrigin(rawURL) {
		return c.ProxiedURL(rawURL, modeDefault)
	}
	return rawURL
}

// ProxyAvatarURLString is the package-level form of ProxyAvatarURL for callers
// outside this package.
//
// `/avatar/@acct` の redirect 先を leak-safe にするのに使う (#2425)。API 応答は
// entity 側で既に wrap 済みだが、**あの endpoint だけ生の URL を Location に
// 入れていた**ので、mention chip を表示するだけで閲覧者の IP が相手インスタンス
// へ渡っていた。
func ProxyAvatarURLString(rawURL string) string {
	return currentMediaURLContext().ProxyAvatarURL(rawURL)
}

// ProxyEmojiURLString wraps a custom emoji URL through the proxy in emoji mode.
//
// `/emoji/:path` の redirect 先に使う (#2425)。upstream `ServerService.ts` も
// この endpoint では `${mediaProxy}/emoji.webp?url=...&emoji=1` へ飛ばしており、
// **生の URL を返していた mk-go の方が乖離していた。**
//
// API 応答の `emojis` field を server-proxy してはいけない (frontend が
// `meta.mediaProxy` で client 側プロキシするので二重になる) のとは**別の経路**。
// こちらは frontend が URL を包まないので、包まないと生で外へ出る。
func ProxyEmojiURLString(rawURL string) string {
	c := currentMediaURLContext()
	if c == nil {
		return rawURL
	}
	if c.shouldProxyRemote() && c.isRemoteOrigin(rawURL) {
		return c.ProxiedURL(rawURL, modeEmoji)
	}
	return rawURL
}

// ProxyMediaURLPtr is the nil-safe pointer form of ProxyMediaURL. It wraps a
// remote-origin URL pointer through the proxy, passing nil/empty and local
// URLs through unchanged. Used for optional admin-set image fields that the
// frontend renders directly in <img src> — role iconUrl, announcement imageUrl
// — so a remote URL there cannot leak the viewer's IP either (#1529 hardening).
func ProxyMediaURLPtr(p *string) *string {
	if p == nil || *p == "" {
		return p
	}
	s := ProxyMediaURL(*p)
	return &s
}

// UserSuppliedProxyTTL bounds how long a signature minted for a
// user-supplied URL stays valid.
//
// **プレビュー応答自身のキャッシュより長く、しかし有限に。** `/url` の応答は
// `max-age=86400, immutable` で 1 日キャッシュされるので、署名がそれより早く
// 死ぬと**キャッシュに残ったプレビューの画像だけが壊れる**。7 日なら 6 日の
// 余裕があり、貼り付けた proxy URL が恒久的に生き続けることもない。
const UserSuppliedProxyTTL = 7 * 24 * time.Hour

// ProxyUserSuppliedMediaURLPtr is ProxyMediaURLPtr for URLs that came from a
// user-supplied document (URL preview の OGP 画像 / favicon)。
//
// **署名に期限を付けるのが違い (#3037)。** `/url` は未認証で任意の URL を
// 渡せるので、攻撃者は自分のページの `og:image` に好きな URL を書いておけば
// **その URL に対する proxy 署名を発行させられる**。`ProxyMediaURLPtr` が
// 出す署名は URL だけを覆っていて**無期限**なので、一度取れば allowlist
// (`user.avatarUrl` / `emoji.originalUrl` / `instance.iconUrl` /
// `drive_file.url` のどれかに実在する URL だけを通す mk-go 独自の硬化) を
// 恒久的に迂回できた。
//
// **管理者が設定する画像 (ロールのアイコン / お知らせの画像 / チャンネルの
// バナー) には付けない。** あちらは値を入れられるのが管理者だけで、しかも
// 長期間そのまま配る前提なので、期限を付けると「いつの間にか画像が消える」
// 側の事故になる。
func ProxyUserSuppliedMediaURLPtr(p *string) *string {
	if p == nil || *p == "" {
		return p
	}
	c := currentMediaURLContext()
	if c == nil {
		return p
	}
	if !c.shouldProxyRemote() || !c.isRemoteOrigin(*p) {
		return p
	}
	// mode は `ProxyMediaURLPtr` と同じ (modeDefault)。**ここで upstream の
	// `preview.webp?preview=1` に寄せない** — 署名の期限とは別の話で、
	// 混ぜると「どちらの変更が何を壊したか」が分からなくなる。
	s := c.expiringProxiedURL(*p, modeDefault)
	return &s
}

// expiringProxiedURL is ProxiedURL with a deadline baked into the signature.
//
// 外部プロキシ構成では署名を出さない (`ProxiedURL` と同じ) — 署名を見るのは
// mk-go 自身の `/proxy` だけなので、外部に渡す URL に付けても意味が無い。
func (c *MediaURLContext) expiringProxiedURL(rawURL string, mode proxyMode) string {
	filename, flag := mode.fileAndFlag()
	q := url.Values{}
	q.Set("url", rawURL)
	if flag != "" {
		q.Set(flag, "1")
	}
	if !c.externalEnabled {
		q.Set("sig", signURLUntil(c.secret, rawURL, time.Now().Add(UserSuppliedProxyTTL)))
	}
	return c.mediaProxyBase + "/" + filename + "?" + q.Encode()
}

// signURLUntil MUST stay byte-for-byte identical to
// internal/core/mediaproxy.SignURLUntil (mediaurl_test.go asserts it, same as
// signURL / SignURL).
func signURLUntil(secret []byte, rawURL string, until time.Time) string {
	exp := strconv.FormatInt(until.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	// タグは `mediaproxy.expiringDigestTag` と同じ値 (parity test が固定する)。
	mac.Write([]byte("mk-go/expiring-proxy-sig\x00"))
	mac.Write([]byte(rawURL))
	mac.Write([]byte("\n"))
	mac.Write([]byte(exp))
	return exp + "." + hex.EncodeToString(mac.Sum(nil))
}

// signURL is hex(HMAC-SHA256(secret, rawURL)). It MUST stay byte-for-byte
// identical to internal/core/mediaproxy.SignURL — mediaurl_test.go asserts this
// so a refactor there can't silently desync and make every proxied URL 403.
func signURL(secret []byte, rawURL string) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(rawURL))
	return hex.EncodeToString(mac.Sum(nil))
}

// mediaURLContext is process-wide so PackDriveFile / IdenticonURL can rewrite
// URLs without threading config through their many call sites (mirrors the
// SetCanChatLookup / SetAvatarDecorationLookup pattern). Router wires it once at
// startup; nil leaves all URLs raw, keeping context-less tests green.
var (
	mediaURLContextMu sync.RWMutex
	mediaURLContext   *MediaURLContext
)

// SetMediaURLContext wires the media-proxy URL rewriter. Pass nil to clear
// (used in tests).
func SetMediaURLContext(c *MediaURLContext) {
	mediaURLContextMu.Lock()
	mediaURLContext = c
	mediaURLContextMu.Unlock()
}

// currentMediaURLContext returns the registered context (may be nil). All
// MediaURLContext methods are nil-safe so callers can use the result directly.
func currentMediaURLContext() *MediaURLContext {
	mediaURLContextMu.RLock()
	c := mediaURLContext
	mediaURLContextMu.RUnlock()
	return c
}
