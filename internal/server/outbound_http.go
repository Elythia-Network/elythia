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

// newOutboundTransport builds a fresh SSRF-safe *http.Transport with the
// shared outbound options applied. allowedPrivateNetworks
// (config.AllowedPrivateNetworks) で開発時の self-loop だけを明示的に許可できる。
func (s *Server) newOutboundTransport() *http.Transport {
	return safehttp.NewSSRFSafeTransport(s.config.AllowedPrivateNetworks, s.outboundOpts()...)
}

// outboundTransport returns the process-wide SSRF-safe *http.Transport that
// every outbound client built by outboundClient shares.
//
// **用途ごとに Transport を作らない (#3591)。** Transport が接続のプールの
// 単位なので、別々に作ると同じ相手への接続を共有できず、新しいリモートの
// 利用者を解決するだけで webfinger・actor の取得・添付の確認がそれぞれ TLS の
// 握手をしていた。共有できるのは、外向きの全経路が同じ設定 (allowedPrivate
// Networks、proxy と bypass、outgoingAddress、outgoingAddressFamily、TLS は
// 既定) で作っているから。用途ごとに違うのはタイムアウトとリダイレクトの扱い
// だけで、どちらも http.Client の側に持つ。
//
// 設定の違う Transport が要る経路 (例: 管理者が指定した内向きの summaly proxy、
// OAuth の discovery の testMode) はここを通さず、自分で作ったままにする。
// 返した Transport を書き換えてはいけない (全経路に効いてしまう)。
func (s *Server) outboundTransport() *http.Transport {
	s.outboundTrOnce.Do(func() {
		tr := s.newOutboundTransport()
		// idle の接続の全体の上限を、safehttp の既定 (100) から上げる。共有した
		// ので、多数のホストへ行く経路 (media proxy、URL プレビュー、webhook)
		// と配送・AP の取得が 1 つの枠を取り合い、100 本では配送の idle の接続が
		// 古いものから追い出される。512 は、1 ホストあたり 32 本
		// (MaxIdleConnsPerHost) を、配送の worker の数 (deliver の既定 16) の
		// ホストへ同時に持てる数 (32 x 16)。idle の接続は使わなければ
		// IdleConnTimeout (90 秒) で閉じるので、常に 512 本を持つわけではない。
		tr.MaxIdleConns = outboundMaxIdleConns
		s.outboundTr = tr
	})
	return s.outboundTr
}

// outboundMaxIdleConns is the idle-connection cap of the shared outbound
// transport (see outboundTransport).
const outboundMaxIdleConns = 512

// outboundClient builds an SSRF-safe http.Client with the requested timeout
// on top of the shared outbound transport.
func (s *Server) outboundClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: s.outboundTransport(),
	}
}

// isolatedOutboundClient builds an SSRF-safe http.Client on its own
// transport, for callers that hand the client to code outside the server.
//
// プラグインに渡す client は Transport ごと外へ出る。プラグインは
// セキュリティ境界ではないが、型アサーションで Transport を書き換えられると
// 連合の配送や取得にまで効いてしまうので、共有の Transport を渡さない。
func (s *Server) isolatedOutboundClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: s.newOutboundTransport(),
	}
}

// diagnosticOutboundClient builds an SSRF-safe http.Client that dials a new
// connection for every request, for connectivity diagnosis.
//
// 疎通の診断 (#3055) が配送の idle の接続を使い回すと、相手の DNS や証明書が
// 直前に壊れていても、張ってあった接続で通って「接続できる」と出てしまう
// (#3591 のレビュー)。診断は毎回、名前の解決・SSRF の検査・TLS の握手から
// やり直す。
func (s *Server) diagnosticOutboundClient(timeout time.Duration) *http.Client {
	tr := s.newOutboundTransport()
	tr.DisableKeepAlives = true
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
	}
}
