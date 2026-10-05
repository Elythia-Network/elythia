package server

import (
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/safehttp"
)

// outboundOpts returns the safehttp options shared across every outbound
// HTTP client that talks to arbitrary remote hosts (AP federation, media
// proxy, URL preview, webhook delivery, Web Push, captcha siteverify, etc.).
//
// 新規 outbound HTTP client はすべてこの helper を経由させ、operator が
// config.proxy / outgoingAddress / outgoingAddressFamily を設定したときに
// origin IP 漏洩経路が無くなるようにする (#638)。
func (s *Server) outboundOpts() []safehttp.Option {
	return []safehttp.Option{
		safehttp.WithProxy(s.config.Proxy, s.config.ProxyBypassHosts),
		safehttp.WithOutgoingAddress(s.config.OutgoingAddress),
		safehttp.WithAddressFamily(s.config.OutgoingAddressFamily),
	}
}

// outboundTransport returns an SSRF-safe *http.Transport with the shared
// outbound options applied. allowedPrivateNetworks (config.AllowedPrivateNetworks)
// で開発時の self-loop だけを明示的に許可できる。
func (s *Server) outboundTransport() *http.Transport {
	return safehttp.NewSSRFSafeTransport(s.config.AllowedPrivateNetworks, s.outboundOpts()...)
}

// outboundClient builds an SSRF-safe http.Client with the requested timeout.
func (s *Server) outboundClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: s.outboundTransport(),
	}
}
