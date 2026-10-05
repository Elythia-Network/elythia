package server

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// CSP modes accepted by the `frontendContentSecurityPolicy` config key.
const (
	// CSPModeOff sends no header. 既定。
	CSPModeOff = "off"
	// CSPModeReportOnly sends `Content-Security-Policy-Report-Only`, which
	// never blocks anything and only reports violations.
	CSPModeReportOnly = "report-only"
	// CSPModeEnforce sends `Content-Security-Policy`.
	CSPModeEnforce = "enforce"
)

// CSPReportPath is where browsers post violation reports.
//
// `/api` 配下に置かない。あちらは auth / body limit / rate limit の既定が
// API 用に組んであり、認証不要で外から叩かれるこの endpoint とは要件が違う。
const CSPReportPath = "/csp-report"

// frontendCSPDirectives is the policy shared by the SPA shell and the embed shell.
//
// **緩すぎると違反が出ず観測の意味が無い**ので、最終的に enforce したい形から
// 始めている。report-only の間に出た違反を潰してから enforce へ切り替える。
//
// **script 側の `'unsafe-inline'` は #2786 で外した。** SPA shell の inline script
// (`VERSION` / `CLIENT_ENTRY` の定義と bootloader) は内容が起動時に固定なので、
// SHA-256 hash を `script-src` に足して通す。style 側は Vue の `:style` が
// 属性として出るため残している (下の style-src のコメントを参照)。
//
// `frame-ancestors` は**入れない**。`X-Frame-Options: DENY` を
// `middleware/frameguard.go` が既に付けており、そちらは `/embed/` を除外する
// 仕組みを持つ。CSP に重ねると除外を二重管理することになり、片方だけ更新して
// 埋め込みが死ぬ。**#2789 で `/embed/` にもこの policy を適用したが、そのときも
// `frame-ancestors` は入れず除外は frameguard の 1 箇所に残した。**
//
// **embed との差は cspExtras 側だけ。** captcha の origin は SPA shell にしか
// 足さない (embed はサインアップ経路を持たない)。media origin (object storage /
// 外部 media proxy) と inline script の hash は両方に足す。
var frontendCSPDirectives = []string{
	"default-src 'self'",
	"base-uri 'self'",
	"object-src 'none'",
	"form-action 'self'",
	// captcha (hCaptcha / reCAPTCHA / Turnstile) の script origin は、meta の
	// enable フラグに連動して cspExtras.Script で**有効な業者の分だけ**足す
	// (#2502、captchaCSPExtras)。ここに常時書かない。
	//
	// esm.sh は Misskey frontend が shiki (コードブロックの syntax highlight) を
	// 動的 import する先。**upstream の vite 設定がそう作っている**
	// (`externalPackages` で `shiki/langs` `shiki/themes` を CDN に逃がす)。
	//
	// バンドルに切り替えれば外部依存を消せるが、**実測でビルド成果物が
	// 242MB → 508MB に倍増した** (2026-08-09)。Misskey frontend は 30 ロケール
	// 分を個別にビルドするため、shiki の言語 347 + テーマ 66 ファイルが
	// ロケールごとに複製され、JS が 13,576 → 23,610 ファイルに増える。
	// 利用者 1 人あたりの転送量は変わらない (自分のロケール分しか取らない) が、
	// 配布物とディスクが倍になる。
	//
	// 軽量さは mk-go の主要な利点なので、それを損なうより CDN を 1 つ許す方を
	// 選んだ。代償として、コードブロックを表示する閲覧者の IP とリファラが
	// esm.sh に渡り、CDN 側の可用性と完全性に依存する。
	//
	// 言語を絞ってバンドルすれば両立できる可能性はある (未検証)。
	// **`'unsafe-inline'` は入れない** (#2786)。SPA shell の inline script は
	// `cspScriptHashes` が出す `'sha256-...'` で通す。hash は HTML に埋める
	// 文字列そのものから導くので、片方だけ変えて壊れることはない。
	"script-src 'self' https://esm.sh",
	// **style 側の `'unsafe-inline'` は残す** (#2786)。Vue の `:style` バインディングが
	// 146 箇所あり、DOM の inline `style` 属性になる。属性は `style-src-attr` の
	// 管轄で hash では救えず (`'unsafe-hashes'` が要る)、外すと UI が広範に壊れる。
	// splash の `<style>:root{--splash-color:...}</style>` も meta 由来で動的。
	"style-src 'self' 'unsafe-inline'",
	// 画像は media proxy 経由で来る。**internal proxy なら同一オリジン**で、
	// 外部 media proxy 構成ではその origin を extraMediaOrigins で足す (#2501)。
	// data: / blob: はクライアント側でのプレビュー生成 (画像編集・録音等) が使う。
	"img-src 'self' data: blob:",
	// **動画・音声は img と違いリモート origin を直接読む** (#2501)。media proxy
	// が扱うのは画像だけで、リンク参照のリモート添付は MkMediaVideo が
	// `<source :src="file.url">` で相手サーバーから直接取得する。'self' に絞ると
	// リモートの動画・音声が全滅する (実際に本番で全滅していた)。upstream は
	// CSP 自体を設定せず全 origin を許すので、https: を許すのが parity。
	// http: は mixed content でどのみちブロックされるため足さない。
	"media-src 'self' https: data: blob:",
	"font-src 'self' data:",
	// streaming は同一オリジンの WebSocket。ws: も許すのは http 配信の
	// 開発環境で wss: にならないため。esm.sh は script-src で既に信頼している
	// CDN で、実測で .map (source map) 取得の connect-src 違反 report が届いて
	// 観測を汚すため明示する (#2501。取得元はブラウザや拡張に依存し、信頼先は
	// 増えない)。
	"connect-src 'self' ws: wss: https://esm.sh",
	"worker-src 'self' blob:",
	// URL プレビューの埋め込みプレイヤー (MkUrlPreview の player.url iframe) は
	// YouTube 等の任意 origin を指す (#2501)。iframe 側の sandbox 属性で制約
	// されており (upstream 設計)、'self' に絞るとプレイヤーが全滅する。
	"frame-src 'self' https:",
}

