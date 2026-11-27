package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	flushFormType  = "application/x-www-form-urlencoded"
	flushTestToken = "dGVzdC10b2tlbi10aGF0LWlzLWxvbmctZW5vdWdoLTEyMzQ"
	// flushTestBody is the form the confirm page sends with the box checked.
	flushTestBody = "token=" + flushTestToken + "&confirm=1"
)

// flushButtonHeaders are the headers a browser sends when the confirm page's
// button is pressed on a site served at site, from origin, with the confirm
// page's cookie.
func flushButtonHeaders(site, origin string) map[string]string {
	scheme, _, _ := strings.Cut(site, "://")
	return map[string]string{
		"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document",
		"Origin": origin, "Content-Type": flushFormType,
		"Cookie": flushTokenCookie(scheme) + "=" + flushTestToken,
	}
}

// serveFlushRequest sends one request to /flush of a server at site and
// returns the response. headers with an empty value are not sent.
func serveFlushRequest(t *testing.T, site, method string, headers map[string]string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/flush", strings.NewReader(body))
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	require.NoError(t, flushPage(site)(echo.New().NewContext(req, rec)))
	return rec
}

// assertFlushClears checks rec is the page that clears, and that it deletes
// the confirm token's cookie.
func assertFlushClears(t *testing.T, rec *httptest.ResponseRecorder, msg string) {
	t.Helper()
	assert.Equal(t, http.StatusOK, rec.Code, msg)
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"), msg)
	cookies := rec.Header().Values("Set-Cookie")
	assert.Contains(t, cookies, "http-flush-failed=1; Path=/flush; Max-Age=60", msg)
	assert.Len(t, cookies, 2, msg)
	var deleted bool
	for _, ck := range cookies {
		if strings.HasPrefix(ck, "__Host-flush-confirm=;") || strings.HasPrefix(ck, "flush-confirm=;") {
			deleted = strings.Contains(ck, "Max-Age=0") && strings.Contains(ck, "Path=/;")
		}
	}
	assert.True(t, deleted, "the confirm token's cookie is not deleted: %v (%s)", cookies, msg)
	assert.Contains(t, rec.Body.String(), `<script src="/static-assets/misc/flush.js"></script>`, msg)
	assert.NotContains(t, rec.Body.String(), "<form", msg)
}

var flushHiddenToken = regexp.MustCompile(`<input type="hidden" name="token" value="([A-Za-z0-9_-]+)">`)

// assertFlushAsks checks rec is the confirm page, which clears nothing, and
// returns the token in its form, which equals the one in its cookie.
func assertFlushAsks(t *testing.T, rec *httptest.ResponseRecorder, status int, msg string) string {
	t.Helper()
	assert.Equal(t, status, rec.Code, msg)
	assert.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"), msg)
	assert.Empty(t, rec.Header().Get("Clear-Site-Data"), msg)
	body := rec.Body.String()
	assert.NotContains(t, body, "flush.js", msg)
	assert.NotContains(t, body, "<script", msg)
	assert.Contains(t, body, `<meta name="referrer" content="same-origin">`, msg)
	assert.Contains(t, body, `<form method="post" action="/flush" autocomplete="off">`, msg)
	assert.Contains(t, body, `<input type="checkbox" id="confirm" name="confirm" value="1" required> <label for="confirm">I agree / 同意する</label>`, msg)
	assert.Contains(t, body, `<button type="submit">`, msg)
	assert.NotContains(t, body, `<a href="/flush">`, msg)
	// 断った理由の文は、POST を断ったとき (403) だけに出す。
	for _, line := range []string{
		"The confirmation expired, or the page was opened again elsewhere. Check the box again and press the button.",
		"確かめの期限が切れたか、別のページで開き直されました。もう一度チェックを入れてボタンを押してください。",
	} {
		if status == http.StatusForbidden {
			assert.Contains(t, body, line, msg)
		} else {
			assert.NotContains(t, body, line, msg)
		}
	}
	assert.Equal(t, "default-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
		rec.Header().Get("Content-Security-Policy"), msg)

	m := flushHiddenToken.FindStringSubmatch(body)
	require.Len(t, m, 2, "no token in the form (%s)", msg)
	token := m[1]
	assert.Len(t, token, 43, msg)
	cookies := rec.Header().Values("Set-Cookie")
	require.Len(t, cookies, 1, msg)
	assert.NotContains(t, cookies[0], "http-flush-failed", msg)
	assert.True(t, strings.HasPrefix(cookies[0], "__Host-flush-confirm="+token+";") || strings.HasPrefix(cookies[0], "flush-confirm="+token+";"),
		"cookie %q, token %q (%s)", cookies[0], token, msg)
	return token
}

