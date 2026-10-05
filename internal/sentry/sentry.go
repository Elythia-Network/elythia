// Package sentry wires the getsentry/sentry-go client to the Misskey
// configuration and exposes a thin Echo middleware that captures panics and
// handler errors. Lives in its own package so its tests do not influence the
// surrounding subsystem coverage.
package sentry

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/misc/redact"
	sentrygo "github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v4"
)

// flushTimeout is the maximum duration to block draining queued events when
// Flush is called at shutdown.
const flushTimeout = 2 * time.Second

// Init initializes the global sentry-go client when cfg.SentryForBackend is
// non-nil. The returned flush function is always safe to call (it is a no-op
// when Sentry is disabled) and the caller is expected to defer it.
//
// Returning the flush func through the API — rather than using
// sentry.Flush directly at the call site — keeps callers free of a direct
// sentry-go import for the disabled case.
func Init(cfg *config.Config) (func(), error) {
	if cfg == nil || cfg.SentryForBackend == nil {
		return func() {}, nil
	}
	opts := cfg.SentryForBackend.Options
	if opts.DSN == "" {
		// DSN 必須。空のまま init すると sentry-go は環境変数 SENTRY_DSN を
		// 探しに行くので、設定意図が曖昧になる。明示的に弾く。
		return nil, fmt.Errorf("sentry: SentryForBackend.Options.DSN is required when sentryForBackend is set")
	}
	if cfg.SentryForBackend.EnableNodeProfiling {
		// Node 専用機能なので Go では何もしない。設定者に気付かせるため起動時に通知。
		slog.Info("sentry: enableNodeProfiling is a no-op on the Go backend; ignored")
	}
	if err := sentrygo.Init(sentrygo.ClientOptions{
		Dsn:              opts.DSN,
		Environment:      opts.Environment,
		Release:          opts.Release,
		SampleRate:       sampleRateOrDefault(opts.SampleRate),
		TracesSampleRate: opts.TracesSampleRate,
		Debug:            opts.Debug,
		ServerName:       opts.ServerName,
		// SendDefaultPII は既定 (false) のままにする。true にすると
		// sentry-go が Cookie ヘッダと全 request header を event に載せる
		// (interfaces.go NewRequest)。
		SendDefaultPII: false,
		BeforeSend:     scrubEvent,
	}); err != nil {
		return nil, fmt.Errorf("sentry init: %w", err)
	}
	slog.Info("sentry: initialized", "environment", opts.Environment, "release", opts.Release)
	return func() { sentrygo.Flush(flushTimeout) }, nil
}

// scrubEvent removes credentials from an event before it leaves the process.
//
// middleware は `hub.Scope().SetRequest(c.Request())` を呼ぶので、sentry-go は
// `Request.QueryString` に `r.URL.RawQuery` をそのまま載せる。この API は
// `?i=<token>` を有効な credential として受け付けるため、panic / error が
// 起きた瞬間に有効な token が Sentry organization へ送られる。送信先の
// 権限境界と retention はこちらの管理外なので、出る前に落とす。
//
// sentry-go v0.45.1 の実測として:
//
//   - `Request.URL` は scheme + host + **path のみ**で query を含まない
//   - `Request.Data` (body) は capture されない (NewRequest の doc に
//     「does not read r.Body」と明記)
//   - `Cookies` と全 header は SendDefaultPII が true のときだけ載る
//
// つまり現状の実害経路は QueryString だけだが、Data / Cookies も防御的に
// 落としておく。SDK の更新や SendDefaultPII の設定変更で経路が増えたときに、
// ここを直し忘れても漏れない側に倒すため。
func scrubEvent(event *sentrygo.Event, _ *sentrygo.EventHint) *sentrygo.Event {
	if event == nil || event.Request == nil {
		return event
	}
	req := event.Request
	// 解析できないクエリは丸ごと捨てる。そのまま残すのは漏らす方向の失敗。
	if redacted, ok := redact.Query(req.QueryString); ok {
		req.QueryString = redacted
	} else {
		req.QueryString = ""
	}
	req.Data = ""
	req.Cookies = ""
	req.URL = redact.URI(req.URL)
	for key := range req.Headers {
		if sentrygo.IsSensitiveHeader(key) {
			delete(req.Headers, key)
		}
	}
	return event
}

// sampleRateOrDefault treats a zero rate as "use sentry-go default" because
// 0.0 means "drop everything" which is rarely the user's intent. Misskey's
// upstream defaults to 1.0 (capture all errors).
func sampleRateOrDefault(r float64) float64 {
	if r <= 0 {
		return 1.0
	}
	return r
}

// Middleware returns an Echo middleware that forwards panics and handler
// errors to the global sentry-go client. When Sentry is disabled the
// middleware short-circuits to a pass-through to keep the request hot path
// allocation-free.
func Middleware(cfg *config.Config) echo.MiddlewareFunc {
	if cfg == nil || cfg.SentryForBackend == nil {
		return func(next echo.HandlerFunc) echo.HandlerFunc { return next }
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			// 各リクエスト毎に hub を分離 (タグ・ユーザー情報がリクエスト境界で混ざらない)
			hub := sentrygo.CurrentHub().Clone()
			hub.Scope().SetRequest(c.Request())
			defer func() {
				if r := recover(); r != nil {
					hub.RecoverWithContext(c.Request().Context(), r)
					// echo の Recover middleware に巻き戻す
					panic(r)
				}
			}()
			err = next(c)
			if err != nil {
				hub.CaptureException(err)
			}
			return err
		}
	}
}
