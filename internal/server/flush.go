package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/labstack/echo/v5"
)

// flushPageHTML is the page /flush answers when it clears. The script it loads
// (frontend/assets/misc/flush.js, the same file upstream serves) clears what
// the client stores when the browser did not honor Clear-Site-Data.
const flushPageHTML = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="application-name" content="Misskey"><title>Clear preferences and cache</title></head>
<body><div id="msg"></div><script src="/static-assets/misc/flush.js"></script></body></html>
`

// flushConfirmHTML is the page /flush answers when it does not clear. It
// clears nothing and runs no script. Its form posts to /flush, which clears,
// and is sent only with the checkbox checked (required). The first %s is
// the notice of a rejected POST (flushRejectedNotice, empty for GET), the
// second the confirm token (flushNewToken), which is also set as a cookie.
//
// チェックボックスとボタンを離して置き、両方を操作しないと送れないようにする。ボタン
// だけを押させても、ブラウザが送信を止める (script を使わない HTML の検証)。label は
// 短い語だけを包み、押せる範囲を狭くする。autocomplete="off" で、履歴の移動で開き直した
// ときに印が戻らないようにする。referrer の meta は、前段が no-referrer を付ける構成でも、
// ボタンの送信に Origin が付くようにする。
const flushConfirmHTML = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="application-name" content="Misskey"><meta name="referrer" content="same-origin"><title>Clear preferences and cache</title></head>
<body>%s<form method="post" action="/flush" autocomplete="off">
<input type="hidden" name="token" value="%s">
<p><input type="checkbox" id="confirm" name="confirm" value="1" required> <label for="confirm">I agree / 同意する</label></p>
<p>Clearing removes your login and the settings stored in this browser. To clear, check the box above, then press the button below.</p>
<p>消去すると、このブラウザに保存したログインと設定が消えます。消すときは、上のチェックボックスに印を付けてから、下のボタンを押してください。</p>
<p><a href="/">Back to Misskey / 戻る</a></p>
<p><button type="submit">Clear preferences and cache / 消去する</button></p>
</form></body></html>
`

// flushRejectedNotice is shown on the confirm page answered to a POST that
// did not clear (403).
const flushRejectedNotice = `<p>The confirmation expired, or the page was opened again elsewhere. Check the box again and press the button.</p>
<p>確かめの期限が切れたか、別のページで開き直されました。もう一度チェックを入れてボタンを押してください。</p>
`

// flushConfirmCSP is the Content-Security-Policy of the confirm page.
//
// script も外部の読み込みも要らないので、すべて止める。form-action は自分のオリジンへの
// 送信 (確かめるボタン) だけを通す。frame-ancestors は、X-Frame-Options
// (middleware/frameguard.go) と同じく、別のページに埋め込ませない。
const flushConfirmCSP = "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

// flushFormMaxBytes caps the body of POST /flush. The confirm page's form
// sends `token` and `confirm=1` only.
const flushFormMaxBytes = 1 << 10

// flushTokenMaxAge is how long, in seconds, the confirm page's token is
// accepted.
const flushTokenMaxAge = 600

// flushTokenCookie returns the name of the cookie that carries the confirm
// token. https では `__Host-` を付け、サブドメインから上書きさせない (http では付けられない)。
func flushTokenCookie(siteScheme string) string {
	if siteScheme == "https" {
		return "__Host-flush-confirm"
	}
	return "flush-confirm"
}

// flushTokenCookieHeader returns the Set-Cookie value of the confirm token.
// maxAge 0 deletes it.
func flushTokenCookieHeader(siteScheme, value string, maxAge int) string {
	v := fmt.Sprintf("%s=%s; Path=/; Max-Age=%d; HttpOnly; SameSite=Strict", flushTokenCookie(siteScheme), value, maxAge)
	if siteScheme == "https" {
		v += "; Secure"
	}
	return v
}

