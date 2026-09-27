package drive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/misc/idnhost"
)

// SensitiveDetector scores media content for NSFW likelihood.
// Implementations may call external services (e.g., NudeNet).
type SensitiveDetector interface {
	// Detect returns a score in [0, 1] where 1 = definitely NSFW.
	Detect(ctx context.Context, body []byte, mime string) (float64, error)
}

// SensitiveConfig holds meta-driven sensitive detection settings.
type SensitiveConfig struct {
	// Detection: "none", "all", "local", "remote"
	Detection string
	// Sensitivity: "veryLow", "low", "medium", "high", "veryHigh"
	Sensitivity string
	// SetFlagAutomatically updates DriveFile.IsSensitive based on detection.
	SetFlagAutomatically bool
	// EnableForVideos enables detection for video MIME types.
	EnableForVideos bool
	// SilencedHosts forces IsSensitive=true for media from these hosts.
	SilencedHosts []string
}

// SensitivityThreshold maps sensitivity names to score thresholds.
// score >= threshold → sensitive.
func SensitivityThreshold(sensitivity string) float64 {
	switch strings.ToLower(sensitivity) {
	case "verylow":
		return 0.95
	case "low":
		return 0.8
	case "medium":
		return 0.5
	case "high":
		return 0.3
	case "veryhigh":
		return 0.1
	default:
		return 0.5
	}
}

// IsSilencedHost reports whether host is in the silenced hosts list
// (case-insensitive, subdomain matching like bannedEmailDomains). The port and
// trailing dot of host are ignored, so `evil.example:8443` is silenced by an
// `evil.example` entry (idnhost.MatchesBlockList).
func IsSilencedHost(host string, silencedHosts []string) bool {
	return idnhost.MatchesBlockList(silencedHosts, host)
}

// ShouldDetect returns whether detection should run for the given upload
// context based on the Detection mode.
func ShouldDetect(cfg SensitiveConfig, isLocalUser bool) bool {
	switch cfg.Detection {
	case "all":
		return true
	case "local":
		return isLocalUser
	case "remote":
		return !isLocalUser
	default:
		return false
	}
}

// IsVideoMIME reports whether the MIME type is a video type.
func IsVideoMIME(mime string) bool {
	return strings.HasPrefix(mime, "video/")
}

// DefaultHTTPDetectorTimeout is the default per-request timeout when
// HTTPDetectorOptions.Timeout is zero.
const DefaultHTTPDetectorTimeout = 30 * time.Second

// HTTPDetectorOptions tunes outbound behaviour of HTTPDetector (#751)。
// AuthHeader が空でなければ "Name: value" として 1 行の HTTP header を注入
// する (例: "Authorization: Bearer xxx" / "X-API-Key: xxx")。SaaS detector
// (Cloudflare Workers AI / AWS Rekognition 等) を thin proxy なしで直接
// 呼ぶための仕組み。Timeout==0 なら DefaultHTTPDetectorTimeout (30s)。
type HTTPDetectorOptions struct {
	AuthHeader string
	Timeout    time.Duration
}

// HTTPDetector calls an external HTTP service for NSFW detection.
// リクエスト: POST <url> with body bytes, Content-Type header.
// レスポンス: JSON { "score": float64 }.
type HTTPDetector struct {
	url        string
	client     *http.Client
	authHeader string
	timeout    time.Duration
}

// NewHTTPDetector creates a detector that calls the given URL with default
// options (no auth header, 30s timeout)。後方互換のため signature を維持。
//
// client は detector が POST に使う outbound HTTP client。nil なら 30s
// timeout の素の Client にフォールバックするが、production では SSRF-safe
// transport + forward proxy 経由の client を渡すこと (#638)。NSFW SaaS は
// operator が信頼する endpoint だが、operator 設定で outbound 経路を集約
// したい場合のため forward proxy には乗せる。
func NewHTTPDetector(url string, client *http.Client) *HTTPDetector {
	return NewHTTPDetectorWithOptions(url, client, HTTPDetectorOptions{})
}

// NewHTTPDetectorWithOptions is the explicit form for callers that need to
// configure auth header / timeout (#751)。
//
// per-request timeout は Detect 内の context.WithTimeout で一本化して制御
// するので、client.Timeout は明示的にセットしない (caller が production の
// outbound client を渡す場合はその client が持つ timeout がさらに上限となる、
// テストで nil 渡しの場合は context-only)。
func NewHTTPDetectorWithOptions(url string, client *http.Client, opts HTTPDetectorOptions) *HTTPDetector {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultHTTPDetectorTimeout
	}
	if client == nil {
		client = &http.Client{}
	}
	return &HTTPDetector{
		url:        url,
		client:     client,
		authHeader: strings.TrimSpace(opts.AuthHeader),
		timeout:    timeout,
	}
}

func (d *HTTPDetector) Detect(ctx context.Context, body []byte, mime string) (float64, error) {
	// caller が ctx を渡してくれていれば cancellation がそのまま効く。
	// per-request timeout は Detector 側で WithTimeout して固定する
	// (caller の ctx に独自 deadline があればそちらが先に hit するので
	// どちらにも上限がかかる)。
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	// bytes.NewReader 直接で OK。`io.NopCloser(strings.NewReader(string(body)))`
	// は []byte → string → []byte で 2 回 copy する典型的な anti-pattern。
	// http.NewRequestWithContext は io.Reader を受けて Close 不要 (内部で nil 確認)。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("sensitive: request creation: %w", err)
	}
	req.Header.Set("Content-Type", mime)
	if d.authHeader != "" {
		// "Name: value" を 1 件分注入する。複数 header は scope 外 (#751)。
		if name, value, ok := strings.Cut(d.authHeader, ":"); ok {
			req.Header.Set(strings.TrimSpace(name), strings.TrimSpace(value))
		}
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("sensitive: request failed: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Score float64 `json:"score"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("sensitive: parse response: %w", err)
	}
	return result.Score, nil
}
