// Package smtp provides a best-effort SMTP email sender shared by admin
// and i handler packages.
//
// SMTP設定エラーはログに出力し、呼び出し元にはエラーを返さない
// (ベストエフォート送信)。
package smtp

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"mime/quotedprintable"
	"net"
	gosmtp "net/smtp"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/proxy"
)

// Options holds optional configuration for Send. Future flags (TLS mode,
// timeouts, etc) live here. Zero-value is "no overrides".
type Options struct {
	// ProxyURL routes the SMTP TCP connection through a forward proxy.
	// 形式は upstream Misskey の `proxySmtp` と同じく URL (scheme で
	// プロトコルを指定): `socks5://user:pass@host:1080` または
	// `http://host:3128` (HTTP CONNECT)。空文字なら direct dial。
	ProxyURL string

	// Secure selects implicit TLS (= smtps://, typically port 465) over
	// the SMTP TCP connection, before the SMTP greeting. Mirrors
	// `meta.smtpSecure` (upstream Misskey TS は Nodemailer の
	// `secure: true` に対応する)。false なら従来通り plain TCP で
	// SMTP greeting を受けてから STARTTLS にアップグレードする (= port
	// 587 の submission 経路)。
	Secure bool
}

// Message bundles the body parts of an email. Subject + Text are required;
// HTML is optional. When HTML is set, SendMessage emits a multipart/alternative
// MIME structure so MUA が好きな方を表示できる (TS の sendEmail と同等)。
type Message struct {
	Subject string
	Text    string
	HTML    string // optional
}

// sanitizeHeaderValue strips CR and LF characters to prevent SMTP header
// injection.
func sanitizeHeaderValue(s string) string {
	// 攻撃者がヘッダーフィールドに改行(CR/LF)を仕込むとBCC等の任意ヘッダーを
	// 注入できてしまうため、ここで無害化する。
	r := strings.NewReplacer("\r", "", "\n", "")
	return r.Replace(s)
}

// Send sends a plain-text email via SMTP. Direct dial; equivalent to
// SendWithOptions(.., Options{}).
func Send(host string, port int, user, pass *string, from, to, subject, body string) {
	SendWithOptions(host, port, user, pass, from, to, subject, body, Options{})
}

// SendWithOptions is Send + functional options. Currently only ProxyURL is
// supported; mismatched scheme falls back to direct dial with a warning.
// 互換のため text/plain only で送る。HTML 同送が必要なら SendMessage を使う。
func SendWithOptions(host string, port int, user, pass *string, from, to, subject, body string, opts Options) {
	SendMessage(host, port, user, pass, from, to, Message{Subject: subject, Text: body}, opts)
}

