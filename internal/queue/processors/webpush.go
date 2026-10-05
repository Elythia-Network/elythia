package processors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	webpushlib "github.com/SherClockHolmes/webpush-go"
	"github.com/elythia-network/elythia/internal/core/webpush"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/queue"
	"github.com/elythia-network/elythia/internal/queue/driver"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// Sender abstracts the webpush delivery call so the processor can be unit
// tested without a real HTTP client.
type Sender interface {
	Send(ctx context.Context, sub *webpushlib.Subscription, message []byte, opts *webpushlib.Options) (*http.Response, error)
}

// LibraryWebPushSender is the production Sender backed by webpush-go.
//
// HTTPClient は webpushlib.Options.HTTPClient に注入する outbound HTTP
// client。SSRF-safe transport + forward proxy 経由の client を渡すと FCM /
// Mozilla / Apple push endpoint への配送も operator の outbound 政策に従う
// (#638)。nil のときは webpush-go 既定の http.DefaultClient ではなく、
// defaultWebPushHTTPClient (allowedPrivateNetworks 無しの SSRF-safe client)
// を使う。
type LibraryWebPushSender struct {
	HTTPClient webpushlib.HTTPClient
}

// Send delegates to webpush.SendNotificationWithContext.
//
// 受け取った opts は immutable に扱う: caller の Options を mutate しないよう
// shallow copy してから HTTPClient を埋めて library に渡す。
func (s LibraryWebPushSender) Send(ctx context.Context, sub *webpushlib.Subscription, message []byte, opts *webpushlib.Options) (*http.Response, error) {
	if opts != nil && opts.HTTPClient == nil {
		// 本番は router が SSRF-safe な client を注入するが、注入を忘れた
		// 呼び出し元でも http.DefaultClient (宛先の検査なし) に落ちないよう、
		// 既定でも宛先を検査する client を使う。
		client := s.HTTPClient
		if client == nil {
			client = defaultWebPushHTTPClient
		}
		copied := *opts
		copied.HTTPClient = client
		opts = &copied
	}
	return webpushlib.SendNotificationWithContext(ctx, message, sub, opts)
}

// defaultWebPushHTTPClient is the outbound client LibraryWebPushSender falls
// back to when none is injected. It refuses private / loopback destinations
// (no allowedPrivateNetworks) and bounds each delivery like the production
// client wired in internal/server.
var defaultWebPushHTTPClient webpushlib.HTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: safehttp.NewSSRFSafeTransport(nil),
}

// WebPushProcessor handles `webpush:notify` tasks by looking up a user's
// SwSubscription list (via cache) and posting to each subscription endpoint.
// Delivery mirrors Misskey's PushNotificationService.ts for wire-compat with
// the upstream service worker.
type WebPushProcessor struct {
	cache    *webpush.SubscriptionCache
	swRepo   repository.SwSubscriptionRepository
	metaRepo repository.MetaRepository
	sender   Sender
	subject  string
}

// NewWebPushProcessor constructs a WebPushProcessor. subject is the `mailto:`
// URI used as the VAPID `sub` claim; Misskey uses the instance URL.
func NewWebPushProcessor(
	cache *webpush.SubscriptionCache,
	swRepo repository.SwSubscriptionRepository,
	metaRepo repository.MetaRepository,
	sender Sender,
	subject string,
) *WebPushProcessor {
	if sender == nil {
		sender = LibraryWebPushSender{}
	}
	return &WebPushProcessor{
		cache:    cache,
		swRepo:   swRepo,
		metaRepo: metaRepo,
		sender:   sender,
		subject:  subject,
	}
}