// The confirm page's token cookie: on https it has the __Host- prefix and is
// Secure; on http it has neither. Both are HttpOnly, SameSite=Strict, Path=/
// and short-lived. Each page gets a new token.
func TestFlushConfirmPage_TokenCookie(t *testing.T) {
	rec := serveFlushRequest(t, "https://example.test", http.MethodGet, nil, "")
	token := assertFlushAsks(t, rec, http.StatusOK, "https")
	assert.Equal(t, "__Host-flush-confirm="+token+"; Path=/; Max-Age=600; HttpOnly; SameSite=Strict; Secure", rec.Header().Get("Set-Cookie"))

	again := assertFlushAsks(t, serveFlushRequest(t, "https://example.test", http.MethodGet, nil, ""), http.StatusOK, "https again")
	assert.NotEqual(t, token, again)

	rec = serveFlushRequest(t, "http://192.0.2.1:3000", http.MethodGet, nil, "")
	token = assertFlushAsks(t, rec, http.StatusOK, "http")
	assert.Equal(t, "flush-confirm="+token+"; Path=/; Max-Age=600; HttpOnly; SameSite=Strict", rec.Header().Get("Set-Cookie"))
}

// The confirm page's checkbox and button are apart: the warning sits between
// them, so one click on the button's place does not also check the box. The
// label wraps a short phrase only.
func TestFlushConfirmPage_CheckboxIsApartFromTheButton(t *testing.T) {
	box := strings.Index(flushConfirmHTML, `type="checkbox"`)
	button := strings.Index(flushConfirmHTML, `<button`)
	require.Positive(t, box)
	require.Positive(t, button)
	between := flushConfirmHTML[box:button]
	assert.Contains(t, between, "</label></p>")
	assert.GreaterOrEqual(t, strings.Count(between, "<p>"), 2)
	assert.Equal(t, 1, strings.Count(flushConfirmHTML, "<label"))
}

// /flush は、消すときは本家と同じく、Clear-Site-Data と、それが効かなかったときに
// flush.js が見る Cookie と、flush.js を読むページを返す (#4204)。
func TestFlushPage(t *testing.T) {
	rec := serveFlushRequest(t, "https://example.test", http.MethodPost, flushButtonHeaders("https://example.test", "https://example.test"), flushTestBody)
	assertFlushClears(t, rec, "")
	assert.Equal(t, `"*"`, rec.Header().Get("Clear-Site-Data"))
	assert.Contains(t, rec.Header().Values("Set-Cookie"), "__Host-flush-confirm=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict; Secure")
	assert.True(t, strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html"))
	assert.Contains(t, rec.Body.String(), `<div id="msg"></div>`)
	assert.Empty(t, rec.Header().Get("Content-Security-Policy"))
}

// 登録されていないと、/flush は SPA catchall の通常の画面になって何も消えない
// (起動の失敗の画面 (public/loader/boot.js) のリンクも効かない)。POST が無いと、
// 確かめるページのボタンを押しても消えない。
func TestFlushPage_RegisteredOnBuiltServer(t *testing.T) {
	routes, _ := builtEchoRoutes(t)
	found := map[string]bool{}
	for _, r := range routes {
		if r.Path == "/flush" && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
			assert.Contains(t, r.Name, "flushPage", r.Method)
			found[r.Method] = true
		}
	}
	assert.True(t, found[http.MethodGet], "GET /flush is not registered")
	assert.True(t, found[http.MethodPost], "POST /flush is not registered")
}