// SendMessage sends an email with optional multipart/alternative HTML body.
// HTML が空なら text/plain only、HTML が設定されていれば multipart/alternative
// で text と HTML 両方を送出する。Misskey TS sendEmail との挙動互換 (#600 item 4)。
func SendMessage(host string, port int, user, pass *string, from, to string, msg Message, opts Options) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	// ヘッダーフィールドのCRLFインジェクション対策
	from = sanitizeHeaderValue(from)
	to = sanitizeHeaderValue(to)
	subject := sanitizeHeaderValue(msg.Subject)

	var auth gosmtp.Auth
	if user != nil && pass != nil && *user != "" {
		auth = gosmtp.PlainAuth("", *user, *pass, host)
	}

	body := buildMessage(from, to, subject, msg.Text, msg.HTML)

	tlsConfig := &tls.Config{ServerName: host}
	conn, err := dialSMTP(addr, opts.ProxyURL, 10*time.Second)
	if err != nil {
		slog.Warn("email: dial failed", "error", err, "proxyConfigured", opts.ProxyURL != "")
		return
	}

	// Implicit TLS (= smtps:// / port 465 etc.): SMTP greeting を受ける前に
	// TCP connection を TLS で包む。handshake には dialSMTP と同じ 10s 上限
	// を deadline で適用してから handshake 後にクリアする (= 以後の SMTP
	// command は標準の wire timeout に従う)。
	if opts.Secure {
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		tlsConn := tls.Client(conn, tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			slog.Warn("email: implicit TLS handshake failed", "error", err, "host", host)
			_ = conn.Close()
			return
		}
		_ = tlsConn.SetDeadline(time.Time{})
		conn = tlsConn
	}

	c, err := gosmtp.NewClient(conn, host)
	if err != nil {
		slog.Warn("email: smtp client failed", "error", err)
		return
	}
	defer c.Close()

	// Implicit TLS 経由なら既に encrypted なので STARTTLS は不要 (= server が
	// EHLO 応答で STARTTLS を提供しない設定が普通)。それ以外の経路は従来
	// 通り STARTTLS を attempt。失敗時は Warn に昇格 — 認証情報を平文で
	// 送る fallback は危険度が高いため operator が気付ける level にする。
	if !opts.Secure {
		if err := c.StartTLS(tlsConfig); err != nil {
			slog.Warn("email: STARTTLS upgrade failed, continuing without encryption",
				"error", err, "host", host)
		}
	}

	if auth != nil {
		if err := c.Auth(auth); err != nil {
			slog.Warn("email: auth failed", "error", err)
			return
		}
	}

	if err := c.Mail(from); err != nil {
		slog.Warn("email: MAIL FROM failed", "error", err)
		return
	}
	if err := c.Rcpt(to); err != nil {
		slog.Warn("email: RCPT TO failed", "error", err)
		return
	}

	w, err := c.Data()
	if err != nil {
		slog.Warn("email: DATA failed", "error", err)
		return
	}
	_, _ = w.Write([]byte(body))
	_ = w.Close()
	_ = c.Quit()

	slog.Info("email sent", "to", to, "subject", subject)
}

// buildMessage assembles the RFC 5322 message body. text-only または
// multipart/alternative のどちらかを返す。HTML が空なら従来通り text/plain。
//
// Subject は非 ASCII を含む可能性があるので RFC 2047 encoded-word で送出する
// (PR #611 review で指摘)。multipart の boundary は crypto/rand 由来で
// per-message に変える。
//
// **本文は quoted-printable で送る (#3280)。** 以前は 8bit のまま折り返さずに
// 書いていたので、長い行 (通報のコメントは最大 2048 文字で、日本語なら約 333
// 文字で 998 オクテットを超える) が RFC 5322 の行の上限を破り、受け取る側の MTA
// が UTF-8 の途中で折り返したり拒否したりしえた。quoted-printable なら 1 行は
// 76 文字に収まり、改行も CRLF に揃う。本家 (nodemailer) も必要に応じて符号化する。
func buildMessage(from, to, subject, text, html string) string {
	encodedSubject := encodeSubject(subject)
	if html == "" {
		return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s",
			from, to, encodedSubject, encodeQuotedPrintable(text))
	}
	boundary := randomBoundary()
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\n", from, to, encodedSubject)
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
	// text part
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n",
		boundary, encodeQuotedPrintable(text))
	// html part
	fmt.Fprintf(&b, "--%s\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n%s\r\n",
		boundary, encodeQuotedPrintable(html))
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.String()
}

// encodeQuotedPrintable encodes s as quoted-printable (RFC 2045). 改行は CRLF
// に揃い、1 行は 76 文字までに折り返される。
func encodeQuotedPrintable(s string) string {
	var b strings.Builder
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(s))
	_ = w.Close()
	return b.String()
}

// maxASCIISubject is the longest ASCII subject sent as-is. 件名の行は
// "Subject: " を含めて 998 オクテットまでなので、それより長いものは
// encoded-word に包んで折り返す。
const maxASCIISubject = 900

// subjectChunkBytes is how many bytes go into one encoded-word. base64 で
// 52 文字になり、"=?UTF-8?B?" と "?=" を足して 64 文字。1 行目の "Subject: "
// を足しても 73 文字で、RFC 2047 の「encoded-word を含む行は 76 文字まで」に収まる。
const subjectChunkBytes = 39

