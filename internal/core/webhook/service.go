// Package webhook implements user and system webhook delivery dispatch.
// The service fires on event-producing core services (note / reaction /
// following / signup) via hook interfaces and enqueues HTTP delivery jobs to
// the webhook queue. The actual HTTP POST is performed by the matching
// processor (internal/queue/processors/webhook.go).
//
// 本家 Misskey の UserWebhookService / SystemWebhookService と等価。
package webhook

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/repository"
)

// Event types recognized by user webhooks, mirroring Misskey's
// `Webhook['on']` enum.
const (
	EventNote     = "note"
	EventReply    = "reply"
	EventRenote   = "renote"
	EventMention  = "mention"
	EventReaction = "reaction"
	EventFollow   = "follow"
	EventUnfollow = "unfollow"
	EventFollowed = "followed"
)

// Event types recognized by system webhooks.
const (
	SystemEventAbuseReport         = "abuseReport"
	SystemEventAbuseReportResolved = "abuseReportResolved"
	SystemEventUserCreated         = "userCreated"
)

// Service dispatches webhook delivery jobs. It holds reference to the queue
// enqueuer plus the user/system webhook repositories needed to look up
// subscribers per event.
type Service struct {
	enqueuer   queue.Enqueuer
	userRepo   repository.WebhookRepository
	systemRepo repository.SystemWebhookRepository
	server     string // instance URL (config.URL), used as payload.server
}

// NewService constructs a webhook dispatcher.
func NewService(
	enqueuer queue.Enqueuer,
	userRepo repository.WebhookRepository,
	systemRepo repository.SystemWebhookRepository,
	server string,
) *Service {
	return &Service{
		enqueuer:   enqueuer,
		userRepo:   userRepo,
		systemRepo: systemRepo,
		server:     server,
	}
}

// Envelope matches the JSON structure the Misskey frontend expects at each
// webhook endpoint. See UserWebhookDeliverProcessorService.ts in upstream for
// the canonical shape.
type Envelope struct {
	Server    string `json:"server"`
	HookID    string `json:"hookId"`
	UserID    string `json:"userId,omitempty"`
	EventID   string `json:"eventId"`
	CreatedAt int64  `json:"createdAt"`
	Type      string `json:"type"`
	Body      any    `json:"body"`
}

// DispatchUser builds an Envelope per active user webhook subscribed to
// eventType and enqueues a delivery job for each. 本家と同様に dispatch は
// best-effort で、DB や enqueue のエラーはログに留めて返さない。
func (s *Service) DispatchUser(userID, eventType string, body any) {
	s.DispatchUserLazy(userID, eventType, func() (any, bool) { return body, true })
}

// DispatchUserLazy is DispatchUser with the body built on demand. build runs
// at most once, and only when userID has an active webhook subscribed to
// eventType; returning false drops the delivery.
//
// follow 系の本文は UserDetailedNotMe で、profile や関係の行を読む。Webhook を
// 持たない利用者 (リモートの利用者を含む) のフォローのたびに読まないよう、
// 購読者が居るときだけ組み立てる (#3269)。
func (s *Service) DispatchUserLazy(userID, eventType string, build func() (any, bool)) {
	if s == nil || s.enqueuer == nil || s.userRepo == nil {
		return
	}
	hooks, err := s.userRepo.ListActiveByUserID(userID)
	if err != nil {
		slog.Warn("webhook: list user webhooks failed",
			"userId", userID, "event", eventType, "err", err)
		return
	}
	now := time.Now().UnixMilli()
	var (
		body  any
		built bool
	)
	for _, h := range hooks {
		if !slices.Contains([]string(h.On), eventType) {
			continue
		}
		if !built {
			var ok bool
			body, ok = build()
			if !ok {
				return
			}
			built = true
		}
		// Envelope は map/string/int のみを含むため json.Marshal は失敗しない。
		raw, _ := json.Marshal(Envelope{
			Server:    s.server,
			HookID:    h.ID,
			UserID:    userID,
			EventID:   uuid.NewString(),
			CreatedAt: now,
			Type:      eventType,
			Body:      body,
		})
		if err := s.enqueuer.EnqueueUserWebhook(context.Background(), queue.WebhookPayload{
			WebhookID: h.ID,
			UserID:    userID,
			EventType: eventType,
			Body:      raw,
		}); err != nil {
			slog.Warn("webhook: enqueue user webhook failed",
				"hookId", h.ID, "event", eventType, "err", err)
		}
	}
}

