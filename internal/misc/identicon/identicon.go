// Package identicon builds the URL of the identicon, the avatar a user without
// one is shown.
package identicon

import "strings"

// URL returns `<baseURL>/identicon/<username>@<host>` with the username
// lowercased, the form upstream Misskey returns (UserEntityService
// getIdenticonUrl). host is the host of the user, and for a local user the
// host of this instance (upstream's config.host, with the port if any).
// baseURL is the instance URL; an empty one gives a relative URL, and an
// empty host leaves out the `@<host>` (both only when the instance URL is not
// wired, as in tests).
//
// /identicon/ の後ろの文字列は絵の種になる。経路 (API の応答・/avatar の
// リダイレクト・ActivityPub のアイコン) で形を変えると同じ人の絵が食い違うので、
// どれもここを通す (#4095)。例外はリバーシの、配線されていないときだけ使う fallback
// (internal/core/reversi の fallbackAvatarURL) で、本番では IdenticonURL が配線される。絵の生成器は本家 (random-seed) と別物なので、種を
// そろえても絵までは本家と一致しない。
func URL(baseURL, username, host string) string {
	seed := strings.ToLower(username)
	if host != "" {
		seed += "@" + host
	}
	return strings.TrimRight(baseURL, "/") + "/identicon/" + seed
}
