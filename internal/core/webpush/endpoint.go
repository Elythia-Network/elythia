package webpush

import "net/url"

// IsValidEndpoint reports whether endpoint is acceptable as a Web Push
// subscription endpoint. It mirrors upstream
// PushNotificationService.isValidEndpoint: the URL must parse, use the https
// scheme, and carry no userinfo. It is used both when a subscription is
// registered (sw/register) and when a stored subscription is delivered to.
func IsValidEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	// url.Parse は scheme を小文字に正規化するので、本家の
	// `url.protocol !== 'https:'` と同じく大文字小文字を区別しない比較になる。
	if u.Scheme != "https" {
		return false
	}
	// 本家は WHATWG URL の username / password が空かどうかを見る。Go では
	// `https://@host/` のような空の userinfo も User が非 nil になるので、
	// userinfo の部分があるだけで断る (本家より少し厳しいが、正規の push
	// service がこの形を返すことはない)。
	if u.User != nil {
		return false
	}
	// WHATWG URL は `https:host` を `https://host/` に補正するが、Go の
	// url.Parse は Opaque として扱い Host が空になる。配送側 (webpush-go) は
	// Go の解釈で送るので、Go の解釈でホスト名が取れない形 (`https:host` や
	// `https://:443/`) は断る。
	if u.Hostname() == "" {
		return false
	}
	return true
}
