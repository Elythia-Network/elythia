package sentry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	mksentry "github.com/elythia-network/elythia/internal/sentry"
	sentrygo "github.com/getsentry/sentry-go"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInit_DisabledIsNoop(t *testing.T) {
	flush, err := mksentry.Init(&config.Config{})
	require.NoError(t, err)
	require.NotNil(t, flush)
	flush() // 二重呼び出しでも panic しない
	flush()
}

func TestInit_NilConfigIsNoop(t *testing.T) {
	flush, err := mksentry.Init(nil)
	require.NoError(t, err)
	require.NotNil(t, flush)
	flush()
}

func TestInit_MissingDSNFails(t *testing.T) {
	cfg := &config.Config{
		SentryForBackend: &config.SentryBackendOptions{
			Options: config.SentryOptions{}, // DSN 空
		},
	}
	flush, err := mksentry.Init(cfg)
	require.Error(t, err)
	assert.Nil(t, flush)
}

func TestInit_ValidDSNSucceeds(t *testing.T) {
	// 標準的な dummy DSN。実通信はしない (transport 差し替え or サンプリング 0)。
	cfg := &config.Config{
		SentryForBackend: &config.SentryBackendOptions{
			EnableNodeProfiling: true, // ログ経路のみ通る
			Options: config.SentryOptions{
				DSN:         "https://public@o0.ingest.sentry.io/0",
				Environment: "test",
				Release:     "test-1.0",
				SampleRate:  1.0,
				Debug:       false,
				ServerName:  "mk-test",
			},
		},
	}
	flush, err := mksentry.Init(cfg)
	require.NoError(t, err)
	require.NotNil(t, flush)
	flush()
}

func TestInit_BadDSNFails(t *testing.T) {
	cfg := &config.Config{
		SentryForBackend: &config.SentryBackendOptions{
			Options: config.SentryOptions{
				DSN: "::not a valid dsn::",
			},
		},
	}
	_, err := mksentry.Init(cfg)
	require.Error(t, err)
}

func TestMiddleware_DisabledPassesThrough(t *testing.T) {
	mw := mksentry.Middleware(&config.Config{})
	require.NotNil(t, mw)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	called := false
	handler := mw(func(c echo.Context) error {
		called = true
		return c.String(http.StatusOK, "ok")
	})
	require.NoError(t, handler(c))
	assert.True(t, called)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddleware_NilCfgPassesThrough(t *testing.T) {
	mw := mksentry.Middleware(nil)
	require.NotNil(t, mw)

	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, mw(func(c echo.Context) error { return c.String(http.StatusOK, "ok") })(c))
	assert.Equal(t, http.StatusOK, rec.Code)
}

// captureTransport は sentry-go の Transport インターフェースを満たす最小実装。
// 送信されたイベント本体をテストから観察できるようにする。
type captureTransport struct {
	mu     sync.Mutex
	events []*sentrygo.Event
}

func (t *captureTransport) Configure(_ sentrygo.ClientOptions) {}
func (t *captureTransport) SendEvent(event *sentrygo.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, event)
}
func (t *captureTransport) Flush(_ time.Duration) bool              { return true }
func (t *captureTransport) FlushWithContext(_ context.Context) bool { return true }
func (t *captureTransport) Close()                                  {}

func TestMiddleware_CapturesHandlerError(t *testing.T) {
	transport := &captureTransport{}
	require.NoError(t, sentrygo.Init(sentrygo.ClientOptions{
		Dsn:       "https://public@o0.ingest.sentry.io/0",
		Transport: transport,
	}))
	defer sentrygo.Flush(0)

	cfg := &config.Config{
		SentryForBackend: &config.SentryBackendOptions{
			Options: config.SentryOptions{DSN: "https://public@o0.ingest.sentry.io/0"},
		},
	}
	mw := mksentry.Middleware(cfg)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	wantErr := errors.New("boom")
	handler := mw(func(c echo.Context) error { return wantErr })
	gotErr := handler(c)
	require.ErrorIs(t, gotErr, wantErr)

	transport.mu.Lock()
	defer transport.mu.Unlock()
	require.Len(t, transport.events, 1, "exactly one event should reach Sentry")
	require.NotEmpty(t, transport.events[0].Exception)
	assert.Equal(t, "boom", transport.events[0].Exception[0].Value)
}

func TestMiddleware_CapturesPanicAndRepanics(t *testing.T) {
	transport := &captureTransport{}
	require.NoError(t, sentrygo.Init(sentrygo.ClientOptions{
		Dsn:       "https://public@o0.ingest.sentry.io/0",
		Transport: transport,
	}))
	defer sentrygo.Flush(0)

	cfg := &config.Config{
		SentryForBackend: &config.SentryBackendOptions{
			Options: config.SentryOptions{DSN: "https://public@o0.ingest.sentry.io/0"},
		},
	}
	mw := mksentry.Middleware(cfg)
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	handler := mw(func(c echo.Context) error { panic("kaboom") })

	// echo の Recover に巻き戻すため再 panic する仕様。テストでは defer recover で受ける。
	defer func() {
		r := recover()
		require.Equal(t, "kaboom", r)

		transport.mu.Lock()
		defer transport.mu.Unlock()
		require.NotEmpty(t, transport.events, "panic should be reported to Sentry")
	}()
	_ = handler(c)
}
