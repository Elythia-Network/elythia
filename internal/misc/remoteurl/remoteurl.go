// Package remoteurl checks the form of media URLs that a remote server chose,
// such as the avatar and banner of a remote user.
package remoteurl

import (
	"net/url"
	"strings"
)

// encoder percent-encodes the characters that are legal in a URL a remote
// server may send but could end a CSS url() token or an HTML attribute.
var encoder = strings.NewReplacer("(", "%28", ")", "%29", "'", "%27", `"`, "%22", " ", "%20")

// Media returns raw with the characters that could end a CSS url() token or an
// HTML attribute percent-encoded, and whether the result is an absolute http
// or https URL with a host. Callers use the returned string, not raw.
//
// 相手が決めた URL は、画面で img の src や CSS の url() に入る。メディアプロキシは
// host を持つ絶対 URL しか包まないので、形の崩れた値は包まれずにそのまま出てしまう。
// この形のものだけを通す。括弧や空白は正規の URL にも現れる (元のファイル名を
// 残すサーバーがある) ので、捨てずに符号化する。符号化しても同じ資源を指す。
// バックスラッシュと山括弧は、ブラウザと url.Parse で解釈が分かれうるので拒否する。
// 制御文字は url.Parse がエラーにする。
func Media(raw string) (string, bool) {
	if raw == "" || strings.ContainsAny(raw, "\\<>") {
		return "", false
	}
	clean := encoder.Replace(raw)
	u, err := url.Parse(clean)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	return clean, true
}