// Through the real router (its middleware included), GET /flush answers the
// confirm page, which cannot be framed, and the button's POST, with the
// page's token, reaches the handler with its form body and clears.
func TestFlushPage_ThroughTheBuiltRouter(t *testing.T) {
	srv := newServerWithTrustProxy(t, nil)

	req := httptest.NewRequest(http.MethodGet, "/flush", nil)
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	token := assertFlushAsks(t, rec, http.StatusOK, "GET")
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))

	req = httptest.NewRequest(http.MethodPost, "/flush", strings.NewReader("token="+token+"&confirm=1"))
	for k, v := range flushButtonHeaders("http://example.test", "http://example.test") {
		req.Header.Set(k, v)
	}
	req.Header.Set("Cookie", "flush-confirm="+token)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assertFlushClears(t, rec, "POST")
}

// GET /flush never clears, wherever it was opened from (the address bar, a
// link inside Elythia, another site or app, a browser without Fetch
// Metadata): it answers the confirm page.
func TestFlushPage_GetNeverClears(t *testing.T) {
	for _, value := range []string{"none", "same-origin", "cross-site", "same-site", "", "bogus"} {
		for _, origin := range []string{"", "https://example.test"} {
			rec := serveFlushRequest(t, "https://example.test", http.MethodGet, map[string]string{
				"Sec-Fetch-Site": value, "Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Origin": origin,
			}, "")
			assertFlushAsks(t, rec, http.StatusOK, "Sec-Fetch-Site "+value+" Origin "+origin)
		}
	}
	// 本文と Cookie がそろっていても、GET では消さない。
	h := flushButtonHeaders("https://example.test", "https://example.test")
	rec := serveFlushRequest(t, "https://example.test", http.MethodGet, h, flushTestBody)
	assertFlushAsks(t, rec, http.StatusOK, "GET with the form")
}

// POST /flush clears only for a navigation sent from this origin. Anything
// else gets the confirm page with 403, and clears nothing.
func TestFlushPage_PostClearsOnlyFromThisOrigin(t *testing.T) {
	const self = "https://example.test"
	cases := []struct {
		name                     string
		site, origin, dest, mode string
		clears                   bool
	}{
		{"same-origin navigation", "same-origin", self, "document", "navigate", true},
		{"same-origin with a spelled own origin", "same-origin", "https://Example.TEST:443", "document", "navigate", true},
		{"no Fetch Metadata with own Origin", "", self, "", "", true},
		{"same-origin dest only", "same-origin", self, "document", "", true},
		{"same-origin mode only", "same-origin", self, "", "navigate", true},
		// 古い Service Worker が遷移を送り直した形。
		{"resent by an old Service Worker", "same-origin", self, "document", "same-origin", true},
		{"resent by an old Service Worker without dest", "same-origin", self, "", "same-origin", true},
		{"same-origin without Origin", "same-origin", "", "document", "navigate", false},
		{"cross-site", "cross-site", "https://other.example", "document", "navigate", false},
		{"cross-site with own Origin", "cross-site", self, "document", "navigate", false},
		{"cross-site resent by an old Service Worker", "cross-site", self, "document", "same-origin", false},
		{"same-site", "same-site", "https://sub.example.test", "document", "navigate", false},
		{"none", "none", self, "document", "navigate", false},
		{"unknown Sec-Fetch-Site", "bogus", self, "document", "navigate", false},
		{"same-origin with another Origin", "same-origin", "https://other.example", "document", "navigate", false},
		{"same-origin with another port", "same-origin", "https://example.test:8443", "document", "navigate", false},
		{"same-origin with Origin null", "same-origin", "null", "document", "navigate", false},
		{"same-origin with http Origin", "same-origin", "http://example.test", "document", "navigate", false},
		{"same-origin with a broken Origin", "same-origin", "://broken", "document", "navigate", false},
		{"no Fetch Metadata with another Origin", "", "https://other.example", "", "", false},
		{"no Fetch Metadata with Origin null", "", "null", "", "", false},
		{"no Fetch Metadata and no Origin", "", "", "", "", false},
		{"same-origin fetch", "same-origin", self, "empty", "cors", false},
		{"same-origin no-cors", "same-origin", self, "empty", "no-cors", false},
		{"same-origin iframe", "same-origin", self, "iframe", "navigate", false},
		{"same-origin fetch in same-origin mode", "same-origin", self, "empty", "same-origin", false},
		{"same-origin image", "same-origin", self, "image", "", false},
		{"same-origin cors without dest", "same-origin", self, "", "cors", false},
	}
	for _, tc := range cases {
		h := flushButtonHeaders(self, tc.origin)
		h["Sec-Fetch-Site"], h["Sec-Fetch-Dest"], h["Sec-Fetch-Mode"] = tc.site, tc.dest, tc.mode
		rec := serveFlushRequest(t, self, http.MethodPost, h, flushTestBody)
		if tc.clears {
			assertFlushClears(t, rec, tc.name)
			assert.Equal(t, `"*"`, rec.Header().Get("Clear-Site-Data"), tc.name)
		} else {
			assertFlushAsks(t, rec, http.StatusForbidden, tc.name)
		}
	}
}

