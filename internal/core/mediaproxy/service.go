package mediaproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	// 注意 (#672 Phase 1): TGA decoder は `blezek/tga` (auto-register 無し)
	// を採用し、`decodeImage` 内で MIME 判定で manual dispatch している。
	// 同 fork 元の `github.com/ftrvxmtrx/tga` は init() で
	// `image.RegisterFormat("tga", "", ...)` (magic bytes 空) するため、
	// blank import すると他 image format (PNG/JPEG/WebP/...) の自動 dispatch
	// を破壊する。**`_ "github.com/ftrvxmtrx/tga"` を絶対追加しないこと**。

	"github.com/gen2brain/avif"
	_ "github.com/gen2brain/heic" // HEIC/HEIF input decode (iPhone uploads)
	_ "github.com/gen2brain/jpegxl"
	"github.com/gen2brain/webp"
	"github.com/kovidgoyal/imaging"
	_ "github.com/mrjoshuak/go-jpeg2000" // JP2/J2K input decode (#734)
	"github.com/shiroha-a/mk/internal/misc/imagedecode"
	_ "github.com/spakin/netpbm" // PBM/PGM/PPM/PAM input decode (#672 Phase 1)
	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"

	"golang.org/x/sync/semaphore"

	coredrive "github.com/shiroha-a/mk/internal/core/drive"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/safehttp"
)

// ProxyMode enumerates the image processing modes.
type ProxyMode int

const (
	ModeDefault ProxyMode = iota
	ModeEmoji
	ModeAvatar
	ModeStatic
	ModePreview
	ModeBadge
)

// String implements fmt.Stringer so log output is readable.
//
// **無いと slog が int をそのまま出す** (`"mode":2`)。shed のログは
// operator に手掛かりを残すのが目的なので、数字だけでは意味が無い
// (#2849 が `Scheme.String()` で直したのと同型)。
func (m ProxyMode) String() string {
	switch m {
	case ModeDefault:
		return "default"
	case ModeEmoji:
		return "emoji"
	case ModeAvatar:
		return "avatar"
	case ModeStatic:
		return "static"
	case ModePreview:
		return "preview"
	case ModeBadge:
		return "badge"
	default:
		return fmt.Sprintf("ProxyMode(%d)", int(m))
	}
}

// 画像処理パラメータ (Misskey TS準拠)
const (
	emojiHeight   = 128
	avatarHeight  = 320
	staticWidth   = 498
	staticHeight  = 422
	previewWidth  = 200
	previewHeight = 200
	badgeSize     = 96
	// badgeMinEntropy は upstream の `stats().entropy < 0.1` と同じ閾値 (#2920)。
	badgeMinEntropy = 0.1
	// sharp の normalise() の既定 (実測)。min-max ではない。
	badgeNormaliseLower = 1.0
	badgeNormaliseUpper = 99.0
	webpQuality         = 77
	avifQuality         = 60       // AVIF default; matches gen2brain/avif.DefaultQuality
	avifSpeed           = 8        // 0..10 — 8 keeps quality close to default while staying responsive
	maxDownload         = 32 << 20 // 32 MB
)

// maxDecodedPixels caps width*height *after* decode so a pixel-bomb input
// (a few hundred kB AVIF/HEIC/JXL that decodes to 30000x30000 = 3.6 GB
// raster) cannot OOM the proxy. Mirrors sharp's `limitInputPixels`
// default. 8192*8192 = 64 MP keeps every realistic phone/camera input
// flowing while rejecting clearly malicious payloads (#637 review UR-016).
//
// var (not const) so tests can shadow the limit without synthesizing a 64 MP
// fixture. Production paths must not assign to it.
var maxDecodedPixels int64 = 8192 * 8192

var (
	ErrUnauthorized = errors.New("mediaproxy: unauthorized URL")
	ErrNotFound     = errors.New("mediaproxy: resource not found")
	ErrBadRequest   = errors.New("mediaproxy: bad request")
	ErrTooLarge     = errors.New("mediaproxy: file too large")
	// ErrUpstreamUnavailable はリモートからバイト列を取れなかったことを表す
	// (#3034)。**「無い」とは言えない**ので `ErrNotFound` と分ける — DNS
	// 失敗・接続拒否・TLS エラー・タイムアウトはどれもリモート側の一時障害
	// で、復旧すれば同じ URL が引ける。
	ErrUpstreamUnavailable = errors.New("mediaproxy: upstream fetch failed")
	// ErrAllowlistUnavailable は allowlist を**引けなかった**ことを表す
	// (#3036)。**「許可されていない」(`ErrUnauthorized`) と分ける** —
	// **分けないと** handler が後者も 403 + `Cache-Control: max-age=86400` で
	// 返し、PostgreSQL が落ちているあいだ `sig` を持たない**すべての**
	// プロキシ URL が 403 になって 1 日焼き付く (#3036 以前の挙動)。
	//
	// **「Go 側のプール枯渇」はここに来ない。** `database/sql` は枠が埋まって
	// いるとエラーではなく**ブロック**し、`/proxy` のリクエスト context に
	// deadline は無いので、枯渇は「遅い応答」か「離脱 →
	// `context.Canceled` → 499」として現れる。client 側の `statement_timeout`
	// も設定していない。
	//
	// **PostgreSQL 側の `max_connections` 枯渇は来る** —
	// `FATAL: sorry, too many clients already` はエラーとして返るので
	// 503 になる。503 の嵐を調査するときの最有力候補。
	// #2913 (403 が 1 日キャッシュされてアイコンが 1 日壊れた) と同じ症状で、
	// #2792 の「lookup error を種別を見ずに 4xx へ潰さない」にも反する。
	//
	// #3034 が直した「リモート取得の失敗を 404 に潰す」より影響が広い —
	// あちらは 1 URL ずつだが、こちらは障害中の全 URL が同時に焼き付く。
	ErrAllowlistUnavailable = errors.New("mediaproxy: allowlist lookup failed")
	// ErrTargetBlocked は SSRF ガードが接続を拒否したことを表す (#3037)。
	//
	// **`ErrSSRFBlocked` (同パッケージの再 export) とは別物。** あちらは
	// safehttp が返す**原因**で、こちらは mediaproxy の**分類**。原因は
	// `%w` で包んであるので `errors.Is(err, ErrSSRFBlocked)` も真になる。
	// `ErrBlocked` という名前にしないのは、`following` / `reaction` の
	// 同名が「利用者がブロックした」意味で使われているため。
	//
	// **`ErrUpstreamUnavailable` と分ける理由は「意味論」だけ。** あちらは
	// 「相手が落ちている」だが、こちらは「mk-go が拒否した」。502 に混ぜると
	// 監視で「相手インスタンスが落ちている」と読めてしまう。
	//
	// **「恒久的だから長期キャッシュでよい」とは書けない (レビューで実測)。**
	// SSRF の可否は**毎リクエストの DNS 解決**と operator 設定で決まる:
	// `privateRanges` には `0.0.0.0/8` と `64:ff9b::/96` (NAT64) が入るので、
	// DNS sinkhole (Pi-hole 等が `0.0.0.0` を返す) や DNS64 環境、
	// `allowedPrivateNetworks` の設定漏れで簡単に覆る。恒久的なのは URL が
	// IP リテラルのときだけ。だから**キャッシュ時間は 502 のときと同じ 5 分**
	// にしてある (#2913 は 403 + 1 日で 68.5% の利用者のアイコンを 1 日壊した)。
	//
	// **`ErrUnauthorized` にも相乗りさせない。** handler の 403 は
	// `Authorize` 失敗の経路で、合流させるとログと監視で「allowlist に無い」と
	// 「private IP を遮断した」が区別できなくなる。
	ErrTargetBlocked = errors.New("mediaproxy: blocked target")
)

