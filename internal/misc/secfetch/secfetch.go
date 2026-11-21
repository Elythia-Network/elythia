// Package secfetch reads the Fetch Metadata request headers (Sec-Fetch-*)
// browsers send, for endpoints that act on what the browser stores and must
// not be triggered by another site.
package secfetch

import "net/http"

// FromOtherSite reports whether the browser says another site started the
// request (Sec-Fetch-Site is cross-site or same-site).
//
// same-site (同じ登録ドメインの別のサブドメイン) も別のサイトとして扱う。サブドメインは
// 別の人が持っていることがある。ヘッダーが無いとき (古いブラウザ、ブラウザ以外) は
// 分からないので false にする。断るのではなく、本家と同じ挙動に倒す。
func FromOtherSite(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		return true
	}
	return false
}