// mediaDirectives are the directives that must also allow the media origins
// (object storage / 外部 media proxy)。drive のファイルはそこから直接配信される。
//
// **connect-src にも足す** (#2501)。通知音にドライブのファイルを設定すると、
// クライアントは `<audio>` ではなく fetch + decodeAudioData で読む (sound.ts)。
// fetch は media-src でなく connect-src の支配下なので、ここに無いと object
// storage 構成で「通知音だけ鳴らない」という気付きにくい壊れ方をする。
var mediaDirectives = []string{"img-src", "media-src", "connect-src"}

// cspExtras carries operator-configuration-dependent origins, grouped by the
// directives they extend.
//
// **設定に応じた分だけ足す**のが方針。使っていない機能の origin を常時許可
// すると、ポリシーが「実際に必要な範囲」から乖離していく。
type cspExtras struct {
	// Media is appended to img-src / media-src / connect-src (object storage
	// と外部 media proxy、#2425 / #2501)。
	Media []string
	// Script is appended to script-src (有効な captcha provider、#2502)。
	Script []string
	// Connect is appended to connect-src only (hCaptcha の API 呼び出し。
	// 公式 CSP ガイダンスの要求、#2502)。
	Connect []string
	// Style is appended to style-src (hCaptcha。公式 CSP ガイダンスが style-src
	// まで要求しており、資材の変わり方によっては欠けると黙って壊れる、#2502)。
	Style []string
	// Image is appended to img-src only (upstream のクレジットページが読む
	// 外部ホスト、#2892)。**`Media` と分けてある** — あちらは media-src /
	// connect-src にも足すが、こちらは画像しか読まない。
	Image []string
}