// browsersafeMIMEs lists MIME types safe to serve inline in browsers.
var browsersafeMIMEs = map[string]bool{
	"image/png":  true,
	"image/gif":  true,
	"image/jpeg": true,
	"image/webp": true,
	"image/avif": true,
	"image/apng": true,
	"image/bmp":  true,
	"image/tiff": true,
	// HEIC/HEIF: Safari 16+ で表示可能。Firefox/Chrome は表示できないが
	// pass-through で返した先で frontend が WebP/AVIF 変換 URL に置換する
	// 想定 (#637 M4)。
	"image/heic": true,
	"image/heif": true,
	// JPEG XL: Safari 17+ 対応、Chrome は flag つきで実験中 (#637 M5)。
	"image/jxl": true,
	// `image/x-icon` は古い慣例、`image/vnd.microsoft.icon` は IANA media
	// type registry に登録されている公式名 (RFC 紐付け無し)。リモート
	// Misskey/Mastodon の favicon.ico は後者で返ってくるホストが多いため
	// 両方許可する (#418)。
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
	"image/vnd.mozilla.apng":   true,
	// Netpbm 系 (PBM/PGM/PPM): pure Go decode (#672 Phase 1)。browser native
	// 表示は無いが、convertible 判定で WebP/AVIF に変換されて配信される。
	"image/x-portable-bitmap":  true,
	"image/x-portable-graymap": true,
	"image/x-portable-pixmap":  true,
	"image/x-portable-anymap":  true,
	// TGA: pure Go decode (#672 Phase 1)。同上。
	"image/x-tga":   true,
	"image/x-targa": true,
	// JPEG 2000 (JP2 file format / J2K codestream): pure Go decode (#734)。
	// mrjoshuak/go-jpeg2000 が image.RegisterFormat 経由で JP2 / J2K の magic
	// bytes (`\x00\x00\x00\x0cjP\x20\x20\r\n\x87\n` / `\xff\x4f\xff\x51`) を
	// 登録するので decodeImage 経由で透過的に処理される。convertible 経路で
	// WebP/AVIF へ変換出力。
	"image/jp2":      true,
	"image/jpeg2000": true,
	// JPX (Part 2 拡張) は library が "not fully supported" としているため
	// convertible 経路には乗せず、pass-through のみ受け入れる。
	"image/jpx": true,
	// JPEG XR / MNG: decode 用 pure Go library が無いため pass-through で
	// 配信し browser ネイティブ対応 (Edge legacy / 一部 viewer plugin) に
	// 委譲する (#672 Phase 1 partial)。完全 transcode は cgo 依存となる
	// ため scope 外。
	"image/jxr":          true,
	"image/vnd.ms-photo": true,
	"video/x-mng":        true,
	"image/x-mng":        true,
	"video/x-jng":        true,
	"audio/opus":         true,
	"video/ogg":          true,
	"audio/ogg":          true,
	"application/ogg":    true,
	"video/quicktime":    true,
	"video/mp4":          true,
	"audio/mp4":          true,
	"video/x-m4v":        true,
	"audio/x-m4a":        true,
	"video/3gpp":         true,
	"video/3gpp2":        true,
	"video/mpeg":         true,
	"audio/mpeg":         true,
	"video/webm":         true,
	"audio/webm":         true,
	"audio/aac":          true,
	"audio/flac":         true,
	"audio/wav":          true,
}

// ProxyResult is the output of proxy resolution + processing.
type ProxyResult struct {
	Body        io.ReadCloser
	ContentType string
	// CacheControl overrides the handler's default `Cache-Control` for this
	// response. 空なら既定 (`max-age=31536000, immutable`)。
	//
	// **成功応答でも長期キャッシュが正しくない場合がある (#3035)。**
	// 生成に失敗してダミー画像へ倒れた応答は 200 で返るが、原因が一時的な
	// ものなら `immutable` で 1 年固定してはいけない。status を変えずに
	// キャッシュだけ変えたいので、error ではなく結果に載せる。
	CacheControl string
}

// DriveFileLookup is the minimal subset of repository.DriveFileRepository
// the proxy needs to resolve a `/files/<accessKey>` request to its cached
// thumbnail / webpublic variant when one exists (#637 M1)。
type DriveFileLookup interface {
	FindByAccessKey(accessKey string) (DriveFileVariants, error)
}

// DriveFileVariants exposes only the access-key fields the proxy needs.
// repository 側でこの shape を直接返さない (model.DriveFile が大きすぎる)
// ので、wire 層で adapter を書いて変換する。
//
// MimeType は upload 時に確定した値で、`http.DetectContentType` が認識
// しない HEIC/HEIF/AVIF/JXL を proxy が誤って `application/octet-stream`
// として扱うのを防ぐ (#637 review UR-017)。
type DriveFileVariants struct {
	AccessKey          *string
	ThumbnailAccessKey *string
	WebpublicAccessKey *string
	MimeType           string
}

// Service handles media proxy authorization and fetching.
type Service struct {
	instanceURL  string
	driveStorage coredrive.Storage
	allowlist    AllowlistChecker
	hmacSecret   []byte
	httpClient   *http.Client
	userAgent    string
	driveLookup  DriveFileLookup // optional, #637 M1
	// localStorage は driveStorage が object storage に切り替わったあとも
	// `storedInternal=true` な既存ファイルを提供するための fallback (#2315)。
	// optional — nil なら fallback しない。
	localStorage     coredrive.Storage
	videoThumbGen    string       // optional, #637 M2 (videoThumbnailGenerator base URL)
	videoThumbMode   string       // "post" (default) | "get" — wire selection
	videoThumbClient *http.Client // built lazily from videoThumbGen, supports unix:// scheme
	// cpuSlots は decode/resize/encode を同時に何本走らせてよいかの枠
	// (#3032)。同時に走る本数がそのまま同時に確保される中間バッファの
	// 本数になるので、開けっ放しだと AVIF のバーストでプロセスの RSS が
	// 1GB 近くまで伸びる。内蔵プロキシは API サーバーと同じプロセスなので、
	// それはインスタンス全体の RSS。根拠と実測は cpulimit.go を参照。
	cpuSlots *semaphore.Weighted
	// cpuConcurrency は cpuSlots に渡した容量。`semaphore.Weighted` は
	// 容量を読み出せないので、テストが既定値を確かめるために持っている
	// (production では書かれるだけ)。**消さないこと** — 読み手は
	// TestNewServiceInstallsCPULimit と
	// TestSetCPUConcurrencyRestoresDefaultForNonPositive の 2 つ。
	cpuConcurrency int
}

// NewService creates a new media proxy Service.
// allowedPrivateNetworks は SSRF 保護で許可するプライベート CIDR リスト (config.AllowedPrivateNetworks)。
// transportOpts は forward proxy 等の追加 transport 設定 (safehttp.WithProxy など)。
func NewService(instanceURL, userAgent string, driveStorage coredrive.Storage, allowlist AllowlistChecker, hmacSecret []byte, allowedPrivateNetworks []string, transportOpts ...safehttp.Option) *Service {
	s := &Service{
		instanceURL:  instanceURL,
		driveStorage: driveStorage,
		allowlist:    allowlist,
		hmacSecret:   hmacSecret,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: NewSSRFSafeTransport(allowedPrivateNetworks, transportOpts...),
		},
		userAgent: userAgent,
	}
	// **既定でも必ず枠を張る (#3032)。** 明示的に設定しなかった構成が
	// 無制限のままだと、塞ごうとしている状態がそのまま既定になる。
	s.SetCPUConcurrency(0)
	return s
}

// SetDriveLookup attaches a DriveFileLookup so the proxy can substitute the
// cached thumbnail / webpublic variant for `?preview` and `?static` requests
// against local files (#637 M1)。
func (s *Service) SetDriveLookup(l DriveFileLookup) {
	s.driveLookup = l
}

// SetLocalStorage wires the always-local backend used as a fallback for rows
// stored before object storage was enabled (#2315). Optional.
func (s *Service) SetLocalStorage(st coredrive.Storage) {
	s.localStorage = st
}

// SetDriveStorage replaces the storage backend (test ergonomics, #637 M1
// review nit).
func (s *Service) SetDriveStorage(st coredrive.Storage) {
	s.driveStorage = st
}

// SetVideoThumbnailGenerator wires an external thumbnail generator with the
// default wire mode (POST multipart). For UDS deployments pass
// `unix:///path/to/socket` and the proxy will dial the socket directly,
// ignoring the URL host. Empty disables the feature (default — proxy
// returns dummy PNG for video MIME, #637 M2).
//
// このコール先は operator-trusted endpoint (内部 sidecar / 自前 SaaS 想定)
// のため、`config.proxy` (#638 outbound forward proxy) は意図的に経由しな
// い。urlpreview の summalyProxy 経由 client (`internal/core/urlpreview/
// fetcher.go`) と同じ pattern。`config.proxy` を強制したい運用は別途
// nginx / sidecar 側で出力経路を制御する。
func (s *Service) SetVideoThumbnailGenerator(genURL string) {
	s.SetVideoThumbnailGeneratorWithMode(genURL, "post")
}

// SetVideoThumbnailGeneratorWithMode is the explicit form. mode = "post"
// (multipart upload, nekonoverse/video-thumb compat) or "get" (URL-based
// Misskey TS videoThumbnailGenerator API). 不正値は "post" にフォール
// バック。
func (s *Service) SetVideoThumbnailGeneratorWithMode(genURL, mode string) {
	s.videoThumbGen = genURL
	switch mode {
	case "get":
		s.videoThumbMode = "get"
	default:
		s.videoThumbMode = "post"
	}
	s.videoThumbClient = newVideoThumbnailClient(genURL)
}

