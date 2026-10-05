package federation

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode"

	// Standard library decoders for jpeg / png / gif.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	// WebP support via golang.org/x/image (already in go.mod via other
	// transitive deps).
	_ "golang.org/x/image/webp"

	coredrive "github.com/elythia-network/elythia/internal/core/drive"
)

// attachmentFetchTimeout は AP Document attachment の先頭取得に使う
// タイムアウト。inbox processor 経由で同期実行されるため、ジョブ全体を
// 遅延させすぎないよう短めに設定する。失敗時は AP の申告と URL から推測した
// 値で登録する best-effort 方針 (取得できないことを理由に添付を捨てない)。
const attachmentFetchTimeout = 3 * time.Second

// attachmentFetchMaxBytes は形式の判定と画像の寸法の復元に必要十分な
// 先頭バイト数。JPEG / PNG / GIF / WebP はいずれも 64KiB あればヘッダから
// 寸法を復元でき、形式の判定 (magic bytes) はそれより手前で足りる。サーバが
// Range を無視して全体を返す場合でも LimitReader で読み込みを打ち切る。
const attachmentFetchMaxBytes int64 = 64 * 1024

// attachmentProbeBudget bounds the total time one inbound document may spend
// probing its attachments.
//
// probe は添付ごとに**直列**で走るので、`attachmentFetchTimeout` (3s) だけでは
// `maxRemoteAttachments` (16) × 3s = 48s まで伸びる。note 単位でも予算を切って、
// 無応答のメディアサーバーを並べた 1 通で inbox worker を押さえ込めないようにする。
// 予算切れの添付は probe 失敗と同じ degrade (申告と URL からの推測) になる。
const attachmentProbeBudget = 10 * time.Second

// attachmentProbe is what the leading bytes of a remote attachment tell us.
type attachmentProbe struct {
	// MIME is the type sniffed from the content (coredrive.DetectMIME).
	MIME string
	// FileName is the Content-Disposition filename, already validated with
	// coredrive.ValidateFileName; empty when absent or unusable.
	FileName string
	// Width / Height are decoded from the image header; zero when the
	// content is not a decodable image.
	Width, Height int
}

// fetchAttachmentProbe issues a ranged GET against rawURL and inspects the
// leading bytes. upstream は `uploadFromUrl` でリモートの添付もいったん
// ダウンロードし、形式を中身から (file-type)、名前を Content-Disposition から
// 取る。mk-go はリンクとして登録するので全体は取らず、先頭だけで同じ情報を得る
// (#3243)。
//
// httpClient is taken as a parameter so tests can inject a mock client.
// In production, callers MUST pass an SSRF-safe *http.Client (see
// probeAttachment / router.go's safehttp wiring). nil は test convenience と
// して http.DefaultClient にフォールバックするのみで、production 経路は
// probeAttachment が nil を弾くので到達しない。
func fetchAttachmentProbe(ctx context.Context, httpClient *http.Client, rawURL string) (attachmentProbe, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, attachmentFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return attachmentProbe{}, fmt.Errorf("federation: build attachment request: %w", err)
	}
	// User-Agent を付けないと Cloudflare 等の WAF に弾かれることがあるので明示する。
	req.Header.Set("User-Agent", "mk-go (+attachment-probe)")
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", attachmentFetchMaxBytes-1))

	resp, err := httpClient.Do(req)
	if err != nil {
		return attachmentProbe{}, fmt.Errorf("federation: attachment fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Range を解するサーバは 206、解さないサーバは 200 で全体を返す。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return attachmentProbe{}, fmt.Errorf("federation: attachment fetch: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, attachmentFetchMaxBytes))
	if err != nil {
		return attachmentProbe{}, fmt.Errorf("federation: read attachment: %w", err)
	}
	if len(body) == 0 {
		return attachmentProbe{}, fmt.Errorf("federation: attachment fetch: empty body")
	}

	// 応答の Content-Type は見ない。upstream も中身で判定するうえ、配信側が
	// octet-stream しか返さない実装 (Misskey 系のオブジェクトストレージ) がある。
	p := attachmentProbe{
		MIME:     coredrive.DetectMIME(body),
		FileName: dispositionFileName(resp.Header.Get("Content-Disposition")),
	}
	if strings.HasPrefix(p.MIME, "image/") {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(body)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			p.Width, p.Height = cfg.Width, cfg.Height
		}
	}
	return p, nil
}

// dispositionFileName extracts the filename from a Content-Disposition
// header. `filename*` (RFC 5987) は mime.ParseMediaType が decode して
// `filename` より優先する (upstream の content-disposition パッケージと同じ順)。
func dispositionFileName(header string) string {
	if header == "" {
		return ""
	}
	_, params, err := mime.ParseMediaType(header)
	if err != nil {
		return ""
	}
	name := params["filename"]
	if !coredrive.ValidateFileName(name) || hasUnsafeFileNameRune(name) {
		return ""
	}
	return name
}

// hasUnsafeFileNameRune reports control characters and bidi controls.
//
// URL の path からは作れない値が Content-Disposition からは来る。Go の
// mime.ParseMediaType は quoted-string の中の制御文字をそのまま通し、
// `filename*` の percent-encoding は改行も U+202E (RLO) も復元する。RLO は
// `photo<RLO>gnp.exe` のように表示上の拡張子を偽装できるので、名前として
// 採らずに URL 由来の名前へ倒す。
func hasUnsafeFileNameRune(name string) bool {
	for _, r := range name {
		if unicode.IsControl(r) {
			return true
		}
		switch {
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F, r == 0x061C:
			return true
		}
	}
	return false
}