// POST /flush clears only when the form's token equals the confirm page's
// cookie, also for a request resent by an old Service Worker.
func TestFlushPage_PostNeedsTheToken(t *testing.T) {
	const self = "https://example.test"
	cases := []struct {
		name, cookie, body string
		clears             bool
	}{
		{"matching", "__Host-flush-confirm=" + flushTestToken, flushTestBody, true},
		{"another token in the form", "__Host-flush-confirm=" + flushTestToken, "token=" + flushTestToken + "x&confirm=1", false},
		{"another token in the cookie", "__Host-flush-confirm=x" + flushTestToken, flushTestBody, false},
		{"a prefix of the token", "__Host-flush-confirm=" + flushTestToken, "token=" + flushTestToken[:10] + "&confirm=1", false},
		{"no cookie", "", flushTestBody, false},
		{"another cookie", "other=" + flushTestToken, flushTestBody, false},
		{"the http cookie on https", "flush-confirm=" + flushTestToken, flushTestBody, false},
		{"no token in the form", "__Host-flush-confirm=" + flushTestToken, "confirm=1", false},
		{"empty token in both", "__Host-flush-confirm=", "token=&confirm=1", false},
		// 同じ名前の Cookie が複数届いたときは、どれか1つが一致すれば通す。
		{"another cookie of the same name first", "__Host-flush-confirm=junk; __Host-flush-confirm=" + flushTestToken, flushTestBody, true},
		{"another cookie of the same name last", "__Host-flush-confirm=" + flushTestToken + "; __Host-flush-confirm=junk", flushTestBody, true},
		{"an empty cookie of the same name first", "__Host-flush-confirm=; __Host-flush-confirm=" + flushTestToken, flushTestBody, true},
		{"no cookie of the same name matches", "__Host-flush-confirm=junk; __Host-flush-confirm=x" + flushTestToken, flushTestBody, false},
	}
	for _, mode := range []string{"navigate", "same-origin"} {
		for _, tc := range cases {
			h := flushButtonHeaders(self, self)
			h["Cookie"], h["Sec-Fetch-Mode"] = tc.cookie, mode
			rec := serveFlushRequest(t, self, http.MethodPost, h, tc.body)
			if tc.clears {
				assertFlushClears(t, rec, tc.name+" "+mode)
			} else {
				assertFlushAsks(t, rec, http.StatusForbidden, tc.name+" "+mode)
			}
		}
	}
}