// storableURL reports whether rawURL can be compared against text columns at
// all (#3036).
//
// PostgreSQL は NUL を含む値も不正な UTF-8 も**比較の右辺に置くだけで**
// クエリごと落とす。どちらも列に入りえないので、一致しえないことは引く前に
// 分かる。#3025 が cursor / id / 検索語に対して置いた guard と同じ判断。
//
// **判定そのものは `colfit.Storable` が持つ。** 以前はここだけが
// `utf8.ValidString` を併記していて、同じ guard を通る他の 118 箇所は不正な
// UTF-8 を素通りさせていた。数え方を 2 箇所に置くとまた分かれるので、ここは
// 「この経路で何を意味するか」だけを持つ。
func storableURL(rawURL string) bool {
	return colfit.Storable(rawURL)
}

// SignURL generates an HMAC-SHA256 signature for the given URL.
func (s *Service) SignURL(rawURL string) string {
	return SignURL(s.hmacSecret, rawURL)
}

// Authorize checks whether the given URL is permitted for proxying.
// HMAC署名があれば先に検証し、なければDBのallowlistを参照する。
func (s *Service) Authorize(ctx context.Context, rawURL, sig string) error {
	if sig != "" && VerifyHMAC(s.hmacSecret, rawURL, sig) {
		return nil
	}
	// **期限付きの署名 (#3037)。** `/url` のように**利用者が渡した URL**に
	// 対して署名を出す経路は、無期限の署名を配ると allowlist を恒久的に
	// 迂回する手段になる。そちらは `<unix 秒>.<hex>` の形で発行する。
	if sig != "" && VerifyExpiringHMAC(s.hmacSecret, rawURL, sig, time.Now()) {
		return nil
	}

	// **列に入りえない値は引く前に弾く (#3036、doctrine は #3025)。**
	// `IsAllowedURL` は `?url` を無検査で 11 箇所に bind する (4 テーブルの UNION) ので、
	// PostgreSQL が受け付けないバイト列を渡すとクエリごと落ちる。
	// それを `ErrAllowlistUnavailable` に流すと、**未認証の利用者が
	// 503 とエラーログを任意に生成できる** — この変更が守ろうとしている
	// 「監視で本物の障害が埋もれない」を自分で壊す形になる。
	//
	// 実測 (実 PostgreSQL): 不正 UTF-8 と孤立サロゲートは
	// `invalid byte sequence for encoding "UTF8"` (SQLSTATE 22021)。NUL は
	// **プロトコルで値が違う** — 本番の extended protocol では同じ 22021、
	// テストハーネスの simple protocol では `invalid message format` (08P01)。
	// **`colfit.Storable` だけでは足りない** — あれは NUL しか見ないので
	// 0xff と孤立サロゲートが素通りする。
	//
	// 返すのは `ErrUnauthorized`。列に入りえない値は allowlist のどの列にも
	// 一致しえないので「許可されていない」が事実で、**クエリを投げていない
	// 以上そこに隠れる障害も無い** (#2792 に反しない)。
	if !storableURL(rawURL) {
		return ErrUnauthorized
	}

	allowed, err := s.allowlist.IsAllowedURL(ctx, rawURL)
	if err != nil {
		// **利用者の離脱は障害ではない。** Error に混ぜると DB 障害の
		// 指標が汚れる (handler は 499 に分類して何も書かない)。
		//
		// **障害中の流量に注意。** handler が `no-store` を返すので、
		// クライアントは閲覧のたびに引き直す = 1 リクエスト 1 行出る。
		// `/proxy/*` は `api` グループの外なので Redis のレートリミッタも
		// 掛からない。#3032 の shed ログ (Warn) と違い Error なので、
		// 障害が長引く構成ではサンプリングを検討すること。
		if !errors.Is(err, context.Canceled) {
			slog.Error("mediaproxy: allowlist lookup failed", "url", rawURL, "err", err)
		}
		// **`ErrUnauthorized` に潰さない (#3036)。** 「許可されていない」と
		// 「判定できなかった」は別で、後者を 403 + 1 日キャッシュで返すと
		// DB の瞬断が全 URL を 1 日壊す。**`%w` で元の原因を残す** —
		// 利用者の離脱 (`context.Canceled`) と DB 障害を handler が
		// 区別できるようにするため。
		return fmt.Errorf("%w: %w", ErrAllowlistUnavailable, err)
	}
	if !allowed {
		return ErrUnauthorized
	}
	return nil
}

// Fetch downloads the remote URL (or resolves a local file), applies image
// processing per the requested mode, and returns the result. out selects the
// encoder format (FormatWebP / FormatAVIF) for resize-class modes.
// Fetch retrieves and processes a remote image.
//
// **animated=false はアニメーションを静止画にする (#2905)。** mode と直交する —
// upstream は `?emoji=1&static=1` を「emoji のサイズで、ただし静止画」として扱う
// (`FileServerProxyHandler.ts` の `animated: !('static' in query)`)。mode を
// ModeStatic に倒すとリサイズ寸法まで変わってしまうので、別の軸で持つ。
func (s *Service) Fetch(ctx context.Context, rawURL string, mode ProxyMode, out OutputFormat, animated bool) (*ProxyResult, error) {
	// ローカルファイルの場合はdriveStorageから直接取得
	filesPrefix := s.instanceURL + "/files/"
	if strings.HasPrefix(rawURL, filesPrefix) {
		return s.resolveLocal(ctx, rawURL, filesPrefix, mode, out, animated)
	}

	return s.fetchRemote(ctx, rawURL, mode, out, animated)
}

// resolveLocal fetches a file from local drive storage by access key.
func (s *Service) resolveLocal(ctx context.Context, rawURL, filesPrefix string, mode ProxyMode, out OutputFormat, animated bool) (*ProxyResult, error) {
	primaryKey := strings.TrimPrefix(rawURL, filesPrefix)
	// パスに/が含まれる場合は先頭のセグメントだけを使う
	if idx := strings.Index(primaryKey, "/"); idx >= 0 {
		primaryKey = primaryKey[:idx]
	}
	if primaryKey == "" {
		return nil, ErrBadRequest
	}

	accessKey := primaryKey
	storedMIME := ""

	// 既に存在する thumbnail / webpublic variant を提供できる mode なら、
	// 元データを再 decode + resize せずに variant を直接返して CPU を節約
	// する (#637 M1)。SetDriveLookup されていない / resize しないモード /
	// 該当 variant が無い場合は従来通り元データを取得して proxy 側で
	// resize する。default mode で DB を引かないようにする (#637 review
	// UR-014)。
	if s.driveLookup != nil && isResizeMode(mode) {
		if swapped, mt, ok := s.swapToVariant(primaryKey, mode); ok {
			accessKey = swapped
			storedMIME = mt
		} else {
			// swap しないが lookup は成功している場合、stored MIME を後段
			// MIME 判定で活用する (HEIC/AVIF/JXL の DetectContentType
			// 誤判定対策、#637 review UR-017)。
			if v, err := s.driveLookup.FindByAccessKey(primaryKey); err == nil {
				storedMIME = v.MimeType
			}
		}
	}

	body, err := s.driveStorage.Get(accessKey)
	// Variant blob が S3 lifecycle 等で消えた場合、primary を再フェッチして
	// resize 経路に戻す (#637 review UR-001)。primary 本体が存在しない /
	// その他 storage error はそのまま伝える。
	if errors.Is(err, coredrive.ErrObjectNotFound) && accessKey != primaryKey {
		body, err = s.driveStorage.Get(primaryKey)
		if err == nil {
			// ここから先は primary を配るので variant の accessKey はもう要らない。
			storedMIME = "" // primary の MIME は後段で判定し直す
		}
	}
	// オブジェクトストレージ有効化より前 (あるいは TS 時代) に保存された
	// `storedInternal=true` な行はローカル FS に実体がある。それらの url は
	// `<instanceURL>/files/<key>` のままなので、有効化した瞬間にこの経路へ来て
	// object storage を空振りし、タイムラインの既存画像が全部壊れる (#2315)。
	// `/files/:accessKey` は #1414 で同じ fallback を持っているが、media proxy
	// 側が漏れていた。
	//
	// drive_file を引いて storedInternal を見る手もあるが、default mode で DB を
	// 引かない設計 (#637 review UR-014) を崩したくないので、object-not-found の
	// ときだけローカルを見に行く。ホットパスには何も足さない。
	//
	// **種別を問わず倒す (#2990)。** この fallback が守っている行 (移行前 /
	// TS 時代に保存された `storedInternal = true`) の実体は object storage に
	// 置かれていないので、あちら側の失敗が not-found か障害かは判断の材料に
	// ならない。`S3Storage.Get` が全エラーを not-found に潰していた
	// 頃はどちらでも倒れていたが、種別を分けた結果**鍵の期限切れや 503 で
	// ローカルを見に行かなくなり、移行前の画像が全部 500 になる**。
	// ローカルにも無ければ下の分岐が元のエラーをそのまま伝える。
	if err != nil && s.localStorage != nil && !coredrive.StorageIsLocal(s.driveStorage) {
		if b, lerr := s.localStorage.Get(primaryKey); lerr == nil {
			body, err = b, nil
			storedMIME = ""
		}
	}
	if err != nil {
		if errors.Is(err, coredrive.ErrObjectNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("mediaproxy: local file access: %w", err)
	}

	// ローカルファイルの場合はMIME判定して画像処理
	data, readErr := io.ReadAll(io.LimitReader(body, maxDownload+1))
	body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("mediaproxy: read local file: %w", readErr)
	}
	if int64(len(data)) > maxDownload {
		return nil, ErrTooLarge
	}

	// stored MIME が分かっていればそちらを優先 (HEIC/AVIF/JXL は
	// http.DetectContentType が認識せず octet-stream を返す)。fetchRemote
	// と同じ挙動に揃える。
	contentType := storedMIME
	if isUnknownBinary(contentType) {
		contentType = http.DetectContentType(data)
	}
	return s.processAndReturn(ctx, data, contentType, mode, out, rawURL, animated)
}