// creditImageOrigins are the hosts that upstream's `/about-misskey` reads
// contributor / sponsor / patron icons from.
//
// **実測 62 枚** — `avatars.githubusercontent.com` が 6 (プロジェクトメンバー)、
// `assets.misskey-hub.net` が 56 (スポンサー 6 + パトロン 50)。`loading="lazy"` も
// `v-if` も折りたたみも無いので、ページを開いた時点で全部読まれる。
//
// #2700 で **upstream の謝辞は消さない**方針にした (頻繁に更新されるファイルなので
// 書き換えると追従のたびにコンフリクトを手で解くことになる)。この 2 origin を
// 許さないと、そのページには恒久的に壊れた画像が 62 個並ぶ (#2892)。
//
// **media proxy 経由にはできない。** mk-go の proxy は upstream と違い open proxy
// ではなく、allowlist は **DB に実在する URL** (user の avatar / banner、drive_file、
// emoji、instance の icon / favicon) だけを通す。静的にハードコードされた URL は
// 入らないので 403 が返る (実測)。
//
// **「外部 origin を許すと投稿経由でトラッキング画像を読ませる経路が開く」は
// 任意 origin の話。** ここで足すのは固定の 2 つで、投稿から別の host を読ませる
// ことはできない。**閲覧者の IP はこの 2 host に渡る** — upstream は CSP 自体を
// 持たないので元から同じ挙動で、mk-go だけが厳しかった箇所を parity に戻す形。
var creditImageOrigins = []string{
	"https://avatars.githubusercontent.com",
	"https://assets.misskey-hub.net",
}

// buildFrontendCSP renders the policy string.
//
// extras には object storage の origin 等、`'self'` 以外に取得を許す必要がある
// host を渡す。**空でない限りポリシーは operator の設定に依存する**ので、
// 静的な定数にはできない。
//
// `report-uri` は deprecated だが、`report-to` だけにすると Reporting-Endpoints
// header を解さない環境で 1 件も届かない。観測が目的なので両方は出さず、対応
// 範囲の広い `report-uri` に寄せる。
func buildFrontendCSP(withReport bool, extras cspExtras) string {
	d := make([]string, 0, len(frontendCSPDirectives)+1)
	for _, dir := range frontendCSPDirectives {
		d = append(d, appendExtras(dir, extras))
	}
	if withReport {
		d = append(d, "report-uri "+CSPReportPath)
	}
	return strings.Join(d, "; ")
}

