// Package remoteurl checks the form of media URLs that a remote server chose,
// such as the avatar and banner of a remote user.
package remoteurl

import (
	"net/url"
	"strings"
)

// WellFormedMedia reports whether raw is an absolute http or https URL with a
// host and without characters that could end a CSS url() token or an HTML
// attribute.
//
// 相手が決めた URL は、画面で img の src や CSS の url() に入る。メディアプロキシは
// host を持つ絶対 URL しか包まないので、形の崩れた値は包まれずにそのまま出てしまう。
// この形のものだけを通す。制御文字は url.Parse がエラーにする。
func WellFormedMedia(raw string) bool {
	if raw == "" || strings.ContainsAny(raw, "()'\"\\<> ") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