// swapToVariant looks up the DriveFile by access key and returns the
// thumbnail / webpublic access key when one matches the requested mode,
// along with the variant's expected MIME (which `http.DetectContentType`
// can sniff but stored MIME is more reliable). Returns (key, mime, true)
// on swap, ("", "", false) otherwise.
func (s *Service) swapToVariant(accessKey string, mode ProxyMode) (string, string, bool) {
	v, err := s.driveLookup.FindByAccessKey(accessKey)
	if err != nil {
		return "", "", false
	}
	// 既に primary 以外 (= 既に variant の access key) を要求されているなら
	// 二重に swap しない。
	if v.AccessKey == nil || *v.AccessKey != accessKey {
		return "", "", false
	}
	// Variant の MIME は driveFile 上に直接持たないので空文字を返し、
	// 呼び出し側で `http.DetectContentType` フォールバックさせる。stored
	// thumbnail/webpublic は通常 image/jpeg or image/webp なので sniffer
	// で正しく判別できる。
	switch mode {
	case ModeEmoji, ModeAvatar, ModePreview, ModeBadge:
		// 小サイズ系: thumbnail があれば優先、無ければ webpublic を試す。
		if v.ThumbnailAccessKey != nil && *v.ThumbnailAccessKey != "" {
			return *v.ThumbnailAccessKey, "", true
		}
		if v.WebpublicAccessKey != nil && *v.WebpublicAccessKey != "" {
			return *v.WebpublicAccessKey, "", true
		}
	case ModeStatic:
		// 498x422 用 mid-size mode。thumbnail (典型的に小さすぎる) には
		// fallback しない: upscale 表示や ratio 崩れを防ぐ (#637 review
		// 自 #2)。webpublic が無ければ swap せず原本を resize する。
		if v.WebpublicAccessKey != nil && *v.WebpublicAccessKey != "" {
			return *v.WebpublicAccessKey, "", true
		}
	}
	return "", "", false
}

// fetchRemote downloads a file from a remote URL.
func (s *Service) fetchRemote(ctx context.Context, rawURL string, mode ProxyMode, out OutputFormat, animated bool) (*ProxyResult, error) {
	// **取りに行けない URL は「リモートの障害」ではない (#3034)。**
	// 相対 URL (`/identicon/<id>`) や `data:` はここまで来るが、そもそも
	// `httpClient.Do` には渡せない。それを 502 + 短期キャッシュにすると、
	// (a) 監視で「相手インスタンスが落ちている」と読め、(b) 恒久的に直らない
	// ものを 5 分ごとに引き直し続ける。
	//
	// **`http.NewRequestWithContext` より前に置く。** あちらは内部で同じ
	// `url.Parse` を先に通すので、後ろに置くと parse 失敗が
	// `mediaproxy: create request` で返ってこの判定に届かず、制御文字入りの
	// URL のような「恒久的に取りに行けない」ものが generic 500 に落ちる。
	if u, perr := url.Parse(rawURL); perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, ErrBadRequest
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("mediaproxy: create request: %w", err)
	}
	req.Header.Set("User-Agent", s.userAgent)
	req.Header.Set("Accept", "image/*,*/*;q=0.5")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// **SSRF ガードの拒否は「相手の障害」ではない (#3037)。**
		// こちらが意図的に遮断した事実なので `ErrUpstreamUnavailable` に
		// 混ぜない — 502 だと監視で「相手が落ちている」と読める。
		// **キャッシュ時間は変えない** (恒久的ではない。理由は
		// `ErrTargetBlocked` の GoDoc)。
		//
		// **`%w` で元の原因を残す** — ログに遮断した URL だけでなく
		// safehttp 側のメッセージも出したいため。
		if errors.Is(err, safehttp.ErrSSRFBlocked) {
			return nil, fmt.Errorf("%w: %w", ErrTargetBlocked, err)
		}
		// **種別を問わず `ErrNotFound` に潰してはいけない (#3034)。**
		// handler はそれを 404 + `Cache-Control: max-age=86400` で返すので、
		// DNS 失敗・接続拒否・TLS エラー・タイムアウトといったリモート側の
		// 一時障害が、CDN に「この画像は存在しない」として 1 日焼き付く。
		// #2913 (403 が 1 日キャッシュされてアイコンが 1 日壊れた) と同型で、
		// #2792 の「lookup error を種別を見ずに 4xx へ潰さない」にも反する。
		//
		// **`%w` を 2 つ使うのが要点。** `http.Client` は cancel / timeout を
		// `*url.Error` で包んで返すので、こう書くと
		// `errors.Is(err, context.Canceled)` が生き残り、handler が利用者の
		// 離脱を 499 に振り分けられる (潰していた頃は 404 になっていた)。
		return nil, fmt.Errorf("%w: %w", ErrUpstreamUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mediaproxy: remote returned %d", resp.StatusCode)
	}

	// Content-Lengthが明示されていてmaxDownloadを超える場合は即拒否
	if resp.ContentLength > maxDownload {
		return nil, ErrTooLarge
	}

	// maxDownload+1バイト読み、実際にmaxDownloadを超えたらエラー
	// Content-Lengthが嘘や未設定の場合でもサイレント切り捨てを防ぐ
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		// 途中で切れた転送も上と同じ「取れなかった」なので 502 側へ倒す
		// (#3034)。`%w` で包むので `context.Canceled` / `DeadlineExceeded`
		// の判定は従来どおり生きる。
		return nil, fmt.Errorf("%w: read remote: %w", ErrUpstreamUnavailable, err)
	}
	if int64(len(data)) > maxDownload {
		return nil, ErrTooLarge
	}

	// Content-Type ヘッダーには `; charset=utf-8` 等のパラメータが付くことが
	// あるので、media type だけを切り出してから allowlist と比較する
	// (#418 Devin review)。`mime.ParseMediaType` 失敗時は元のヘッダ値を
	// そのまま使い、後段の DetectContentType フォールバックに任せる。
	rawCT := resp.Header.Get("Content-Type")
	contentType := rawCT
	if mt, _, err := mime.ParseMediaType(rawCT); err == nil {
		contentType = mt
	}
	if isUnknownBinary(contentType) {
		contentType = http.DetectContentType(data)
	}

	return s.processAndReturn(ctx, data, contentType, mode, out, rawURL, animated)
}

// OutputFormat selects the encoder used by resize-class processing modes.
type OutputFormat int

const (
	// FormatWebP is the default browser-safe encoder.
	FormatWebP OutputFormat = iota
	// FormatAVIF requests AVIF output. caller-side preconditions: client
	// either set ?avif=1 or sent `Accept: image/avif,...`. AVIF は CPU
	// 重めなので明示要求時のみ使う (Misskey TS と同 semantics)。
	FormatAVIF
)

