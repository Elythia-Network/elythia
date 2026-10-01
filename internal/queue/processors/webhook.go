package processors

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/repository"
)

// WebhookStatusRecorder is the subset of repository methods the processor
// uses to persist the latest delivery outcome. Abstracted so that user and
// system webhook processors share the same Handle implementation.
type WebhookStatusRecorder interface {
	UpdateLatestStatus(id string, sentAt time.Time, status int) error
}

// WebhookHTTPClient abstracts the HTTP round-trip so the processor can be
// unit tested with a fake transport.
type WebhookHTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// DefaultWebhookTimeout is the per-request HTTP timeout for webhook delivery.
// Exported so the wire layer can use the same value when constructing the
// SSRF-safe outbound client.
//
// 本家の webhook 配送は HttpRequestService.send の既定 5 秒を使う (#3263)。
// 以前は「本家と同じ 10 秒」としていたが誤りだった。
const DefaultWebhookTimeout = 5 * time.Second

// webhookResponseDrainLimit bounds how much of the response body is read
// before closing it.
//
// 本家は応答の本文を読まない。こちらは接続を使い回すために読み捨てるが、
// 上限が無いと、大きな本文を返す相手に対してタイムアウトまで読み続ける (#3263)。
// 上限を超えた分は読まずに閉じる (その接続は使い回されないだけ)。
const webhookResponseDrainLimit = 64 << 10

// WebhookProcessor handles a single webhook delivery task. It is instantiated
// twice (once for user webhooks, once for system webhooks) with different
// status recorders and URL/secret lookup strategies.
type WebhookProcessor struct {
	userRepo   repository.WebhookRepository
	systemRepo repository.SystemWebhookRepository
	client     WebhookHTTPClient
	host       string // instance hostname, used as X-Misskey-Host
	userAgent  string
}

// NewWebhookProcessor constructs a WebhookProcessor. A zero client falls back
// to http.DefaultClient with the configured timeout.
func NewWebhookProcessor(
	userRepo repository.WebhookRepository,
	systemRepo repository.SystemWebhookRepository,
	client WebhookHTTPClient,
	host string,
) *WebhookProcessor {
	if client == nil {
		client = &http.Client{Timeout: DefaultWebhookTimeout}
	}
	return &WebhookProcessor{
		userRepo:   userRepo,
		systemRepo: systemRepo,
		client:     client,
		host:       host,
		userAgent:  "Misskey-Hooks",
	}
}

// HandleUser dispatches a user webhook task.
func (p *WebhookProcessor) HandleUser(ctx context.Context, t driver.Task) error {
	return p.handle(ctx, t, true)
}

// HandleSystem dispatches a system webhook task.
func (p *WebhookProcessor) HandleSystem(ctx context.Context, t driver.Task) error {
	return p.handle(ctx, t, false)
}

