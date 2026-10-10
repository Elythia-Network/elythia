package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
)

// EventKind is the kind of a notification.
type EventKind string

// Notification kinds.
const (
	// EventFailure: taking, verifying or pruning returned an error.
	EventFailure EventKind = "failure"
	// EventMismatch: the verification ran and found the backup broken.
	EventMismatch EventKind = "mismatch"
	// EventDelay: no usable generation was made within DelayAfter.
	EventDelay EventKind = "delay"
)

// Event is one notification.
type Event struct {
	Kind EventKind `json:"event"`
	// Instance is the URL of the Elythia server the backup belongs to.
	Instance     string    `json:"instance"`
	OccurredAt   time.Time `json:"occurredAt"`
	GenerationID string    `json:"generationId,omitempty"`
	// Stage is "take", "verify" or "prune" for EventFailure.
	Stage   string `json:"stage,omitempty"`
	Message string `json:"message"`
	// FailedStages and Mismatches come from verify.json for EventMismatch.
	FailedStages []backup.StageResult `json:"failedStages,omitempty"`
	Mismatches   []backup.RowMismatch `json:"mismatches,omitempty"`
	// LastUsableAt is the newest usable generation for EventDelay (nil when
	// there is none).
	LastUsableAt *time.Time `json:"lastUsableAt,omitempty"`
}

// Text renders e as one human-readable message.
func (e Event) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[Elythia backup] %s: ", e.Instance)
	switch e.Kind {
	case EventFailure:
		fmt.Fprintf(&b, "%s failed", e.Stage)
	case EventMismatch:
		b.WriteString("verification found a broken backup")
	case EventDelay:
		b.WriteString("no new backup")
	}
	if e.GenerationID != "" {
		fmt.Fprintf(&b, " (generation %s)", e.GenerationID)
	}
	b.WriteString(": ")
	b.WriteString(e.Message)
	for _, s := range e.FailedStages {
		fmt.Fprintf(&b, "\n- stage %s: %s", s.Stage, s.Error)
	}
	for _, m := range e.Mismatches {
		fmt.Fprintf(&b, "\n- %s: expected %d rows, got %d", m.Table, m.Expected, m.Actual)
	}
	return b.String()
}

// Notifier delivers events.
type Notifier interface {
	Notify(ctx context.Context, e Event) error
}

// nopNotifier is used when no webhook is configured; the daemon still logs.
type nopNotifier struct{}

func (nopNotifier) Notify(context.Context, Event) error { return nil }

// Webhook formats.
const (
	FormatGeneric = "generic"
	FormatDiscord = "discord"
	FormatSlack   = "slack"
)

// Limits of the chat services' message text. 超えると送信そのものが 400 で
// 落ちるので、手前で切る。
const (
	discordContentLimit = 2000
	slackTextLimit      = 3000
)

// messageLimit bounds the error text put into an event, so that a long
// pg_dump stderr does not make the payload unwieldy.
const messageLimit = 1000

// Webhook posts events to a URL.
type Webhook struct {
	url    string
	format string
	client *http.Client
	// retryDelays are the waits before the second and later attempts.
	retryDelays []time.Duration
}

// NewWebhook validates the URL and format and returns a Webhook that posts
// with client. An empty format means generic.
//
// client は呼び出し側が渡す。本番では internal/safehttp の SSRF 対策付きの
// transport (allowedPrivateNetworks、proxy、outgoingAddress を尊重するもの) を使う。
// URL は運営者が設定ファイルに書く値で利用者の入力ではないが、本体の他の外向きの
// 通信と同じ経路・同じ制限に揃える。同じ LAN の通知先へ送るときは、本体と同じく
// allowedPrivateNetworks に書いて許す。
func NewWebhook(rawURL, format string, client *http.Client) (*Webhook, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("backup.notify.webhookUrl: must be an http(s) URL")
	}
	switch format {
	case "":
		format = FormatGeneric
	case FormatGeneric, FormatDiscord, FormatSlack:
	default:
		return nil, fmt.Errorf("backup.notify.format: unknown format %q (generic, discord or slack)", format)
	}
	return &Webhook{url: rawURL, format: format, client: client, retryDelays: []time.Duration{2 * time.Second, 10 * time.Second}}, nil
}

// Payload returns the JSON body sent for e.
func (w *Webhook) Payload(e Event) ([]byte, error) {
	switch w.format {
	case FormatDiscord:
		return json.Marshal(map[string]any{
			"content": truncateRunes(e.Text(), discordContentLimit),
			// 失敗の文面は外から来る (pg_dump の stderr など)。@everyone などで
			// 意図しない一斉通知を起こさないよう、mention を全て無効にする。
			"allowed_mentions": map[string]any{"parse": []string{}},
		})
	case FormatSlack:
		return json.Marshal(map[string]any{"text": truncateRunes(slackEscape(e.Text()), slackTextLimit)})
	default:
		return json.Marshal(struct {
			Event
			Text string `json:"text"`
		}{e, e.Text()})
	}
}

// Notify posts e, retrying a failed attempt twice.
func (w *Webhook) Notify(ctx context.Context, e Event) error {
	body, err := w.Payload(e)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= len(w.retryDelays); attempt++ {
		if attempt > 0 {
			t := time.NewTimer(w.retryDelays[attempt-1])
			select {
			case <-ctx.Done():
				t.Stop()
				return errors.Join(lastErr, ctx.Err())
			case <-t.C:
			}
		}
		if lastErr = w.post(ctx, body); lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (w *Webhook) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		// Discord / Slack の URL は token を含むので、URL を載せる *url.Error を
		// そのまま返さない (ログと /status に出る)。
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("webhook: %w", ue.Err)
		}
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook: status %d", resp.StatusCode)
	}
	return nil
}

// NewWebhookClient returns the client NewWebhook expects, using transport.
// It does not follow redirects: a webhook endpoint answers directly, and
// following would let a redirect send the payload to another host.
func NewWebhookClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// slackEscape escapes the three characters Slack treats as markup.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