// processAndReturn applies image processing per mode and returns the result
// in the requested output format (WebP / AVIF). Badge と passThrough は
// format negotiation の対象外で常に PNG / 元 MIME を返す。
//
// video/* MIME は image pipeline に直接乗らないので、download 済の bytes
// をそのまま videoThumbnailGenerator に POST multipart で投げて静止画
// thumbnail を取得し、再帰的に image pipeline に通す。generator 未設定 /
// 呼び出し失敗のときは dummy PNG にフォールバックして frontend を壊さない
// (#637 M2)。bytes 経由なので local /files/ source も remote URL も同じ
// path で扱える。
func (s *Service) processAndReturn(ctx context.Context, data []byte, contentType string, mode ProxyMode, out OutputFormat, sourceURL string, animated bool) (*ProxyResult, error) {
	if isVideoMIME(contentType) && isResizeMode(mode) {
		if s.videoThumbClient == nil {
			// **generator が配線されていないケースは既定 (長期) のまま。**
			// 設定を見れば分かる事実で、generator の応答を分類できない
			// ケースとは性質が違う。しかもここは既定構成の経路で、
			// `mediaurl.go` が thumbnail 無しリモート動画を
			// `/proxy/static.webp?url=<動画本体>` に回すため、キャッシュが
			// 外れるたびに動画を最大 `maxDownload` (32MiB) 取り直す。
			// mediaproxy に結果キャッシュは無く handler も `ETag` を出さない
			// ので、再検証は必ずフルミスになる (#3035 レビュー 3 周目で実測)。
			return makeDummyPNG(), nil
		}
		// GET mode は generator が sourceURL を fetch する前提なので、
		// local /files/<key> を投げても (UDS-only stack 等の) generator
		// から到達できないことが多い。早期 fallback で無駄な RT を省く。
		// POST mode では bytes 直送なので skip 不要。
		if s.videoThumbMode == "get" && strings.HasPrefix(sourceURL, s.instanceURL+"/files/") {
			// 同上 — 設定 (`videoThumbnailGeneratorMode`) だけで決まる。
			return makeDummyPNG(), nil
		}
		frame, frameMIME, err := s.fetchVideoThumbnail(ctx, data, contentType, sourceURL)
		if err != nil {
			slog.Warn("mediaproxy: video thumbnail generator failed",
				"url", sourceURL, "err", err)
			// **どちらも `immutable` にはしない (#3035)。** 生成失敗の
			// ダミーを「絶対に変わらない」と宣言すると、原因が直っても
			// リロードで戻せなくなる。
			if errors.Is(err, ErrVideoThumbnailTransient) {
				return dummyPNGWithCache(transientDummyCacheControl), nil
			}
			return dummyPNGWithCache(permanentDummyCacheControl), nil
		}
		data = frame
		contentType = frameMIME
	}
	// アニメ形式 (GIF / APNG) は image.Decode が 1 frame しか返さないため
	// resize 経路に乗せると静止画化される (#941)。emoji / avatar / preview の
	// ようにアニメ表示が期待される mode では pass-through で保持する。
	// static / badge は明示的に静止画を要求しているので従来通り decode する。
	//
	// **animated=false なら pass-through しない (#2905)。** 利用者の
	// 「アニメーション画像を再生しない」設定 (disableShowingAnimatedImages) が
	// `?emoji=1&static=1` として届くが、mode だけを見ていたので無視されていた。
	//
	// **WebP は MIME では判定できない (#3128)。** アニメーションも静止画も
	// `image/webp` なので、`isAnimatedFormat` (MIME 判定) では拾えず、
	// `processResize` → `decodeImage` (先頭 1 コマ) → `encodeWebP` (1 枚しか
	// 受けない) で静止画になっていた。コンテナを歩いて判定を足す。
	if shouldPassThroughAnimated(contentType, data, mode, animated) {
		return s.passThrough(data, contentType)
	}
	// #2106 N19: resize 系 mode (emoji/avatar/static/preview/badge) は変換可能な image を
	// 要求する。video 静止画化 + animated passThrough を経た後でも convertible でない MIME
	// (text/html, application/pdf 等) は upstream FileServerProxyHandler の "Unexpected mime"
	// と同じく 404 にする。生 bytes を宣言 content-type で配信すると passThrough 経路の
	// browsersafe allowlist を回避でき、CSP の無い /proxy 上で XSS 補助面になる。
	if isResizeMode(mode) && !isConvertibleImage(contentType) {
		return nil, ErrNotFound
	}
	// **ここから下だけが CPU 仕事 (#3032)。** 上のリモート fetch や video
	// thumbnail generator への HTTP 呼び出しは P を使わない待ちなので、
	// 枠の内側に入れるとスループットだけが落ちる。pass-through と dummy は
	// resize mode ではないので枠を取らない。
	if isResizeMode(mode) {
		release, err := s.acquireCPU(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	switch mode {
	case ModeEmoji:
		return s.processResize(data, contentType, 0, emojiHeight, out)
	case ModeAvatar:
		return s.processResize(data, contentType, 0, avatarHeight, out)
	case ModeStatic:
		return s.processResize(data, contentType, staticWidth, staticHeight, out)
	case ModePreview:
		return s.processResize(data, contentType, previewWidth, previewHeight, out)
	case ModeBadge:
		return s.processBadge(data, contentType)
	default:
		return s.passThrough(data, contentType)
	}
}

// isResizeMode reports whether the mode triggers image-pipeline processing
// (so a video source needs a still frame extracted first).
func isResizeMode(mode ProxyMode) bool {
	switch mode {
	case ModeEmoji, ModeAvatar, ModeStatic, ModePreview, ModeBadge:
		return true
	}
	return false
}

// processResize decodes the image, resizes it, and encodes to WebP or AVIF.
// width=0の場合はheightのみでアスペクト比を維持する。out=FormatAVIF の
// ときは AVIF で書き出し、エンコード失敗時は WebP に fallback する。
func (s *Service) processResize(data []byte, contentType string, width, height int, out OutputFormat) (*ProxyResult, error) {
	if !isConvertibleImage(contentType) {
		// 変換できない画像フォーマットはそのまま返す
		return makeResult(data, contentType), nil
	}

	img, err := decodeImage(data, contentType)
	if err != nil {
		// **pixel cap に当たったものを素通ししない (#3037)。** cap の判定を
		// `image.DecodeConfig` の段へ前倒しした結果、64MP 超はデコード
		// **エラー**になってこの枝へ落ちるようになった。素通しすると
		// 74 バイトのダミー PNG だったものが**最大 32MiB の原本**に変わり、
		// しかも成功応答なので `max-age=31536000, immutable` で CDN に焼かれる。
		// タイムラインに出るたび全閲覧者へ配られ、各ブラウザが 9 億画素を
		// デコードする — 塞ごうとした爆弾を増幅して配る形になる。
		// **入力の大きさで断ったものも同じ扱い。** 素通しすると、デコーダに
		// 渡すには大きすぎると判断したバイト列をそのまま閲覧者へ配ることに
		// なる (判断の意味が無い)。
		//
		// **`immutable` にはしない (#3037 レビュー 2 周目)。** 素の
		// `makeDummyPNG` は `CacheControl` が空なので handler の既定
		// (`max-age=31536000, immutable`) が付く。上限は運営者が設定で
		// 変えられるし、こちらの見積もりが実測と合っていなくて落ちることも
		// ある — その場合に**直してもリロードで戻せなくなる**。
		// #3035 が動画サムネイルの生成失敗について同じ判断をしている。
		if errors.Is(err, imagedecode.ErrTooManyPixels) || errors.Is(err, imagedecode.ErrEncodedTooLarge) {
			return dummyPNGWithCache(permanentDummyCacheControl), nil
		}
		// デコード失敗時は元データをそのまま返す
		return makeResult(data, contentType), nil
	}
	if exceedsPixelCap(img) {
		// pixel-bomb (32MB AVIF/HEIC/JXL が 30000x30000 に展開される類) は
		// resize/encode で多 GB のバッファを抱えるので dummy PNG にして
		// proxy の OOM を防ぐ (#637 review UR-016)。
		return dummyPNGWithCache(permanentDummyCacheControl), nil
	}

	var resized image.Image
	if width == 0 {
		// height指定のみ: アスペクト比を維持して高さに合わせる
		resized = resizeToHeight(img, height)
	} else {
		resized = resizeFit(img, width, height)
	}

	if out == FormatAVIF {
		encoded, err := encodeAVIF(resized)
		if err == nil {
			return makeResult(encoded, "image/avif"), nil
		}
		// AVIF 失敗時は WebP に fallback。原因は libavif (wazero) 側の
		// memory limit / 入力色空間など多岐に渡るので observability の
		// ため warn level で残す (UR review nit #6)。
		slog.Warn("mediaproxy: avif encode failed, falling back to webp",
			"err", err)
	}
	encoded, err := encodeWebP(resized)
	if err != nil {
		return makeResult(data, contentType), nil
	}
	return makeResult(encoded, "image/webp"), nil
}

// exceedsPixelCap returns true when the decoded image's pixel count exceeds
// maxDecodedPixels. Used to refuse processing pixel-bomb inputs.
func exceedsPixelCap(img image.Image) bool {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return true
	}
	return int64(w)*int64(h) > maxDecodedPixels
}

// processBadge creates a 96x96 badge PNG, mirroring upstream
// FileServerProxyHandler.processBadge (#2920).
//
// **sharp は呼び出し順ではなく固定の pipeline 順で処理する。** JS の記述は
//
//	resize(...).greyscale().normalise().linear(1.75, -(128*1.75)+128)
//	  .flatten({background: '#000'}).toColorspace('b-w')
//
// だが、`sharp/src/pipeline.cc` の実行順は **flatten → greyscale → resize/embed →
// linear → normalise**。記述順どおりに normalise → linear → flatten と書くと、
// 輝度レンジの狭い絵文字で平均絶対差 28.7/255・43.8% の画素が 32 以上ずれる
// (実測)。**呼び出し順を入れ替えても sharp の出力が 1 バイトも変わらない**ことで
// 固定順であることを確認してある。
//
// **contain であって cover ではない。** 以前は imaging.Fill (cover + 中央クロップ)
// だったので、200x120 の絵文字は横幅の 40% が失われていた。
//
// **最後の XOR は no-op ではない。** 透明な RGBA canvas (全バイト 0) と mask を
// XOR すると R=G=B=A=mask になる。つまり**暗いところが透明**な silhouette に
// なる。通知 UI の背景に乗せる形なのでこれが要る。
func (s *Service) processBadge(data []byte, contentType string) (*ProxyResult, error) {
	// upstream の `requiresImageConversion && !isConvertibleImage` → 404
	// (`Unexpected mime`)。**本番経路では Fetch 側の
	// `isResizeMode(mode) && !isConvertibleImage` が先に同じ 404 を返す**ので
	// ここは到達しないが、processBadge 単体の契約として揃えておく。
	if !isConvertibleImage(contentType) {
		return nil, ErrNotFound
	}

	img, err := decodeImage(data, contentType)
	if err != nil {
		// cap に当たったものは従来どおりダミーへ (`processResize` と同じ理由。
		// `immutable` にしないのも同じ)。
		if errors.Is(err, imagedecode.ErrTooManyPixels) || errors.Is(err, imagedecode.ErrEncodedTooLarge) {
			return dummyPNGWithCache(permanentDummyCacheControl), nil
		}
		return nil, ErrNotFound
	}
	if exceedsPixelCap(img) {
		return dummyPNGWithCache(permanentDummyCacheControl), nil
	}

	// **entropy は元画像で測る。** upstream の `stats()` は
	// `sharp/src/stats.cc` で入力を開き直すので、**pipeline の操作を一切
	// 適用しない**。`linear(0, 100)` で定数化しても `threshold(128)` で 2 値化
	// しても値が変わらないことで確認してある (どれも log2(値の種類) に一致)。
	// つまり判定は「元画像の greyscale が実質単色か」。
	// **Clone は 1 回だけ。** 入力を NRGBA に正規化して Pix を直読みする。
	// **`normalizeForResize` を先に通す** — `imaging.Clone` は `*image.NYCbCrA`
	// の alpha を行単位で壊す (#2925)。
	src := imaging.Clone(normalizeForResize(img))
	if inputEntropy(src) < badgeMinEntropy {
		// upstream の `StatusError('Skip to provide badge', 404)`。SW の
		// create-notification.ts は `res.status !== 200` で iconUrl('plus') へ
		// 落ちるので、判別できないバッジを配るより 404 の方が親切。
		return nil, ErrNotFound
	}

	mask := badgeMask(src)
	out := image.NewNRGBA(image.Rect(0, 0, badgeSize, badgeSize))
	for i, v := range mask {
		out.Pix[i*4] = v
		out.Pix[i*4+1] = v
		out.Pix[i*4+2] = v
		out.Pix[i*4+3] = v
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, ErrNotFound
	}
	return makeResult(buf.Bytes(), "image/png"), nil
}

// srgbToLinear / linearToSRGB convert one 0-255 sRGB channel to and from linear
// light. vips の `colourspace(B_W)` は線形光を経由するので、係数を sRGB 値へ
// 直接掛けると彩度の高い色で大きくずれる (実測: 純赤は vips 127 に対し 54)。
var srgbToLinearLUT = func() [256]float64 {
	var t [256]float64
	for i := range t {
		c := float64(i) / 255
		if c <= 0.04045 {
			t[i] = c / 12.92
		} else {
			t[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return t
}()

// linearToSRGBLUT maps linear luminance (0-1, 16bit 量子化) to the 0-255 sRGB
// value. **画素ごとに math.Pow を呼ぶと重い** — 4000x4000 の入力で 2 秒かかって
// いたところの大半がこれだった。丸め誤差は最終的な 0-255 への丸めに吸収される。
var linearToSRGBLUT = func() [65536]uint8 {
	var t [65536]uint8
	for i := range t {
		v := float64(i) / 65535
		var c float64
		if v <= 0.0031308 {
			c = v * 12.92
		} else {
			c = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		t[i] = uint8(math.Round(math.Min(255, math.Max(0, c*255))))
	}
	return t
}()

// greyLevel returns the vips-compatible greyscale value of one sRGB pixel.
func greyLevel(r, g, b uint8) uint8 {
	y := 0.2126*srgbToLinearLUT[r] + 0.7152*srgbToLinearLUT[g] + 0.0722*srgbToLinearLUT[b]
	if y <= 0 {
		return 0
	}
	if y >= 1 {
		return 255
	}
	return linearToSRGBLUT[int(y*65535+0.5)]
}

// inputEntropy returns the Shannon entropy (bits) of the source image's
// greyscale histogram, which is what upstream's `stats().entropy` measures.
// **alpha は見ない** — upstream も RGB の greyscale だけを測る (RGB 一様で alpha
// だけ変化する画像の entropy が 0 になることで確認済み)。
func inputEntropy(src *image.NRGBA) float64 {
	// **`img.At()` で舐めない。** 画素ごとに interface boxing が起きるので、
	// 4000x4000 の入力で 32M アロケーション・2.7 秒になる (実測)。
	// `imaging.Clone` で一度 NRGBA にしてから Pix を直読みする。
	//
	// **非乗算で読むのが要点。** `At().RGBA()` は premultiplied を返すので、
	// そのままだと「alpha を見ない」つもりが透明部分だけ 0 に潰れる。
	var hist [256]int
	n := len(src.Pix) / 4
	for i := 0; i < n; i++ {
		hist[greyLevel(src.Pix[i*4], src.Pix[i*4+1], src.Pix[i*4+2])]++
	}
	return histEntropy(hist[:], n)
}

func histEntropy(hist []int, n int) float64 {
	if n == 0 {
		return 0
	}
	total := float64(n)
	entropy := 0.0
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		entropy -= p * math.Log2(p)
	}
	return entropy
}

func clampByte(v float64) uint8 {
	switch {
	case v <= 0:
		return 0
	case v >= 255:
		return 255
	default:
		return uint8(math.Round(v))
	}
}

// badgeMask runs the upstream badge pipeline and returns the 96x96 mask as one
// byte per pixel (row-major). 順序は sharp の pipeline 順
// (flatten → greyscale → resize/embed → linear → normalise)。
func badgeMask(src *image.NRGBA) []byte {
	// 1. flatten({background: '#000'}) → 2. greyscale
	//    **resize より前**なので、透明部分は先に黒へ落ちる。
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	flat := image.NewGray(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		// flatten({background: '#000'}) は RGB×alpha。NRGBA は非乗算なので
		// ここで掛ける (`At().RGBA()` の premultiplied 値を使うと、透明部の
		// RGB が読めない decoder で潰れる)。
		a := uint32(src.Pix[i*4+3])
		flat.Pix[i] = greyLevel(
			uint8(uint32(src.Pix[i*4])*a/255),
			uint8(uint32(src.Pix[i*4+1])*a/255),
			uint8(uint32(src.Pix[i*4+2])*a/255))
	}

	// 3. resize(96, 96, {fit: 'contain', withoutEnlargement: false}) + embed(黒)
	//    **拡大もする**。imaging.Fit は縮小しかしないので倍率を自分で出す。
	scale := math.Min(float64(badgeSize)/float64(w), float64(badgeSize)/float64(h))
	nw := max(1, int(math.Round(float64(w)*scale)))
	nh := max(1, int(math.Round(float64(h)*scale)))
	fitted := imaging.Resize(flat, nw, nh, imaging.Lanczos)
	canvas := imaging.New(badgeSize, badgeSize, color.NRGBA{A: 255})
	// **切り捨てで左上寄せ** — sharp の CalculateEmbedPosition と同じ。
	// imaging.PasteCenter は奇数差で右下へ寄るので 1px ずれる。
	canvas = imaging.Paste(canvas, fitted, image.Pt((badgeSize-nw)/2, (badgeSize-nh)/2))

	n := badgeSize * badgeSize
	vals := make([]float64, n)
	for i := 0; i < n; i++ {
		// 4. linear(1.75, -(128*1.75)+128)。sharp は uchar:true なので
		//    この時点で 0-255 にクリップされる。
		v := float64(canvas.Pix[i*4])
		v = math.Round(1.75*v - 96)
		v = math.Min(255, math.Max(0, v))
		vals[i] = v
	}

	// 5. normalise。**min-max ではなく 1/99 パーセンタイル。**
	//    sharp の既定は `{lower: 1, upper: 99}` (実測) で、`operations.cc` は
	//    `luminance.percent(lower)` / `percent(upper)` を使う。min-max で
	//    実装すると、暗部・明部の裾が薄い実写系の画像で upstream との
	//    平均絶対差が 22.7/255・最大 40 になる (upstream 自身のテスト画像
	//    `test/resources/192.png` で実測)。
	return normaliseToBytes(vals)
}

// normaliseToBytes applies sharp's `normalise()` to the 0-255 values.
//
// **min-max ではなく 1/99 パーセンタイル。** sharp の既定は
// `{lower: 1, upper: 99}` (実測) で、`operations.cc` は `luminance.percent(lower)`
// / `percent(upper)` を使う。min-max で実装すると、暗部・明部の裾が薄い実写系の
// 画像で upstream との平均絶対差が 23.9/255・最大 42・32 を超える画素 16.7% に
// なる (upstream 自身のテスト画像 `test/resources/192.png` で実測)。
func normaliseToBytes(vals []float64) []byte {
	out := make([]byte, len(vals))
	lo, hi := percentileRange(vals, badgeNormaliseLower, badgeNormaliseUpper)
	// upstream の `std::abs(max - min) > 1`。**単位が違う** — あちらは LAB の L
	// (0-100) なので、0-255 では 2.55 に相当する。ここを跨ぐのは実質単色の
	// 画像だけで、それらは元画像の entropy guard で先に 404 になる。
	span := hi - lo
	for i, v := range vals {
		if span > 1 {
			v = (v - lo) * 255 / span
		}
		out[i] = clampByte(v)
	}
	return out
}

// percentileRange returns the values at the lower/upper percentiles of the
// 0-255 histogram of vals, mirroring vips' `percent()`.
func percentileRange(vals []float64, lower, upper float64) (float64, float64) {
	var hist [256]int
	for _, v := range vals {
		hist[clampByte(v)]++
	}
	n := len(vals)
	at := func(pct float64) float64 {
		want := pct / 100 * float64(n)
		cum := 0
		for v, c := range hist {
			cum += c
			if float64(cum) >= want {
				return float64(v)
			}
		}
		return 255
	}
	return at(lower), at(upper)
}

// passThrough validates the MIME type and returns data as-is.
func (s *Service) passThrough(data []byte, contentType string) (*ProxyResult, error) {
	// image/svg+xmlはセキュリティ上そのまま返さない（XSSリスク）
	if contentType == "image/svg+xml" {
		return s.svgFallback(data)
	}

	if !browsersafeMIMEs[contentType] {
		return nil, fmt.Errorf("mediaproxy: rejected MIME type: %s", contentType)
	}

	return makeResult(data, contentType), nil
}

// svgFallback converts SVG to a 1x1 transparent PNG placeholder.
// SVGラスタライズはv2で実装予定。
func (s *Service) svgFallback(_ []byte) (*ProxyResult, error) {
	return makeDummyPNG(), nil
}

// DummyPNG returns a 1x1 transparent PNG for fallback responses.
func DummyPNG() *ProxyResult {
	return makeDummyPNG()
}

// 動画サムネイル生成に失敗してダミー画像へ倒れたときの `Cache-Control`
// (#3035)。**成功応答の既定 (`max-age=31536000, immutable`) は使わない。**
//
// `immutable` は「この URL の中身は絶対に変わらない」という宣言で、
// **ブラウザはリロードでも再検証しない**。生成失敗のダミーにそれを張ると、
// 原因が直っても戻す手段が無くなる (URL を変えるしかない)。
//
// 一時障害 (generator の再起動 / 輻輳) は 5 分。**`no-store` にはしない** —
// generator がハングしている間、動画 1 本ごとに毎回
// `videoThumbnailTimeout` (30 秒) を踏みに行くことになる。
//
// **generator が応答したのに使えなかった**ときは 1 日。**mk-go にはその
// 4xx が「動画のせい」か「設定のせい」かを判定する材料が無い** —
// `videoThumbnailGenerator` の URL を打ち間違えれば generator 本体が 404 を
// 返すし、前段に認証や WAF を置けば 401 / 403 が来る。どれも operator が
// 直せば解消するので 1 年固定してはいけない。1 日は #3034 の恒久側
// (取りに行けない URL / リモートの 404) と同じ値。
//
// **generator が配線されていない**ケース (未設定 / GET モードでローカル
// `/files/` を skip) はここに含めない。設定を見れば分かる事実で、しかも
// 既定構成の経路なので、短くするとリモート動画 1 本あたり最大 32MiB の
// 取り直しが増える (`processAndReturn` のコメント参照)。後から generator を
// 設定したときに古いダミーが残るのは承知のうえの取引。
//
// **pixel cap のダミーもこちらを使う (#3037 レビュー 3 周目で訂正)。**
// 以前ここには「pixel cap は入力バイト列とデプロイだけで決まるので既定の
// ままでよい」と書いてあったが、`immutable` で 1 年固定すると**デプロイで
// 直してもリロードで戻せない** — 見積もり (1 画素あたりのバイト数) が実測と
// 合わずに落ちる形があるので、そちらの理由だけで十分。上限そのものは
// `const` / 代入の無い `var` で、運営者は変えられない。
// **`svgFallback` は今も素の `makeDummyPNG()`。**
//
// `internal/api/proxy` にも同じリテラルがあるが、パッケージを跨いで定数を
// 共有はしない — レイヤが違い、それぞれ独立して動かせるほうがよい。
const (
	transientDummyCacheControl = "max-age=300"
	permanentDummyCacheControl = "max-age=86400"
)

// dummyPNGWithCache returns the fallback image with an explicit cache policy.
func dummyPNGWithCache(cacheControl string) *ProxyResult {
	res := makeDummyPNG()
	res.CacheControl = cacheControl
	return res
}

func makeDummyPNG() *ProxyResult {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.Set(0, 0, color.Transparent)
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return makeResult(buf.Bytes(), "image/png")
}

func makeResult(data []byte, contentType string) *ProxyResult {
	return &ProxyResult{
		Body:        io.NopCloser(bytes.NewReader(data)),
		ContentType: contentType,
	}
}

// isUnknownBinary reports whether a Content-Type carries no type information,
// so the bytes themselves must be sniffed instead.
//
// **`application/octet-stream` だけを見てはいけない。** 標準はそちらだが、
// S3 互換ストレージの一部は `binary/octet-stream` を返す。どちらも「型が
// 分からない」以上の意味を持たないので、中身から判定し直す対象は同じ。
//
// 実際に、この 2 つを区別していたせいで `binary/octet-stream` を返す
// インスタンスの画像が resize 経路で `isConvertibleImage` に弾かれ、
// アイコン・バナー・カスタム絵文字がまとめて 404 になっていた (#2541)。
func isUnknownBinary(contentType string) bool {
	switch contentType {
	case "", "application/octet-stream", "binary/octet-stream":
		return true
	}
	return false
}

// shouldPassThroughAnimated reports whether the response must be returned
// untouched to keep its animation.
//
// **判定を関数にしてある。** 呼び出し側に条件を直書きすると、`Fetch` を通した
// テストでしか固定できず、そこはデコードまで走るので「素通しするか」だけを
// 見ることができない (壊れた WebP は decoder が panic する)。
func shouldPassThroughAnimated(contentType string, data []byte, mode ProxyMode, animated bool) bool {
	// 利用者の「アニメーション画像を再生しない」設定 (#2905) が来ているなら
	// 静止化するのが正しいので素通ししない。
	if !animated {
		return false
	}
	switch mode {
	case ModeEmoji, ModeAvatar, ModePreview:
	default:
		// static / badge は明示的に静止画を要求している。
		return false
	}
	// **WebP は MIME では判定できない (#3128)。** アニメーションも静止画も
	// `image/webp` なので、`isAnimatedFormat` では拾えず resize 経路
	// (先頭 1 コマだけ decode → `encodeWebP` は 1 枚しか受けない) で
	// 静止画になっていた。コンテナを歩いて判定する。
	return isAnimatedFormat(contentType) || imagedecode.IsAnimatedWebP(data)
}

// isAnimatedFormat returns true for image MIME types that natively encode
// multi-frame animation (GIF, APNG variants)。
//
// Go の image.Decode は 1 frame しか取り出さないため、resize 経路に乗せると
// 静止画化されてしまう (#941)。emoji / avatar / preview のように「アニメ
// 表示が期待される」mode では pass-through すべき MIME 群。
func isAnimatedFormat(mime string) bool {
	switch mime {
	case "image/gif", "image/apng",
		"image/vnd.mozilla.apng":
		return true
	}
	return false
}

// isConvertibleImage returns true if the MIME type can be decoded and converted.
func isConvertibleImage(mime string) bool {
	switch mime {
	case "image/jpeg", "image/png", "image/gif",
		"image/webp", "image/bmp", "image/tiff",
		// IANA 公式名 (image/vnd.microsoft.icon) と古い慣例 (image/x-icon)
		// を両方許可する (#418)。
		"image/x-icon", "image/vnd.microsoft.icon",
		// APNG は 2 綴りある。drive 側の判定器が `image/apng` を返すように
		// なった (#2319) ので両方受ける。
		"image/vnd.mozilla.apng", "image/apng",
		// gen2brain wazero ベースの decoder で対応 (#637 M3/M4/M5):
		// image/avif (in/out), image/heic, image/heif, image/jxl は input 専用
		// として decode → WebP/AVIF 出力経路に乗せる。
		"image/avif", "image/heic", "image/heif", "image/jxl",
		// #672 Phase 1: pure Go decoder で input 対応。
		// Netpbm 系 (spakin/netpbm) と TGA (ftrvxmtrx/tga) は Go の標準
		// image.Decode に format register されるので decodeImage 経由で
		// 透過的に扱える。output は WebP/AVIF/PNG への transcode 経路に乗る。
		"image/x-portable-bitmap", "image/x-portable-graymap",
		"image/x-portable-pixmap", "image/x-portable-anymap",
		"image/x-tga", "image/x-targa",
		// #734: pure Go JPEG 2000 decoder (mrjoshuak/go-jpeg2000)。
		// JP2/J2K 両方 magic bytes 登録済みなので imaging.Decode 経由。
		// JPX (Part 2) は library 未対応のため除外 (browsersafe 側で
		// pass-through のみ受け入れる)。
		"image/jp2", "image/jpeg2000":
		return true
	default:
		return false
	}
}

func decodeImage(data []byte, contentType string) (image.Image, error) {
	// TGA は magic bytes が無いため image.RegisterFormat 経由の自動 dispatch
	// が他フォーマットを破壊する (ftrvxmtrx/tga の既知問題)。MIME type で
	// 明示判定して blezek/tga (auto-register 無し) の Decode を直接呼ぶ
	// (#672 Phase 1)。
	if contentType == "image/x-tga" || contentType == "image/x-targa" {
		// **cap 付きの入口を通す (#3037 の穴)。** `imagedecode.Decode` の
		// 「ヘッダを読めなかったら通す」枝では TGA を判定できないので、
		// `DecodeTGAWithPixelCap` がヘッダから寸法を直接読む。素で
		// `tga.Decode` を呼ぶと、18 バイトのヘッダが宣言した寸法どおりに
		// ラスタを確保してしまう (寸法の検証がライブラリ側に無い)。
		return imagedecode.DecodeTGAWithPixelCap(data, imagedecode.MaxPixels)
	}
	// **インターレースの truecolor PNG は decoder のバグを踏む (#2925)。**
	// 規則は `internal/misc/imagedecode` に 1 つだけ置いてある — drive 側の
	// image processor も同じ decode をしており、片方だけ直すともう片方に
	// 同じバグが残る (初版で実際にそうなっていた)。
	img, err := imagedecode.Decode(data)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// normalizeForResize converts img to NRGBA when the resize library cannot read
// its type correctly.
//
// **`*image.NYCbCrA` を imaging に直接渡すと全画素が 0 になる。** アルファ付きの
// 拡張 WebP (VP8X + ALPH + VP8) を Go の webp デコーダが返す型で、
// kovidgoyal/imaging v1.8.21 の scanner が 4:2:0 の NYCbCrA を読めていない。
// デコード自体は成功しているので、**200 で真っ白 (完全透明) の画像が返る**
// という気づきにくい壊れ方をする (リモート利用者のアイコンが出ない、#2591)。
//
// 単純な VP8 は `*image.YCbCr` になるので影響を受けない。ここで型を絞って
// 変換するのは、NRGBA 化が画素あたりのコピーを 1 回増やすため。
func normalizeForResize(img image.Image) image.Image {
	src, ok := img.(*image.NYCbCrA)
	if !ok {
		return img
	}
	// **`imaging.Clone` にも `draw.Draw` にも渡せない (#2925)。** どちらも
	// 片方を壊す:
	//
	//   - `draw.Draw` / `At()` は premultiplied な値しか出さないので、
	//     **完全に透明な画素の RGB が復元できず 0 に潰れる**
	//   - `imaging.Clone` は RGB を plane から正しく読むが、**サブサンプル
	//     (4:2:0 / 4:2:2 / 4:4:0 = lossy WebP の通常形) の分岐で alpha の
	//     index を内側ループで進めない**ため、行の全画素がその行の先頭画素の
	//     alpha になる (`nrgba/scanner.go`)。upstream のテスト画像
	//     `with-alpha.webp` で 22.5% の画素の alpha が誤り、badge を sharp と
	//     比べると 32 を超える差が 16.6% の画素に出る
	//
	// Y / Cb / Cr / A の plane を自分で読めば両方とも正しく取れる。
	b := src.Bounds()
	dst := image.NewNRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl := color.YCbCrToRGB(
				src.Y[src.YOffset(x, y)],
				src.Cb[src.COffset(x, y)],
				src.Cr[src.COffset(x, y)])
			i := dst.PixOffset(x, y)
			dst.Pix[i] = r
			dst.Pix[i+1] = g
			dst.Pix[i+2] = bl
			dst.Pix[i+3] = src.A[src.AOffset(x, y)]
		}
	}
	return dst
}

// resizeToHeight resizes image to the specified height while preserving aspect
// ratio. 元画像がheight以下の場合は拡大し��い。
func resizeToHeight(img image.Image, height int) image.Image {
	bounds := img.Bounds()
	if bounds.Dy() <= height {
		return img
	}
	return imaging.Resize(normalizeForResize(img), 0, height, imaging.Lanczos)
}

// resizeFit resizes img to fit within maxW x maxH preserving aspect ratio.
func resizeFit(img image.Image, maxW, maxH int) image.Image {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= maxW && h <= maxH {
		return img
	}
	return imaging.Fit(normalizeForResize(img), maxW, maxH, imaging.Lanczos)
}

func encodeWebP(img image.Image) ([]byte, error) {
	// gen2brain/webp は内部で image.Image を任意の型から libwebp に渡せるが、
	// 互換性のため明示的に NRGBA に正規化しておく (chai2010 時代と同等の前段)。
	bounds := img.Bounds()
	nrgba := image.NewNRGBA(bounds)
	draw.Draw(nrgba, bounds, img, bounds.Min, draw.Src)

	var buf bytes.Buffer
	if err := webp.Encode(&buf, nrgba, webp.Options{Quality: webpQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// encodeAVIF encodes img as AVIF using gen2brain/avif (wazero based, no cgo).
// quality / speed は WebP より重いので avifSpeed=8 を default にしておく。
func encodeAVIF(img image.Image) ([]byte, error) {
	bounds := img.Bounds()
	nrgba := image.NewNRGBA(bounds)
	draw.Draw(nrgba, bounds, img, bounds.Min, draw.Src)

	var buf bytes.Buffer
	if err := avif.Encode(&buf, nrgba, avif.Options{Quality: avifQuality, Speed: avifSpeed}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