// Handle dispatches a Web Push task to all subscribers of the target user.
// Errors are logged but never propagated unless the task payload itself is
// malformed (which makes the task unrecoverable — ErrSkipRetry).
func (p *WebPushProcessor) Handle(ctx context.Context, t driver.Task) error {
	payload, err := queue.DecodeWebPushPayload(t.Payload())
	if err != nil {
		return fmt.Errorf("decode webpush payload: %w: %w", err, driver.ErrSkipRetry)
	}
	if payload.UserID == "" {
		return fmt.Errorf("webpush payload missing userId: %w", driver.ErrSkipRetry)
	}

	// VAPID 鍵が未設定 / enableServiceWorker=false ならそもそも配信しない
	// (本家の PushNotificationService.ts も同様に早期 return する)
	meta, err := p.metaRepo.Fetch()
	if err != nil {
		return fmt.Errorf("fetch meta: %w", err)
	}
	if !meta.EnableServiceWorker || meta.SwPublicKey == nil || meta.SwPrivateKey == nil {
		return nil
	}
	vapidPublic := *meta.SwPublicKey
	vapidPrivate := *meta.SwPrivateKey

	subs, err := p.cache.Get(ctx, payload.UserID)
	if err != nil {
		return fmt.Errorf("load subscriptions: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}

	message, err := json.Marshal(pushEnvelope{
		Type:     payload.Type,
		Body:     payload.Body,
		UserID:   payload.UserID,
		DateTime: nowMillis(),
	})
	if err != nil {
		return fmt.Errorf("marshal push envelope: %w: %w", err, driver.ErrSkipRetry)
	}

	invalidate := false
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		// readAllNotifications はフロント設定で抑制可能
		if payload.Type == webpush.TypeReadAllNotifications && !sub.SendReadMessage {
			continue
		}
		// 本家 PushNotificationService と同じく、配送先として使えない endpoint
		// には送らない。登録時の検証が入る前に保存された行や、移行元の DB から
		// 引き継いだ行が該当する。
		if !webpush.IsValidEndpoint(sub.Endpoint) {
			continue
		}
		if err := p.deliverOne(ctx, sub, message, vapidPublic, vapidPrivate); err != nil {
			if errors.Is(err, errGone) {
				invalidate = true
				if derr := p.swRepo.DeleteByUserAndEndpoint(sub.UserID, sub.Endpoint); derr != nil {
					slog.Warn("webpush: delete expired subscription failed",
						"user", sub.UserID, "endpoint", sub.Endpoint, "err", derr)
				}
				continue
			}
			slog.Warn("webpush: deliver failed",
				"user", sub.UserID, "endpoint", sub.Endpoint, "err", err)
		}
	}
	if invalidate {
		p.cache.Invalidate(ctx, payload.UserID)
	}
	return nil
}

// errGone is a sentinel reported by deliverOne when the push service returns
// 410 Gone (subscription permanently expired / revoked).
var errGone = errors.New("push endpoint gone")

func (p *WebPushProcessor) deliverOne(
	ctx context.Context,
	sub *model.SwSubscription,
	message []byte,
	vapidPublic, vapidPrivate string,
) error {
	s := &webpushlib.Subscription{
		Endpoint: sub.Endpoint,
		Keys: webpushlib.Keys{
			Auth:   sub.Auth,
			P256dh: sub.PublicKey,
		},
	}
	opts := &webpushlib.Options{
		Subscriber:      p.subject,
		VAPIDPublicKey:  vapidPublic,
		VAPIDPrivateKey: vapidPrivate,
		TTL:             30,
	}
	resp, err := p.sender.Send(ctx, s, message, opts)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusGone:
		return errGone
	default:
		return fmt.Errorf("push endpoint status %d", resp.StatusCode)
	}
}

// pushEnvelope is the exact JSON structure sw.js expects on the `push` event.
// 本家 PushNotificationService.ts (line 99-104) と同一キー構成。
type pushEnvelope struct {
	Type     string          `json:"type"`
	Body     json.RawMessage `json:"body"`
	UserID   string          `json:"userId"`
	DateTime int64           `json:"dateTime"`
}

// nowMillis returns the current time as Unix milliseconds. 変数化してテストで
// 差し替え可能にしている (sw.jsが24h超の通知を破棄するため、固定時刻テストが
// 必要)。
var nowMillis = func() int64 { return time.Now().UnixMilli() }
