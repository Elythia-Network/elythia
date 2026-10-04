package middleware

import (
	"strings"

	"github.com/labstack/echo/v4"
)

// hstsValue mirrors upstream ServerService exactly (6 months + preload).
//
// **`includeSubDomains` は付けない。** upstream が付けていないのに mk-go だけ
// 付けると、同じドメインの別サブドメインを平文で運用している構成を切替の瞬間に
// 壊す。値を揃えておけば TS から切り替えたときに挙動が変わらない。
const hstsValue = "max-age=15552000; preload"

// HSTS returns a GLOBAL middleware that sets `Strict-Transport-Security`.
//
// upstream ServerService は `config.url` が https で `disableHsts` が偽のときに
// 同じ header を付ける。**mk-go は disableHsts を設定として読んでいたのに header
// を出していなかった** ので、TS から切り替えると HSTS が黙って消えていた。
//
// https でないときは付けない。平文で配っている構成に付けると、ブラウザが以後
// その host を https でしか開かなくなり、**設定を戻しても max-age の間は復旧
// できない**。
func HSTS(serverURL string, disabled bool) echo.MiddlewareFunc {
	enabled := !disabled && strings.HasPrefix(strings.ToLower(serverURL), "https://")
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if enabled {
				c.Response().Header().Set("Strict-Transport-Security", hstsValue)
			}
			return next(c)
		}
	}
}

// Cross-Origin-Opener-Policy values accepted by the `crossOriginOpenerPolicy`
// config key.
const (
	// COOPOff sends no header. 既定。
	COOPOff = "off"
	// COOPSameOriginAllowPopups keeps the opener link for popups this page
	// opens, but cuts it for anything that opened this page.
	COOPSameOriginAllowPopups = "same-origin-allow-popups"
	// COOPSameOrigin cuts the opener link in both directions.
	COOPSameOrigin = "same-origin"
)

// COOP returns a GLOBAL middleware that sets `Cross-Origin-Opener-Policy`.
//
// **upstream は既定で出さない** (`enableCrossOriginIsolation` を立てたテスト
// 構成でだけ `same-origin` を出す)。mk-go は運用者が選べる形で足す。
//
// 既定が off なのは、**外部アプリが認証ページをポップアップで開いて閉じるのを
// 待つ**形の連携を切りうるため。MiAuth / OAuth は callbackUrl へのリダイレクトで
// 完結する設計なので通常は問題にならないが、切れたときの症状 (「認証したのに
// アプリが気づかない」) から原因に辿り着くのが難しい。
//
// frontend 自身は `window.open` を全て `noopener` で呼んでいるので、こちら側が
// opener を必要とすることは無い。
func COOP(mode string) echo.MiddlewareFunc {
	value := ""
	switch mode {
	case COOPSameOriginAllowPopups, COOPSameOrigin:
		value = mode
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if value != "" {
				c.Response().Header().Set("Cross-Origin-Opener-Policy", value)
			}
			return next(c)
		}
	}
}

// ValidCOOPMode reports whether mode is one this package understands.
func ValidCOOPMode(mode string) bool {
	switch mode {
	case "", COOPOff, COOPSameOriginAllowPopups, COOPSameOrigin:
		return true
	}
	return false
}
