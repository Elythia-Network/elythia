package server

import (
	"net/http"
	"net/url"

	"github.com/elythia-network/elythia/internal/misc/idnhost"
	"github.com/elythia-network/elythia/internal/misc/secfetch"
	"github.com/labstack/echo/v5"
)

// flushPageHTML is the page served at /flush. The script it loads
// (frontend/assets/misc/flush.js, the same file upstream serves) clears what
// the client stores when the browser did not honor Clear-Site-Data.
const flushPageHTML = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="application-name" content="Misskey"><title>Clear preferences and cache</title></head>
<body><div id="msg"></div><script src="/static-assets/misc/flush.js"></script></body></html>
`

// flushConfirmHTML is the page /flush answers instead when another site sent
// the browser there. It clears nothing; its link is a same-origin navigation
// back to /flush.
const flushConfirmHTML = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><meta name="application-name" content="Misskey"><title>Clear preferences and cache</title></head>
<body><p>Another site sent you here. Clearing removes your login and the settings stored in this browser.</p>
<p>別のサイトからこのページに移動しました。消去すると、このブラウザに保存したログインと設定が消えます。</p>
<p><a href="/flush">Clear preferences and cache / 消去する</a></p>
<p><a href="/">Back to Misskey / 戻る</a></p></body></html>
`

// flushPage serves /flush, which clears the client's stored data so a client
// stuck on stale state (a revoked token, a broken cache) can start over
// (#4204). It mirrors upstream ClientServerService's /flush.
//
// 本家と同じく、Origin が付いた要求では、https かつ自分と同じホストのときだけ
// Clear-Site-Data を付ける (Clear-Site-Data は https でしか効かない)。Origin を
// 読めないときも付けない (本家は new URL() が投げて 500 になる)。付けられなかった
// ときは、Cookie が残ることを flush.js が見て、localStorage と IndexedDB を自分で
// 消す。ホストは大文字・既定のポート・punycode をそろえてから比べる。
func flushPage(siteURL string) echo.HandlerFunc {
	var siteHost string
	if u, err := url.Parse(siteURL); err == nil {
		siteHost = idnhost.HostPort(u)
	}
	return func(c *echo.Context) error {
		h := c.Response().Header()
		// 本家は付けないが、消去の指示を中継のキャッシュに残させない。
		h.Set("Cache-Control", "private, no-store")
		// **別のサイトから遷移させられたときは、消さずに確かめる。** 本家は確かめずに消すので、
		// 別のサイトのリンクやスクリプトで遷移させるだけで、ログインと端末の設定を消させられる
		// (トップレベルの遷移には Origin が付かないので、下の判定を素通りする)。
		if secfetch.FromOtherSite(c.Request()) {
			return c.HTML(http.StatusOK, flushConfirmHTML)
		}
		send := true
		if origin := c.Request().Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "https" || idnhost.HostPort(u) != siteHost {
				send = false
			}
		}
		if send {
			h.Set("Clear-Site-Data", `"*"`)
		}
		h.Add("Set-Cookie", "http-flush-failed=1; Path=/flush; Max-Age=60")
		return c.HTML(http.StatusOK, flushPageHTML)
	}
}
