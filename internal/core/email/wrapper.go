// Package email provides shared helpers for outbound email body composition.
//
// WrapHTML / Template defines a Misskey TS 互換の HTML wrapper for transactional
// emails (signup confirm, password reset, ...). Layout matches upstream
// EmailService.sendEmail ── header (logo / site banner color), article (title +
// caller-supplied body HTML), footer (link to email settings).
package email

import (
	"fmt"
	"html"
	"strings"

	"github.com/elythia-network/elythia/internal/config"
)

// HTMLWrapInput captures everything WrapHTML needs to build the outer email
// shell. caller (signup / reset-password 等) は本文 HTML だけ作って渡す。
type HTMLWrapInput struct {
	// SiteName は header / footer に表示するインスタンス名。空なら config.DisplayName。
	SiteName string
	// SiteURL は footer の bottom link 先 (instance トップ)。空なら link 省略。
	SiteURL string
	// LogoURL は header に表示するロゴ画像の URL。空なら省略。
	LogoURL string
	// Subject は <title> と article 内 <h1> に流す件名。escape 済前提ではなく、
	// WrapHTML 内で html.EscapeString する。
	Subject string
	// BodyHTML は article 内の <h1> の下に流し込む本文 HTML。caller が CTA
	// link 等を組んで渡す。改行は <br/> または <p> で表現する想定で、
	// WrapHTML は escape しない (caller responsibility)。
	BodyHTML string
	// EmailSettingsURL は <main> 内 <footer> に置く「メール設定」link の宛先。
	// 認証済 user 向けメール (reset-password 等) で設定。signup-pending のような
	// 未認証ユーザー向けメールでは空にして footer 自体省略 (TS と同じ運用)。
	EmailSettingsURL string
	// EmailSettingsLabel は footer link の表示文言。l10n 済み文字列を渡す。
	// 空なら "Email setting" (英語 fallback)。
	EmailSettingsLabel string
}

// WrapHTML returns an Misskey TS-style HTML email body suitable for the html
// part of a multipart/alternative message. Inline CSS のみで MUA / Gmail web
// view どちらでもレイアウトが崩れない最小限のテンプレート。
//
// Misskey TS の EmailService.sendEmail と視覚的に揃えてあるので、TS から
// mk-go に切り替えても受信側 UX が変わらない (#600 item 4)。
func WrapHTML(in HTMLWrapInput) string {
	siteName := in.SiteName
	if siteName == "" {
		siteName = config.DisplayName
	}
	subject := html.EscapeString(in.Subject)
	siteNameEsc := html.EscapeString(siteName)

	var header strings.Builder
	if in.LogoURL != "" {
		fmt.Fprintf(&header, `<img src="%s" alt="%s" style="max-width:128px;max-height:28px;vertical-align:bottom" />`,
			html.EscapeString(in.LogoURL), siteNameEsc)
	} else {
		// ロゴが無いインスタンスでは site name をそのままヘッダーに置く。
		fmt.Fprintf(&header, `<span style="color:#fff;font-weight:bold;font-size:16px">%s</span>`, siteNameEsc)
	}

	var nav strings.Builder
	if in.SiteURL != "" {
		fmt.Fprintf(&nav, `<a href="%s" style="color:#888;text-decoration:none">%s</a>`,
			html.EscapeString(in.SiteURL), siteNameEsc)
	}

	// EmailSettingsURL がある場合のみ <main> 内 <footer> を生成する (TS の
	// 二段構造に揃える)。空なら footer ごと省略してすっきり見せる。
	var innerFooter string
	if in.EmailSettingsURL != "" {
		label := in.EmailSettingsLabel
		if label == "" {
			label = "Email setting"
		}
		innerFooter = fmt.Sprintf(
			`<footer style="padding:32px;border-top:solid 1px #eee"><a href="%s" style="color:#86b300;text-decoration:none">%s</a></footer>`,
			html.EscapeString(in.EmailSettingsURL), html.EscapeString(label))
	}

	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><title>%s</title></head>
<body style="background:#eee;padding:16px;margin:0;font-family:sans-serif;font-size:14px">
<main style="max-width:500px;margin:0 auto;background:#fff;color:#555">
  <header style="padding:32px;background:#86b300">%s</header>
  <article style="padding:32px"><h1 style="margin:0 0 1em 0">%s</h1>%s</article>
  %s</main>
<nav style="max-width:500px;margin:16px auto 0 auto;padding:0 32px">%s</nav>
</body></html>`,
		subject, header.String(), subject, in.BodyHTML, innerFooter, nav.String())
}

// PlainText is the no-CTA counterpart of LinkText, for emails whose body is
// just a sentence (例: new-login notification)。plain-text 版はそのまま、HTML 版は
// escape して段落に包んだものを返す。
//
// LinkText を link 無しで流用すると空の <a> が残るので分けている。
func PlainText(body string) (text, htmlBody string) {
	return body, fmt.Sprintf(`<p>%s</p>`, html.EscapeString(body))
}

// LinkText is a convenience for callers who want a single-CTA email body. It
// returns both a plain-text version and a basic HTML body that can be passed
// to WrapHTML as `BodyHTML`. lead 文と link を渡せば自動的に最低限の HTML
// マークアップ + escape を付ける。
func LinkText(lead, linkLabel, linkURL string) (text, htmlBody string) {
	text = lead + "\n" + linkURL
	htmlBody = fmt.Sprintf(`<p>%s</p><p><a href="%s" style="color:#86b300">%s</a></p>`,
		html.EscapeString(lead), html.EscapeString(linkURL), html.EscapeString(linkLabel))
	return
}