// cspScriptHashes returns `'sha256-<base64>'` for each non-empty inline script.
//
// **呼び出し側は HTML に埋める文字列そのものを渡すこと。** 同じ内容を 2 箇所で
// 組み立てると、片方だけ変えたときに CSP を有効にしているインスタンスで
// script が丸ごとブロックされ、画面が真っ白になる (#2786)。
//
// 空文字は飛ばす。loader は外部参照になることがあり (`inlineOrLinkJS`)、その
// ときは inline script が存在しないので hash も要らない。
//
// CSP の hash は **script 要素の中身をそのまま** (前後の空白も含めて) 取る。
// `<script>` タグの属性や改行の入れ方を変えると一致しなくなるので、HTML 側の
// 埋め込みは `<script>%s</script>` の形を保つこと。
func cspScriptHashes(scripts ...string) []string {
	out := make([]string, 0, len(scripts))
	for _, s := range scripts {
		if s == "" {
			continue
		}
		sum := sha256.Sum256([]byte(s))
		out = append(out, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return out
}

// appendExtras adds the configuration-dependent origins to a directive.
func appendExtras(directive string, ex cspExtras) string {
	name, _, ok := strings.Cut(directive, " ")
	if !ok {
		return directive
	}
	var add []string
	if slices.Contains(mediaDirectives, name) {
		add = append(add, ex.Media...)
	}
	if name == "img-src" {
		add = append(add, ex.Image...)
	}
	if name == "script-src" {
		add = append(add, ex.Script...)
	}
	if name == "connect-src" {
		add = append(add, ex.Connect...)
	}
	if name == "style-src" {
		add = append(add, ex.Style...)
	}
	if len(add) == 0 {
		return directive
	}
	return directive + " " + strings.Join(add, " ")
}

// captchaCSPExtras returns the origins the enabled captcha providers need
// (#2502)。
//
// **有効な業者の分だけ、各社の公式 CSP ガイダンスにある要求だけ**を足す。
// frame 側は frame-src https: (#2501) で既に通る。mcaptcha は iframe +
// バンドル済みの glue (postMessage) で外部 script を読まないため何も要らず、
// testcaptcha はローカル完結。
func captchaCSPExtras(hcaptcha, recaptcha, turnstile bool) cspExtras {
	var ex cspExtras
	if hcaptcha {
		// 公式ガイダンスは script / frame / style / connect の 4 directive に
		// hcaptcha.com + *.hcaptcha.com を要求する (asset の subdomain は時期・
		// 地域で変わるため wildcard を使えという指定)。frame は https: で充足。
		origins := []string{"https://hcaptcha.com", "https://*.hcaptcha.com"}
		ex.Script = append(ex.Script, origins...)
		ex.Connect = append(ex.Connect, origins...)
		ex.Style = append(ex.Style, origins...)
	}
	if recaptcha {
		// Misskey は recaptcha.net 経由で読み、api.js が本体を
		// www.gstatic.com/recaptcha/ から読む。公式推奨は path 付きだが、CSP の
		// path source は redirect 後に無視される仕様で保証にならないため、
		// objectStorageOrigin と同じく origin 単位で書く。gstatic 全体に script
		// 実行を許す広さは、CSP の path source が redirect 後に無視される以上
		// 避けられない。**#2786 で script-src から 'unsafe-inline' を外したので、
		// これは reCAPTCHA を有効にした instance でだけ script-src を広げる**
		// (以前は unsafe-inline があるので実質差が無かった)。
		ex.Script = append(ex.Script, "https://www.recaptcha.net", "https://www.gstatic.com")
	}
	if turnstile {
		// 公式の要求は script-src と frame-src のみ。connect-src は要求に
		// 無いので足さない (pre-clearance が使う /cdn-cgi/ は 'self' で通る)。
		ex.Script = append(ex.Script, "https://challenges.cloudflare.com")
	}
	return ex
}

// objectStorageOrigin extracts the scheme+host to allow from a configured
// object storage base URL.
//
// **path は落として origin だけにする。** CSP の source expression は path を
// 書けるが、`https://host/bucket` のように書くと「その prefix 配下だけ」を意味し、
// 実際の配信 URL がわずかに違う形 (末尾スラッシュの有無など) で外れる。origin
// 単位で許す方が確実で、object storage は専用ホストなので粒度としても妥当。
//
// 解釈できない値では "" を返す。**ゴミを CSP に混ぜない**のが要点で、不正な
// source expression が 1 つでもあるとブラウザがその directive ごと落とす実装が
// ある。
func objectStorageOrigin(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// applyFrontendCSP sets the CSP header on a shell response.
//
// 呼び出し元は SPA shell (`frontend.go`) と embed shell (`embed.go`) の 2 つ。
//
// mode が未知 / off なら何もしない。**判定できない値で enforce に倒さない**の
// が要点で、設定ミスでフロントが動かなくなるより無効の方が安全。
func applyFrontendCSP(c echo.Context, mode string, extras cspExtras) {
	switch mode {
	case CSPModeReportOnly:
		c.Response().Header().Set("Content-Security-Policy-Report-Only",
			buildFrontendCSP(true, extras))
	case CSPModeEnforce:
		// enforce でも報告は受け取る。ブロックが起きていることに気付けないと
		// 「一部の機能だけ動かない」という報告しか上がってこない。
		c.Response().Header().Set("Content-Security-Policy",
			buildFrontendCSP(true, extras))
	}
}

// cspMediaExtras returns the media origins that depend on instance settings.
//
// SPA shell (`frontend.go`) と embed (`embed.go`) で共通。drive のファイルが
// object storage から直接配信される構成と、外部 media proxy 構成では `'self'`
// だけでは足りず、enforce 時に画像・動画・音声が丸ごと表示できなくなる
// (#2425 / #2501)。embed も同じ経路でリモート画像を出すので同じものが要る (#2789)。
//
// **順序は object storage → media proxy で固定する。** header の文字列がこの
// 順序で決まるので、入れ替えると値だけ変わって差分がノイズになる。
func cspMediaExtras(cfg *config.Config, m *model.Meta) []string {
	var out []string
	// useObjectStorage が false でも baseUrl が残っていることがあるので、
	// **両方が揃っているときだけ**許可する。使っていない host を CSP に
	// 載せる必要は無い。
	if m != nil && m.UseObjectStorage && m.ObjectStorageBaseURL != nil {
		if origin := objectStorageOrigin(*m.ObjectStorageBaseURL); origin != "" {
			out = append(out, origin)
		}
	}
	// 外部 media proxy 構成では、リモート画像もカスタム絵文字も proxy の origin
	// から配信される。internal proxy ('self') なら何も足さない (#2501)。
	if cfg != nil && cfg.ExternalMediaProxyEnabled {
		if origin := objectStorageOrigin(cfg.MediaProxy); origin != "" {
			out = append(out, origin)
		}
	}
	return out
}
