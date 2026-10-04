package federation

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

// inboundMentionFetchSlots is how many inbound notes may fetch unknown
// mentioned actors at the same time, across all inbox workers.
//
// 枠が取れなかったノートは取りに行かず、DB の照合だけにする (この経路の
// 変更前の挙動)。応答しないホストへのメンションを並べたノートを送り続けても、
// 外部の応答待ちで止まる inbox の worker はこの数までになる。
const inboundMentionFetchSlots = 2

// inboundMentionFailureTTL is how long a failed fetch of a mentioned actor is
// remembered: the URI (any failure) and, for a transport failure, its host.
const inboundMentionFailureTTL = 10 * time.Minute

// inboundMentionFailureMaxEntries caps the failure memory. 溢れたら期限切れを
// 掃除し、それでも溢れていれば全部捨てる (覚えておくのは最適化に過ぎない)。
const inboundMentionFailureMaxEntries = 10000

// mentionFetchGuard bounds the unknown-actor fetches of inbound notes:
// a process-wide concurrency limit and a short memory of failures.
type mentionFetchGuard struct {
	slots chan struct{}

	mu    sync.Mutex
	uris  map[string]time.Time // 失敗した URI → 期限
	hosts map[string]time.Time // 到達できなかったホスト → 期限
}

func newMentionFetchGuard(slots int) *mentionFetchGuard {
	return &mentionFetchGuard{
		slots: make(chan struct{}, slots),
		uris:  map[string]time.Time{},
		hosts: map[string]time.Time{},
	}
}

// tryAcquire takes a slot without waiting. The returned release must be
// called once when ok is true.
func (g *mentionFetchGuard) tryAcquire() (release func(), ok bool) {
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, true
	default:
		return nil, false
	}
}

// failedRecently reports whether uri, or its host, failed within the TTL.
func (g *mentionFetchGuard) failedRecently(uri, host string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if exp, ok := g.uris[uri]; ok && now.Before(exp) {
		return true
	}
	if exp, ok := g.hosts[host]; ok && now.Before(exp) {
		return true
	}
	return false
}

// recordFailure remembers a failed fetch of uri, and of the whole host when
// unreachable is true.
func (g *mentionFetchGuard) recordFailure(uri, host string, unreachable bool, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	exp := now.Add(inboundMentionFailureTTL)
	g.uris = putBounded(g.uris, uri, exp, now)
	if unreachable {
		g.hosts = putBounded(g.hosts, host, exp, now)
	}
}

// putBounded sets m[key] = exp, pruning expired entries (and, failing that,
// everything) when m is full.
func putBounded(m map[string]time.Time, key string, exp, now time.Time) map[string]time.Time {
	if _, ok := m[key]; !ok && len(m) >= inboundMentionFailureMaxEntries {
		for k, v := range m {
			if !now.Before(v) {
				delete(m, k)
			}
		}
		if len(m) >= inboundMentionFailureMaxEntries {
			m = map[string]time.Time{}
		}
	}
	m[key] = exp
	return m
}

// isHTTPURI reports whether uri has the http or https scheme.
func isHTTPURI(uri string) bool {
	u, err := url.Parse(uri)
	if err != nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return true
	}
	return false
}

// isUnreachableHostError reports whether err from fetching an actor on host
// means host itself could not be reached: a DNS, connect or TLS failure, or a
// timeout, of a request to host itself.
//
// **広く取らない。** 1 件の Mention で正常なホストを 10 分「到達できない」と
// 覚えさせられるため。*url.Error というだけでは数えない — 未対応の scheme や
// redirect の上限も *url.Error で返る。redirect 先 (url.Error.URL が別のホスト)
// での失敗も数えない — 転送先を相手が選べるので、無関係なホストへの失敗を
// href のホストに付けられてしまう。
func isUnreachableHostError(err error, host string) bool {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return false
	}
	if h, herr := hostFromURI(urlErr.URL); herr != nil || h != host {
		return false
	}
	if urlErr.Timeout() {
		return true
	}
	var opErr *net.OpError
	var dnsErr *net.DNSError
	var recErr tls.RecordHeaderError
	var verifyErr *tls.CertificateVerificationError
	var unknownCA x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	return errors.As(urlErr.Err, &opErr) || errors.As(urlErr.Err, &dnsErr) ||
		errors.As(urlErr.Err, &recErr) || errors.As(urlErr.Err, &verifyErr) ||
		errors.As(urlErr.Err, &unknownCA) || errors.As(urlErr.Err, &hostnameErr) ||
		errors.As(urlErr.Err, &invalidCert)
}