// DispatchUserTest enqueues a single test delivery for i/webhooks/test (#1546).
// 通常の DispatchUser と違い (1) 指定 webhookID 1 件だけに送り (= テスト対象を
// 限定)、(2) overrideURL が非空なら保存済 webhook でなく overrideURL /
// overrideSecret へ送り、(3) 再試行しない (本家の attempts: 1、#3278)。
// イベント購読 (h.On) は無視する (テストは任意の type を送れる)。
func (s *Service) DispatchUserTest(webhookID, userID, eventType string, body any, overrideURL, overrideSecret string) {
	if s == nil || s.enqueuer == nil {
		return
	}
	raw, _ := json.Marshal(Envelope{
		Server:    s.server,
		HookID:    webhookID,
		UserID:    userID,
		EventID:   uuid.NewString(),
		CreatedAt: time.Now().UnixMilli(),
		Type:      eventType,
		Body:      body,
	})
	if err := s.enqueuer.EnqueueUserWebhook(context.Background(), queue.WebhookPayload{
		WebhookID:      webhookID,
		UserID:         userID,
		EventType:      eventType,
		Body:           raw,
		OverrideURL:    overrideURL,
		OverrideSecret: overrideSecret,
		// 本家はテスト送信を attempts: 1 で積む (#3278)。
		SingleAttempt: true,
	}); err != nil {
		slog.Warn("webhook: enqueue test webhook failed",
			"hookId", webhookID, "event", eventType, "err", err)
	}
}

// DispatchSystemTest enqueues a single system webhook test delivery for
// admin/system-webhook/test (#1542)。DispatchUserTest の system 版で、UserID は
// 持たず、overrideURL が非空なら overrideURL / overrideSecret へ送る。real
// delivery (processor) を経由するため header / envelope は本配送と完全に一致する。
// 本家と同じく再試行しない (attempts: 1、#3262)。
func (s *Service) DispatchSystemTest(webhookID, eventType string, body any, overrideURL, overrideSecret string) {
	if s == nil || s.enqueuer == nil {
		return
	}
	raw, _ := json.Marshal(Envelope{
		Server:    s.server,
		HookID:    webhookID,
		EventID:   uuid.NewString(),
		CreatedAt: time.Now().UnixMilli(),
		Type:      eventType,
		Body:      body,
	})
	if err := s.enqueuer.EnqueueSystemWebhook(context.Background(), queue.WebhookPayload{
		WebhookID:      webhookID,
		EventType:      eventType,
		Body:           raw,
		OverrideURL:    overrideURL,
		OverrideSecret: overrideSecret,
		SingleAttempt:  true,
	}); err != nil {
		slog.Warn("webhook: enqueue system test webhook failed",
			"hookId", webhookID, "event", eventType, "err", err)
	}
}

// DispatchSystem mirrors DispatchUser for system webhooks (admin-level
// events such as abuseReport / userCreated). 本家 SystemWebhookService.enqueueSystemWebhook 相当。
func (s *Service) DispatchSystem(eventType string, body any) {
	s.DispatchSystemExcluding(eventType, body, nil)
}

// DispatchSystemExcluding is DispatchSystem but skips any active system webhook
// whose ID appears in excludes. 本家 enqueueSystemWebhook の opts.excludes 相当:
// abuse-report-notification の resolve 通知では、inactive な notification
// recipient が指す systemWebhookId を excludes に渡して配送対象から外す
// (#1723、AbuseReportNotificationService.notifySystemWebhook)。
func (s *Service) DispatchSystemExcluding(eventType string, body any, excludes []string) {
	if s == nil || s.enqueuer == nil || s.systemRepo == nil {
		return
	}
	hooks, err := s.systemRepo.ListActive()
	if err != nil {
		slog.Warn("webhook: list system webhooks failed",
			"event", eventType, "err", err)
		return
	}
	now := time.Now().UnixMilli()
	for _, h := range hooks {
		if !slices.Contains([]string(h.On), eventType) {
			continue
		}
		if slices.Contains(excludes, h.ID) {
			continue
		}
		raw, _ := json.Marshal(Envelope{
			Server:    s.server,
			HookID:    h.ID,
			EventID:   uuid.NewString(),
			CreatedAt: now,
			Type:      eventType,
			Body:      body,
		})
		if err := s.enqueuer.EnqueueSystemWebhook(context.Background(), queue.WebhookPayload{
			WebhookID: h.ID,
			EventType: eventType,
			Body:      raw,
		}); err != nil {
			slog.Warn("webhook: enqueue system webhook failed",
				"hookId", h.ID, "event", eventType, "err", err)
		}
	}
}