// POST /flush clears only when the body is the confirm page's form with the
// checkbox checked.
func TestFlushPage_PostNeedsTheCheckbox(t *testing.T) {
	const self = "https://example.test"
	tok := "token=" + flushTestToken
	cases := []struct {
		name, contentType, body string
		clears                  bool
	}{
		{"checked", flushFormType, flushTestBody, true},
		{"checked with a charset", flushFormType + "; charset=UTF-8", flushTestBody, true},
		{"checked among other fields", flushFormType, "x=y&" + flushTestBody, true},
		{"unchecked", flushFormType, tok, false},
		{"empty body", flushFormType, "", false},
		{"another value", flushFormType, tok + "&confirm=on", false},
		{"empty value", flushFormType, tok + "&confirm=", false},
		{"another field", flushFormType, tok + "&confirmed=1", false},
		{"broken form", flushFormType, flushTestBody + "&%zz", false},
		{"multipart", "multipart/form-data; boundary=x", "--x\r\nContent-Disposition: form-data; name=\"confirm\"\r\n\r\n1\r\n--x\r\nContent-Disposition: form-data; name=\"token\"\r\n\r\n" + flushTestToken + "\r\n--x--\r\n", false},
		{"text/plain", "text/plain", flushTestBody, false},
		{"JSON", "application/json", `{"confirm":"1","token":"` + flushTestToken + `"}`, false},
		{"no Content-Type", "", flushTestBody, false},
		{"too large", flushFormType, flushTestBody + "&pad=" + strings.Repeat("a", flushFormMaxBytes), false},
		{"just at the limit", flushFormType, flushTestBody + "&pad=" + strings.Repeat("a", flushFormMaxBytes-len(flushTestBody+"&pad=")), true},
	}
	for _, tc := range cases {
		h := flushButtonHeaders(self, self)
		h["Content-Type"] = tc.contentType
		rec := serveFlushRequest(t, self, http.MethodPost, h, tc.body)
		if tc.clears {
			assertFlushClears(t, rec, tc.name)
		} else {
			assertFlushAsks(t, rec, http.StatusForbidden, tc.name)
		}
	}
}

// On a site served over http, the button clears too (flush.js clears, since
// Clear-Site-Data needs https), with or without Fetch Metadata, as long as
// Origin is the site's own and the token (in the cookie without the __Host-
// prefix) matches.
func TestFlushPage_PostOnHTTPSite(t *testing.T) {
	const self = "http://192.0.2.1:3000"
	for name, h := range map[string]map[string]string{
		"with Fetch Metadata":    flushButtonHeaders(self, self),
		"without Fetch Metadata": {"Origin": self, "Content-Type": flushFormType, "Cookie": "flush-confirm=" + flushTestToken},
	} {
		rec := serveFlushRequest(t, self, http.MethodPost, h, flushTestBody)
		assertFlushClears(t, rec, name)
		assert.Empty(t, rec.Header().Get("Clear-Site-Data"), name)
		assert.Contains(t, rec.Header().Values("Set-Cookie"), "flush-confirm=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict", name)
	}
	rec := serveFlushRequest(t, self, http.MethodPost, flushButtonHeaders(self, "https://192.0.2.1:3000"), flushTestBody)
	assertFlushAsks(t, rec, http.StatusForbidden, "https Origin on an http site")
	h := flushButtonHeaders(self, self)
	h["Cookie"] = "__Host-flush-confirm=" + flushTestToken
	rec = serveFlushRequest(t, self, http.MethodPost, h, flushTestBody)
	assertFlushAsks(t, rec, http.StatusForbidden, "the https cookie on http")

	// http では別のサブドメインが Path の長い同名の Cookie を置けるので、それが先に届いても消せる。
	h = flushButtonHeaders(self, self)
	h["Cookie"] = "flush-confirm=junk; flush-confirm=" + flushTestToken
	assertFlushClears(t, serveFlushRequest(t, self, http.MethodPost, h, flushTestBody), "another cookie of the same name first on http")
	h["Cookie"] = "flush-confirm=junk; flush-confirm=junk2"
	assertFlushAsks(t, serveFlushRequest(t, self, http.MethodPost, h, flushTestBody), http.StatusForbidden, "no cookie of the same name matches on http")
}