// encodeSubject applies RFC 2047 encoded-word formatting when the subject
// contains non-ASCII bytes or is too long for one header line. 短い ASCII なら
// そのまま返す (encoded-word でラップすると一部の MUA が読みにくく表示するため)。
//
// mime.BEncoding は ASCII だけの件名を符号化せず、長い件名も encoded-word を
// 空白でつなぐだけで行を折り返さない。どちらも長い件名で 1 行の上限
// (998 オクテット) を超えるので、自分で区切って encoded-word の間で折り返す
// (RFC 5322 の folding、#3280)。区切りは UTF-8 の文字の途中に置かない。
func encodeSubject(s string) string {
	needsEncoding := len(s) > maxASCIISubject
	for i := 0; i < len(s) && !needsEncoding; i++ {
		if s[i] > 127 {
			needsEncoding = true
		}
	}
	if !needsEncoding {
		return s
	}
	// 不正な UTF-8 は置き換えておく。継続バイトが並ぶと、下の「文字の途中で
	// 切らない」調整で区切りが 0 まで下がって進まなくなる。
	s = strings.ToValidUTF8(s, "\uFFFD")
	var words []string
	for len(s) > 0 {
		n := len(s)
		if n > subjectChunkBytes {
			n = subjectChunkBytes
			for n > 0 && !utf8.RuneStart(s[n]) {
				n--
			}
		}
		words = append(words, "=?UTF-8?B?"+base64.StdEncoding.EncodeToString([]byte(s[:n]))+"?=")
		s = s[n:]
	}
	return strings.Join(words, "\r\n ")
}

// randomBoundary は crypto/rand 由来の 16 byte hex 文字列を boundary に使う。
// 固定 boundary だと body 内に同文字列が含まれた場合 MIME 解析が崩壊する
// (RFC 2046)。実害はほぼ無いが defensive に毎メッセージ別 boundary にする。
func randomBoundary() string {
	var buf [16]byte
	if _, err := cryptorand.Read(buf[:]); err != nil {
		// crypto/rand が失敗するのは process が壊れているレベルの異常事態
		// なので fallback は時刻ベースの dummy で済ませる (送信失敗より
		// 何か送る方が良い、UTC 秒で衝突確率ほぼ無視できる)。
		return fmt.Sprintf("----=_MK_GO_BOUNDARY_%d", time.Now().UnixNano())
	}
	return "----=_MK_GO_BOUNDARY_" + hex.EncodeToString(buf[:])
}

// dialSMTP opens a TCP connection to addr (SMTP server). When proxyURL is
// set, the connection is tunnelled via the named proxy:
//
//   - socks5://[user:pass@]host:port → SOCKS5 (golang.org/x/net/proxy)
//   - socks5h:// は socks5 と同じ扱い (Go 標準では区別しない)
//   - http://host:port / https://host:port → HTTP CONNECT tunnel
//
// 不明な scheme / parse 失敗時は warn ログを出して direct dial にフォール
// バックする (proxy 設定ミスでメール送信全停止になるよりは、direct で
// 送れる経路があれば送る方を優先する)。
func dialSMTP(addr, proxyURL string, timeout time.Duration) (net.Conn, error) {
	if proxyURL == "" {
		return net.DialTimeout("tcp", addr, timeout)
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		// **生の URL を出さない。** `socks5://user:pass@host` の形を取るので、
		// パース失敗時に丸ごと出すと認証情報がログに残る。同じ値を
		// `config_dump` は `redactURLUserinfo` でマスクしている。
		slog.Warn("email: invalid proxySmtp URL, falling back to direct dial", "err", err)
		return net.DialTimeout("tcp", addr, timeout)
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		return dialSOCKS5(u, addr, timeout)
	case "http", "https":
		return dialHTTPConnect(u, addr, timeout)
	default:
		slog.Warn("email: unsupported proxySmtp scheme, falling back to direct dial",
			"scheme", u.Scheme)
		return net.DialTimeout("tcp", addr, timeout)
	}
}

// dialSOCKS5 wraps golang.org/x/net/proxy.SOCKS5 with the configured
// timeout. user/pass は URL の userinfo から取り出す (空なら認証無し)。
func dialSOCKS5(u *url.URL, addr string, timeout time.Duration) (net.Conn, error) {
	var auth *proxy.Auth
	if u.User != nil {
		pw, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pw}
	}
	d, err := proxy.SOCKS5("tcp", u.Host, auth, &net.Dialer{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("socks5 setup: %w", err)
	}
	// proxy.Dialer.Dial は context をサポートしないが、x/net/proxy の
	// SOCKS5 内部 Dialer に Timeout を渡しているので接続全体に伝播する。
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, "tcp", addr)
	}
	return d.Dial("tcp", addr)
}