// probeAttachment wraps fetchAttachmentProbe with logging at WARN for
// failed probes (operator visibility). Returns ok=false on any failure,
// never panics.
//
// httpClient は SSRF-safe transport を持つ *http.Client (典型的には
// resolver.attachmentProbeClient = router.go で safehttp の transport を
// 適用したもの) を渡すこと。untrusted な URL を fetch する経路なので
// http.DefaultClient で呼ぶと SSRF 脆弱性になる (#464 review)。nil 渡しは
// 呼び出し側で gate されている前提だが、安全側に倒して probe 自体を止める。
//
// **ctx は呼び出し側が握る。** 呼び出し側 (upsertAttachments) は note 単位の
// 予算を張った ctx を渡す。
func probeAttachment(ctx context.Context, httpClient *http.Client, rawURL string) (attachmentProbe, bool) {
	if httpClient == nil {
		// SSRF 対策: 未配線時は probe しない (defaultClient フォールバック禁止)
		return attachmentProbe{}, false
	}
	p, err := fetchAttachmentProbe(ctx, httpClient, rawURL)
	if err != nil {
		slog.Warn("federation: attachment probe failed", "url", rawURL, "err", err)
		return attachmentProbe{}, false
	}
	return p, true
}

// resolveAttachmentMIME picks drive_file.type for a remote attachment.
//
// 優先順は (1) 中身から判定した値、(2) AP の `mediaType`、(3) AP の `type` と
// URL の拡張子からの推測、(4) octet-stream。
//
// **中身の判定を申告より優先する** のは upstream と同じ (申告を見ずに file-type で
// 決める)。ただし判定が octet-stream (判定不能) と text/plain (標準 sniffer が
// 「バイナリでない何か」に付ける曖昧な値) のときは、申告の方が具体的なので申告を
// 採る。text/plain は申告も推測も無いときだけ使う。
//
// **メディアと名乗る添付が文書に見えたら、判定を捨てる。** 配信側が 200 で返す
// エラーページ (メンテナンス画面、WAF、CDN の soft-404、S3 の `<Error>` XML、
// JSON のエラー) を読んだ形で、ここで採ると本物の画像が恒久的に文書扱いになる
// (添付は URI で dedup するので取り直さない)。upstream は取得した中身をそのまま
// 採るが、一時的な失敗で表示が戻らなくなる方が害が大きいので申告側に倒す。
func resolveAttachmentMIME(sniffed, declared, apType, rawURL string) string {
	if claimsMedia(declared, apType) && isDocumentMIME(sniffed) {
		sniffed = ""
	}
	if sniffed != "" && sniffed != coredrive.MIMEOctetStream && sniffed != "text/plain" {
		return sniffed
	}
	if declared != "" && declared != coredrive.MIMEOctetStream && fitsColumn(declared, driveFileTypeMaxRunes) {
		return declared
	}
	if inferred := inferAttachmentMIME(apType, rawURL); inferred != "" {
		return inferred
	}
	if sniffed == "text/plain" {
		return sniffed
	}
	return coredrive.MIMEOctetStream
}

// claimsMedia reports whether the AP object presents itself as an image,
// video or audio file, by its declared mediaType or its object type.
func claimsMedia(declared, apType string) bool {
	switch apType {
	case "Image", "Video", "Audio":
		return true
	}
	return strings.HasPrefix(declared, "image/") ||
		strings.HasPrefix(declared, "video/") ||
		strings.HasPrefix(declared, "audio/")
}

// isDocumentMIME reports whether a sniffed type is a text document, which is
// what an error page served with status 200 sniffs as.
func isDocumentMIME(sniffed string) bool {
	switch sniffed {
	case "application/json", "application/xml", "application/xhtml+xml":
		return true
	}
	return strings.HasPrefix(sniffed, "text/")
}

// extensionMIME maps URL extensions to media types for the fallback guess.
//
// **SVG を入れない。** 拡張子だけで image/svg+xml と名乗らせると、中身を
// 一度も見ていないスクリプト入りの文書が画像として扱われる経路になる。
// 入れているのは表示の分岐 (画像 / 動画 / 音声) を決めるのに要るものだけ。
// mime.TypeByExtension は使わない — 表がホストの mime.types に依存する
// (distroless の image では最小限の組み込み表しか無い)。
var extensionMIME = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".apng": "image/apng",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".heic": "image/heic",
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".mkv":  "video/x-matroska",
	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".ogg":  "audio/ogg",
	".opus": "audio/opus",
	".wav":  "audio/wav",
	".flac": "audio/flac",
	".aac":  "audio/aac",
}

// inferAttachmentMIME guesses a media type from the AP object type and the
// URL extension. Image / Video / Audio は拡張子の分類と一致するときだけ採る
// (`type: Audio` の `.webm` を video/webm と名乗らせない)。Document と type
// 無しは分類を問わない。Page などリンク系の型は推測しない。
func inferAttachmentMIME(apType, rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	guess, ok := extensionMIME[strings.ToLower(path.Ext(u.Path))]
	if !ok {
		return ""
	}
	switch apType {
	case "", "Document":
		return guess
	case "Image":
		if strings.HasPrefix(guess, "image/") {
			return guess
		}
	case "Video":
		if strings.HasPrefix(guess, "video/") {
			return guess
		}
	case "Audio":
		if strings.HasPrefix(guess, "audio/") {
			return guess
		}
	}
	return ""
}
