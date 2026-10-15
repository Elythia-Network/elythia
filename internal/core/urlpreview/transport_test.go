package urlpreview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type countingRoundTripper struct {
	n    atomic.Int64
	next http.RoundTripper
}

func (c *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.next.RoundTrip(r)
}

func TestFetcher_SetTransportKeepsTimeoutAndRedirectPolicy(t *testing.T) {
	f := NewFetcher(Config{Enabled: true, TimeoutMs: 1234}, nil)
	rt := &countingRoundTripper{next: http.DefaultTransport}

	f.SetTransport(rt)

	assert.Same(t, http.RoundTripper(rt), f.client.Transport)
	assert.Equal(t, 1234*time.Millisecond, f.client.Timeout)
	assert.NotNil(t, f.client.CheckRedirect, "AllowRedirect=false still stops at the first response")
}

// 設定を毎回読む構成 (本番) でも、差し替えた Transport を通って取得する。
func TestFetcher_SetTransportIsUsedWithConfigProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta property="og:title" content="Hello"></head></html>`))
	}))
	defer srv.Close()

	f := NewFetcher(Config{}, nil)
	rt := &countingRoundTripper{next: http.DefaultTransport}
	f.SetTransport(rt)
	f.SetConfigProvider(func() (Config, error) {
		return Config{Enabled: true, AllowRedirect: true, TimeoutMs: 5000, MaxContentLength: 1 << 20}, nil
	})

	result, err := f.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	assert.Equal(t, "Hello", *result.Title)
	assert.Positive(t, rt.n.Load())
}
