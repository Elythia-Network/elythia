package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/safehttp"
	"github.com/elythia-network/elythia/plugin"
)

func TestOutboundClient_SharesOneTransportAcrossTimeouts(t *testing.T) {
	s := &Server{config: &config.Config{}}

	ap := s.outboundClient(30 * time.Second)
	probe := s.outboundClient(10 * time.Second)

	assert.Same(t, ap.Transport, probe.Transport, "clients built by outboundClient share one connection pool")
	assert.Same(t, s.outboundTransport(), ap.Transport)
	assert.Equal(t, 30*time.Second, ap.Timeout)
	assert.Equal(t, 10*time.Second, probe.Timeout)

	tr, ok := ap.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, 32, tr.MaxIdleConnsPerHost, "the shared transport keeps the per-host idle pool from safehttp")
	assert.Equal(t, 512, tr.MaxIdleConns, "every outbound path shares one idle pool, so its total cap is raised")
	assert.Equal(t, 100, s.newOutboundTransport().MaxIdleConns, "transports built for a single caller keep the safehttp default")
}

func TestIsolatedOutboundClient_DoesNotShareTheTransport(t *testing.T) {
	s := &Server{config: &config.Config{}}

	shared := s.outboundClient(30 * time.Second)
	a := s.isolatedOutboundClient(5 * time.Second)
	b := s.isolatedOutboundClient(5 * time.Second)

	assert.NotSame(t, shared.Transport, a.Transport)
	assert.NotSame(t, a.Transport, b.Transport)
	assert.Equal(t, 5*time.Second, a.Timeout)
}

// 共有した Transport でも、接続を使い回した後でも、allowedPrivateNetworks の外の
// private な宛先は拒否される (#3591)。
func TestOutboundClient_SharedTransportKeepsSSRFGuard(t *testing.T) {
	var newConns int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			atomic.AddInt64(&newConns, 1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)

	s := &Server{config: &config.Config{AllowedPrivateNetworks: []string{"127.0.0.1/32"}}}
	t.Cleanup(func() { s.outboundTransport().CloseIdleConnections() })

	get := func(c *http.Client, host string) error {
		resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/")
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}

	require.NoError(t, get(s.outboundClient(30*time.Second), "127.0.0.1"))
	require.NoError(t, get(s.outboundClient(10*time.Second), "127.0.0.1"))
	assert.Equal(t, int64(1), atomic.LoadInt64(&newConns), "the second client reuses the first client's connection")

	// 127.0.0.2 は loopback だが allowedPrivateNetworks の外。
	for _, timeout := range []time.Duration{30 * time.Second, 10 * time.Second} {
		err := get(s.outboundClient(timeout), "127.0.0.2")
		require.Error(t, err)
		assert.ErrorIs(t, err, safehttp.ErrSSRFBlocked)
	}
	assert.Equal(t, int64(1), atomic.LoadInt64(&newConns))
}

func TestDiagnosticOutboundClient_DisablesKeepAlives(t *testing.T) {
	s := &Server{config: &config.Config{}}
	c := s.diagnosticOutboundClient(7 * time.Second)

	tr, ok := c.Transport.(*http.Transport)
	require.True(t, ok)
	assert.True(t, tr.DisableKeepAlives)
	assert.NotSame(t, s.outboundTransport(), tr)
	assert.Equal(t, 7*time.Second, c.Timeout)
}

// プラグインに渡す client は共有の Transport を持たない (#3591)。Transport ごと
// プラグインのコードへ出るので、書き換えられると連合の経路にまで効く。
func TestSetupPlugins_PluginHTTPClientDoesNotShareTheOutboundTransport(t *testing.T) {
	s, api := newPluginTestServer(config.RoleServer)
	var got *http.Client
	def := pluginDef("p", func(ctx plugin.Context, _ plugin.Router) error { got = ctx.HTTP(); return nil }, nil)
	require.NoError(t, s.setupPlugins(api, []plugin.Definition{def}, noopStorage))
	require.NotNil(t, got)

	tr, ok := got.Transport.(*http.Transport)
	require.True(t, ok, "the plugin client is wired with an SSRF-safe transport, not the unwired fallback")
	assert.NotSame(t, s.outboundTransport(), tr)
	assert.Equal(t, pluginHTTPTimeout, got.Timeout)
}