// dialHTTPConnect opens a TCP connection to the HTTP proxy and issues a
// CONNECT request to tunnel through to addr. TLS over HTTP CONNECT (https://
// proxy) は proxy への接続を TLS で包んでから CONNECT を送る; SMTP server
// 側の StartTLS とは独立。
//
// 重要: 実 proxy (Squid / nginx 等) は target に先に接続して greeting を
// バッファし、200 応答とまとめて 1 TCP segment で push する場合がある。
// 単純な conn.Read だと SMTP banner まで読み込んで捨ててしまうため、
// bufio.Reader で CONNECT 応答だけを正確に消費し、残った bytes は
// 接続に noticed させて caller (gosmtp.NewClient) に届ける必要がある。
// readCONNECTResponse + bufferedConn でこれを実装する。
func dialHTTPConnect(u *url.URL, addr string, timeout time.Duration) (net.Conn, error) {
	proxyHost := u.Host
	if _, _, splitErr := net.SplitHostPort(proxyHost); splitErr != nil {
		port := "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
		proxyHost = net.JoinHostPort(u.Hostname(), port)
	}

	dialer := &net.Dialer{Timeout: timeout}
	var conn net.Conn
	var err error
	if strings.EqualFold(u.Scheme, "https") {
		conn, err = tls.DialWithDialer(dialer, "tcp", proxyHost, &tls.Config{ServerName: u.Hostname()})
	} else {
		conn, err = dialer.Dial("tcp", proxyHost)
	}
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyHost, err)
	}

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", addr, addr)
	if u.User != nil {
		// Basic auth — RFC 7617 base64(user:pass). 空 user / 空 pass は省略。
		username := u.User.Username()
		password, _ := u.User.Password()
		if username != "" || password != "" {
			creds := username + ":" + password
			encoded := encodeBasicAuth(creds)
			req += "Proxy-Authorization: Basic " + encoded + "\r\n"
		}
	}
	req += "\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT: %w", err)
	}

	// bufio.Reader で CONNECT 応答 (status line + 空行までの header) を
	// line-by-line で読む。Read 1 回で SMTP banner まで巻き込まないこと、
	// それでも buffer に残ったバイトは bufferedConn 経由で gosmtp.NewClient
	// に届くことを保証する。
	br := bufio.NewReader(conn)
	if err := readCONNECTResponse(conn, br); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn wraps net.Conn so subsequent Reads draw from a bufio.Reader
// buffer first (consumed bytes that were left over after CONNECT response
// parsing). Writes / Close / Deadline 系は素 conn にそのまま委譲する。
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// readCONNECTResponse parses the proxy's HTTP/1.1 CONNECT response from a
// bufio.Reader. Returns nil on 2xx, error otherwise. Header 行は status の
// 後ろの空行まで読み捨てる。実 proxy が buffer した SMTP banner は br に
// 残し、後段の bufferedConn で gosmtp に渡す。
func readCONNECTResponse(conn net.Conn, br *bufio.Reader) error {
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return fmt.Errorf("set read deadline: %w", err)
	}
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	statusLine, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read CONNECT status line: %w", err)
	}
	statusLine = strings.TrimRight(statusLine, "\r\n")
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[1], "2") {
		return fmt.Errorf("CONNECT failed: %s", statusLine)
	}
	// header 行を空行まで消費する。Header line を超えた bytes は bufio
	// の buffer に残り、bufferedConn 経由で次の Read に渡る。
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read CONNECT headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			return nil
		}
	}
}

// encodeBasicAuth returns the base64 form of "user:pass" for the
// Proxy-Authorization header.
func encodeBasicAuth(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// errProxyTunnelFailed is exported for tests. (Currently dial errors are
// wrapped via fmt.Errorf, but a sentinel kept as an extension point.)
var errProxyTunnelFailed = errors.New("proxy tunnel failed")

var _ = errProxyTunnelFailed