// handle is the shared delivery implementation. user==true uses user webhooks,
// user==false uses system webhooks.
func (p *WebhookProcessor) handle(ctx context.Context, t driver.Task, user bool) error {
	payload, err := queue.DecodeWebhookPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode webhook payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.WebhookID == "" {
		return fmt.Errorf("webhook payload missing webhookId: %w", driver.ErrSkipRetry)
	}

	// i/webhooks/test の override (#1546): 非空なら保存済 webhook を引かず指定の
	// url/secret へ送る。test 送信なので saved webhook の status は汚さない (下記)。
	isOverride := payload.OverrideURL != ""
	url, secret := payload.OverrideURL, payload.OverrideSecret
	if !isOverride {
		var err error
		url, secret, err = p.resolveTarget(payload.WebhookID, user)
		if err != nil {
			// **not-found と DB 障害を分ける (#2792)。** 種別を見ずに
			// `ErrSkipRetry` へ潰すと、DB が詰まった瞬間に配送待ちだった webhook が
			// **1 回で恒久 failed** になる (本来は 4 回 + backoff)。
			// 同じディレクトリの `deliver_keysource.go` /
			// `post_scheduled_note.go` は `repository.IsNotFound` で分けている。
			// 配線の問題 (repo 未設定) も恒久エラー。retry しても直らない。
			if repository.IsNotFound(err) || errors.Is(err, errWebhookConfig) {
				return fmt.Errorf("resolve webhook %s: %w: %w", payload.WebhookID, err, driver.ErrSkipRetry)
			}
			return fmt.Errorf("resolve webhook %s: %w", payload.WebhookID, err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload.Body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w: %w", err, driver.ErrSkipRetry)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", p.userAgent)
	req.Header.Set("X-Misskey-Host", p.host)
	req.Header.Set("X-Misskey-Hook-Id", payload.WebhookID)
	// #2106 L62: upstream は secret 空でも X-Misskey-Hook-Secret ヘッダーを無条件に送る
	// (受信側が header の有無で分岐する場合の wire 互換)。
	req.Header.Set("X-Misskey-Hook-Secret", secret)
	// mk-go の追加。upstream は共有秘密を平文で載せるだけなので、受信側は
	// 「本文が改ざんされていないか」を確認できない。本文の HMAC を足しておくと、
	// 秘密を知っている者だけがこの本文を送れたことまで確かめられる。
	// **未知のヘッダは無視されるだけ**なので、既存の受信側は影響を受けない。
	if sig := webhookSignature(secret, payload.Body); sig != "" {
		req.Header.Set(webhookSignatureHeader, sig)
	}

	resp, err := p.client.Do(req)
	sentAt := time.Now()
	if err != nil {
		// ネットワーク障害はリトライ対象。
		// #2106 L60: upstream は非 StatusError (ネットワーク障害) のとき latestStatus=1 を
		// 記録する (StatusError なら statusCode)。0 は「未送信」を示すので 1 に揃える。
		if !isOverride {
			p.recordStatus(payload.WebhookID, user, sentAt, 1)
		}
		slog.Warn("webhook deliver: http error",
			"hookId", payload.WebhookID, "url", url, "err", err)
		return err
	}
	defer func() {
		_, _ = io.CopyN(io.Discard, resp.Body, webhookResponseDrainLimit)
		_ = resp.Body.Close()
	}()

	if !isOverride {
		p.recordStatus(payload.WebhookID, user, sentAt, resp.StatusCode)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		// #2106 N30: 429 は 4xx だが upstream status-error.ts では retryable。
		// rate-limited な webhook 先には backoff 付きで再試行する (ErrSkipRetry を付けない)。
		return errors.New("webhook rate limited: " + resp.Status)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// 4xx は受信側の不正リクエスト扱いでリトライしない。
		return fmt.Errorf("webhook client error (%d): %w", resp.StatusCode, driver.ErrSkipRetry)
	default:
		// 5xx 等は一時的障害としてリトライ。
		return errors.New("webhook server error: " + resp.Status)
	}
}

// errWebhookConfig marks a wiring problem (repository not configured), which
// retrying cannot fix.
var errWebhookConfig = errors.New("webhook processor is not configured")

// resolveTarget looks up a webhook row's URL and secret. For user webhooks it
// queries the WebhookRepository; for system webhooks it uses the
// SystemWebhookRepository.
func (p *WebhookProcessor) resolveTarget(id string, user bool) (url, secret string, err error) {
	if user {
		if p.userRepo == nil {
			return "", "", fmt.Errorf("user webhook repo not configured: %w", errWebhookConfig)
		}
		h, err := p.userRepo.FindByID(id)
		if err != nil {
			return "", "", err
		}
		return h.URL, h.Secret, nil
	}
	if p.systemRepo == nil {
		return "", "", fmt.Errorf("system webhook repo not configured: %w", errWebhookConfig)
	}
	h, err := p.systemRepo.FindByID(id)
	if err != nil {
		return "", "", err
	}
	return h.URL, h.Secret, nil
}

// recordStatus updates latestSentAt/latestStatus for the webhook. Errors from
// the repo are logged but never propagated—status recording is best-effort.
func (p *WebhookProcessor) recordStatus(id string, user bool, sentAt time.Time, status int) {
	var recorder WebhookStatusRecorder
	if user {
		recorder = p.userRepo
	} else {
		recorder = p.systemRepo
	}
	if recorder == nil {
		return
	}
	if err := recorder.UpdateLatestStatus(id, sentAt, status); err != nil {
		slog.Warn("webhook deliver: record status failed",
			"hookId", id, "err", err)
	}
}

// webhookSignatureHeader carries the HMAC of the request body.
//
// **`X-Hub-Signature-256` を選んでいる。** WebSub / GitHub で広く使われている
// 名前で、受信側のライブラリがそのまま使えることが多い。`X-Misskey-Hook-*` の
// 名前空間を使うと、upstream が将来同じ名前で別の意味を持たせたときに受信側が
// 壊れる。
const webhookSignatureHeader = "X-Hub-Signature-256"

// webhookSignature returns `sha256=<hex>` of HMAC-SHA256(secret, body).
//
// 秘密が空なら署名しない (空鍵の HMAC は誰でも作れるので、付けると「検証できて
// いる」という誤解だけを生む)。
func webhookSignature(secret string, body []byte) string {
	if secret == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