// flushNewToken returns a random confirm token (256 bits, base64url).
func flushNewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// flushPage serves GET and POST /flush, which clear the client's stored data
// so a client stuck on stale state (a revoked token, a broken cache) can start
// over (#4204). When it clears, it answers the page upstream
// ClientServerService's /flush answers.
//
// GET always answers the confirm page, which clears nothing. Only the confirm
// page's form clears: a same-origin POST navigation with `confirm=1` and the
// token the page set as a cookie.
func flushPage(siteURL string) echo.HandlerFunc {
	// 自分とみなすのは config.URL のホストだけ。別名のホスト名で届く構成では、ボタンを押しても消えない。
	var siteHost, siteScheme string
	if u, err := url.Parse(siteURL); err == nil {
		siteHost = idnhost.HostPort(u)
		siteScheme = u.Scheme
	}
	return func(c *echo.Context) error {
		// 本家は付けないが、消去の指示と確かめるページの値を中継のキャッシュに残させない。
		c.Response().Header().Set("Cache-Control", "private, no-store")
		r := c.Request()
		if r.Method != http.MethodPost {
			// **GET では消さない。** どこから開いたか (アドレス欄、ほかのアプリ、ページの
			// リンク) によらず、確かめるページのボタンを押してもらう。
			return flushConfirm(c, http.StatusOK, siteScheme)
		}
		form, ok := flushForm(r)
		if !ok || form.Get("confirm") != "1" {
			return flushConfirm(c, http.StatusForbidden, siteScheme)
		}
		if !flushTokenMatches(r, siteScheme, form.Get("token")) || !flushPostAllowed(r, siteScheme, siteHost) {
			return flushConfirm(c, http.StatusForbidden, siteScheme)
		}
		return flushClear(c, siteScheme)
	}
}

// flushPostAllowed reports whether a POST to /flush is a navigation sent from
// this origin.
//
// Sec-Fetch-Site を送るブラウザでは same-origin だけを通す。Origin は必須で、自分の
// スキームとホストに一致することを求める (Origin はウェブページからは偽れない)。
// 部品としての読み込みや fetch (遷移でないもの) も通さない。見出しが無ければ遷移とみなす。
// 古い Service Worker が遷移を送り直すと Sec-Fetch-Mode が same-origin になるので、
// それも受ける。呼び出し元が、確かめるページの値の一致を先に求めている。
func flushPostAllowed(r *http.Request, siteScheme, siteHost string) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != siteScheme || idnhost.HostPort(u) != siteHost {
		return false
	}
	if d := r.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Mode") {
	case "", "navigate", "same-origin":
		return true
	}
	return false
}

// flushForm reads the body of POST /flush: `application/x-www-form-urlencoded`,
// no larger than flushFormMaxBytes.
func flushForm(r *http.Request) (url.Values, bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, flushFormMaxBytes+1))
	if err != nil || len(body) > flushFormMaxBytes {
		return nil, false
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, false
	}
	return form, true
}

// flushTokenMatches reports whether the token in the form equals the one in
// a confirm page's cookie. 別のサイトは確かめるページを読めないので、値を知らない。
//
// 同じ名前の Cookie が複数届くことがある (http では別のサブドメインが Path の長い同名の
// Cookie を置ける) ので、どれか1つが一致すれば通す。Cookie を保存しないブラウザでは消せない。
func flushTokenMatches(r *http.Request, siteScheme, formToken string) bool {
	if formToken == "" {
		return false
	}
	name := flushTokenCookie(siteScheme)
	matched := 0
	for _, ck := range r.Cookies() {
		if ck.Name != name || ck.Value == "" {
			continue
		}
		matched |= subtle.ConstantTimeCompare([]byte(ck.Value), []byte(formToken))
	}
	return matched == 1
}

// flushConfirm answers the confirm page, which clears nothing, with a new
// token in its form and cookie.
func flushConfirm(c *echo.Context, status int, siteScheme string) error {
	token, err := flushNewToken()
	if err != nil {
		return fmt.Errorf("flush: confirm token: %w", err)
	}
	h := c.Response().Header()
	h.Set("Content-Security-Policy", flushConfirmCSP)
	h.Add("Set-Cookie", flushTokenCookieHeader(siteScheme, token, flushTokenMaxAge))
	notice := ""
	if status == http.StatusForbidden {
		notice = flushRejectedNotice
	}
	return c.HTML(status, fmt.Sprintf(flushConfirmHTML, notice, token))
}

// flushClear answers the page that clears: Clear-Site-Data, the cookie
// flush.js reads, and the page that loads flush.js. It also deletes the
// confirm token's cookie.
//
// Clear-Site-Data は https でしか効かないので、https で配っているときだけ付ける。
// 付けられなかったときは、Cookie が残ることを flush.js が見て、localStorage と
// IndexedDB を自分で消す。確かめるページの値の Cookie は HttpOnly なので、flush.js の
// 判定 (document.cookie) には入らない。
func flushClear(c *echo.Context, siteScheme string) error {
	h := c.Response().Header()
	if siteScheme == "https" {
		h.Set("Clear-Site-Data", `"*"`)
	}
	h.Add("Set-Cookie", flushTokenCookieHeader(siteScheme, "", 0))
	h.Add("Set-Cookie", "http-flush-failed=1; Path=/flush; Max-Age=60")
	return c.HTML(http.StatusOK, flushPageHTML)
}
