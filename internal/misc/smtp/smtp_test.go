package smtp

import (
	"bufio"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSanitizeHeaderValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain value is unchanged",
			in:   "user@example.com",
			want: "user@example.com",
		},
		{
			name: "strips LF injection",
			in:   "victim@example.com\nBcc: attacker@evil.com",
			want: "victim@example.comBcc: attacker@evil.com",
		},
		{
			name: "strips CR injection",
			in:   "subject\rSubject: spoofed",
			want: "subjectSubject: spoofed",
		},
		{
			name: "strips CRLF injection",
			in:   "victim@example.com\r\nBcc: attacker@evil.com",
			want: "victim@example.comBcc: attacker@evil.com",
		},
		{
			name: "empty input returns empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeHeaderValue(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeHeaderValue(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// fakeSMTP is a minimal SMTP server for exercising the full Send dialog
// without depending on a real mail server.
type fakeSMTP struct {
	ln        net.Listener
	offerAuth bool
	// rejectAt: "MAIL", "RCPT", "DATA", or "" for happy path.
	rejectAt string
	mu       sync.Mutex
	received []string
}

func newFakeSMTP(t *testing.T, offerAuth bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{ln: ln, offerAuth: offerAuth}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func newRejectingSMTP(t *testing.T, rejectAt string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{ln: ln, rejectAt: rejectAt}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSMTP) port() int {
	_, portStr, _ := net.SplitHostPort(f.ln.Addr().String())
	p, _ := strconv.Atoi(portStr)
	return p
}

func (f *fakeSMTP) record(s string) {
	f.mu.Lock()
	f.received = append(f.received, s)
	f.mu.Unlock()
}

func (f *fakeSMTP) messages() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]string, len(f.received))
	copy(cp, f.received)
	return cp
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	fmt.Fprint(conn, "220 localhost ESMTP\r\n")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimRight(line, "\r\n")
		upper := strings.ToUpper(trimmed)
		switch {
		case strings.HasPrefix(upper, "EHLO"):
			if f.offerAuth {
				fmt.Fprint(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
			} else {
				fmt.Fprint(conn, "250 localhost\r\n")
			}
		case strings.HasPrefix(upper, "STARTTLS"):
			fmt.Fprint(conn, "502 STARTTLS not supported\r\n")
		case strings.HasPrefix(upper, "AUTH"):
			f.record("AUTH")
			if f.rejectAt == "AUTH" {
				fmt.Fprint(conn, "535 authentication failed\r\n")
				return
			}
			fmt.Fprint(conn, "235 2.7.0 Authentication successful\r\n")
		case strings.HasPrefix(upper, "MAIL FROM"):
			f.record(trimmed)
			if f.rejectAt == "MAIL" {
				fmt.Fprint(conn, "550 mailbox unavailable\r\n")
				return
			}
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.HasPrefix(upper, "RCPT TO"):
			f.record(trimmed)
			if f.rejectAt == "RCPT" {
				fmt.Fprint(conn, "550 mailbox unavailable\r\n")
				return
			}
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.HasPrefix(upper, "DATA"):
			if f.rejectAt == "DATA" {
				fmt.Fprint(conn, "554 transaction failed\r\n")
				return
			}
			fmt.Fprint(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")
			var body strings.Builder
			for {
				dline, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if dline == ".\r\n" {
					break
				}
				body.WriteString(dline)
			}
			f.record(body.String())
			fmt.Fprint(conn, "250 OK\r\n")
		case strings.HasPrefix(upper, "QUIT"):
			fmt.Fprint(conn, "221 Bye\r\n")
			return
		default:
			fmt.Fprint(conn, "250 OK\r\n")
		}
	}
}

func waitForMessages(f *fakeSMTP, min int) []string {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.messages()) >= min {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f.messages()
}

func TestSend_DeliversAndSanitizesHeaders(t *testing.T) {
	srv := newFakeSMTP(t, false)

	// ヘッダーに改行を仕込んでも注入されないことを検証する。
	Send("127.0.0.1", srv.port(), nil, nil,
		"from@example.com\r\nBcc: evil@example.com",
		"to@example.com",
		"Hello\nInjected: header",
		"body text")

	msgs := waitForMessages(srv, 3)
	if len(msgs) < 3 {
		t.Fatalf("expected at least 3 SMTP exchanges, got %d: %v", len(msgs), msgs)
	}

	// 改行が除去されたことでMAIL FROMが単一コマンドになり、追加のMAIL FROMが
	// 発行されていないことを確認する (注入成功なら2回発行される)。
	var mailFromCount int
	for _, m := range msgs {
		if strings.HasPrefix(m, "MAIL FROM:") {
			mailFromCount++
		}
	}
	if mailFromCount != 1 {
		t.Errorf("expected exactly 1 MAIL FROM (injection neutralized), got %d: %v", mailFromCount, msgs)
	}
	// DATA本文では連結済みのサニタイズ結果が入っていること。
	body := msgs[2]
	if !strings.Contains(body, "From: from@example.comBcc: evil@example.com") {
		t.Errorf("body missing sanitized From header: %q", body)
	}
	if !strings.Contains(body, "Subject: HelloInjected: header") {
		t.Errorf("body missing sanitized Subject: %q", body)
	}
	if !strings.Contains(body, "body text") {
		t.Errorf("body text missing: %q", body)
	}
}

func TestSend_WithAuth(t *testing.T) {
	srv := newFakeSMTP(t, true)
	user, pass := "u", "p"

	Send("127.0.0.1", srv.port(), &user, &pass,
		"from@example.com", "to@example.com", "subj", "body")

	msgs := waitForMessages(srv, 4)
	if !slices.Contains(msgs, "AUTH") {
		t.Errorf("expected AUTH to be issued, got %v", msgs)
	}
}

func TestSend_RejectionPathsAreBestEffort(t *testing.T) {
	// サーバ側が途中でエラー応答してもpanicせずSendが終了することを確認する。
	for _, rej := range []string{"MAIL", "RCPT", "DATA"} {
		t.Run(rej, func(t *testing.T) {
			srv := newRejectingSMTP(t, rej)
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Send panicked on reject=%s: %v", rej, r)
				}
			}()
			Send("127.0.0.1", srv.port(), nil, nil,
				"from@example.com", "to@example.com", "subj", "body")
		})
	}
}

func TestSend_AuthFailureIsBestEffort(t *testing.T) {
	// サーバがAUTHを拒否してもpanicせず終了することを確認する。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	f := &fakeSMTP{ln: ln, offerAuth: true, rejectAt: "AUTH"}
	go f.serve()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	user, pass := "u", "p"

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send panicked: %v", r)
		}
	}()
	Send("127.0.0.1", port, &user, &pass,
		"from@example.com", "to@example.com", "subj", "body")
}

func TestSend_DialFailureIsBestEffort(t *testing.T) {
	// ポート未割り当てでdial失敗時にpanicせずreturnすることを確認する。
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send panicked: %v", r)
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()
	Send("127.0.0.1", port, nil, nil, "from@example.com", "to@example.com", "subj", "body")
}

// #496: HTTP CONNECT proxy 経由で SMTP サーバに接続できることを確認する。
// 実 proxy (Squid 等) の挙動を真似て、target SMTP に先に接続して banner を
// バッファし、HTTP 200 応答 + banner を 1 write で client に push する。
// 旧実装の readCONNECTResponse は raw conn.Read で over-read していたため
// このシナリオで SMTP banner を握り潰して接続が hang していた (#553 review)。
func TestSendWithOptions_HTTPConnectProxy(t *testing.T) {
	srv := newFakeSMTP(t, false)
	targetAddr := fmt.Sprintf("127.0.0.1:%d", srv.port())

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })

	go func() {
		client, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer client.Close()

		// 1) CONNECT request を読み捨て
		r := bufio.NewReader(client)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		// 2) 先に target に dial して banner を 1 行読む (実 proxy 動作の
		//    模倣)。fakeSMTP は accept 後即 "220 localhost ESMTP\r\n" を送る。
		target, err := net.Dial("tcp", targetAddr)
		if err != nil {
			return
		}
		defer target.Close()
		bannerBuf := make([]byte, 256)
		_ = target.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := target.Read(bannerBuf)
		_ = target.SetReadDeadline(time.Time{})

		// 3) 200 応答 + banner を 1 write で push (TCP coalescing simulation)。
		_, _ = client.Write(append(
			[]byte("HTTP/1.1 200 Connection established\r\n\r\n"),
			bannerBuf[:n]...,
		))

		// 4) 以降は通常 pipe。
		done := make(chan struct{}, 2)
		go func() { _, _ = copyConn(target, r); done <- struct{}{} }()
		go func() { _, _ = copyConn(client, target); done <- struct{}{} }()
		<-done
	}()

	proxyURL := fmt.Sprintf("http://%s", proxyLn.Addr().String())
	SendWithOptions("127.0.0.1", srv.port(), nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: proxyURL})

	msgs := waitForMessages(srv, 3)
	if len(msgs) < 3 {
		t.Fatalf("expected ≥3 SMTP exchanges via proxy, got %d: %v", len(msgs), msgs)
	}
}

// 不正な proxy URL が指定された場合は warn ログを出して direct dial に
// fallback する。SMTP 配送そのものは止まらない (best-effort 方針)。
func TestSendWithOptions_InvalidProxyFallsBackToDirect(t *testing.T) {
	srv := newFakeSMTP(t, false)
	SendWithOptions("127.0.0.1", srv.port(), nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: "://not-a-url"})
	if len(waitForMessages(srv, 3)) < 3 {
		t.Fatalf("expected SMTP delivery via direct fallback")
	}
}

// 未対応 scheme (例: ftp) も direct fallback する。
func TestSendWithOptions_UnsupportedSchemeFallsBackToDirect(t *testing.T) {
	srv := newFakeSMTP(t, false)
	SendWithOptions("127.0.0.1", srv.port(), nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: "ftp://127.0.0.1:21"})
	if len(waitForMessages(srv, 3)) < 3 {
		t.Fatalf("expected SMTP delivery via direct fallback")
	}
}

// HTTP CONNECT が non-2xx で返してきた場合は接続失敗扱いで panic せず
// 終了する。
func TestSendWithOptions_HTTPConnectRejected(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })

	go func() {
		client, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		r := bufio.NewReader(client)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		fmt.Fprint(client, "HTTP/1.1 403 Forbidden\r\n\r\n")
	}()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send panicked on proxy 403: %v", r)
		}
	}()
	SendWithOptions("127.0.0.1", 25, nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: "http://" + proxyLn.Addr().String()})
}

func TestEncodeBasicAuth(t *testing.T) {
	if got := encodeBasicAuth("alice:secret"); got != "YWxpY2U6c2VjcmV0" {
		t.Errorf("encodeBasicAuth: got %q", got)
	}
}

// HTTP CONNECT proxy で Proxy-Authorization Basic ヘッダが付与されることを
// 確認する。proxy 側で受信した CONNECT request 全体を録画してアサート。
func TestSendWithOptions_HTTPConnectBasicAuth(t *testing.T) {
	srv := newFakeSMTP(t, false)
	targetAddr := fmt.Sprintf("127.0.0.1:%d", srv.port())

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })

	var capturedReq strings.Builder
	go func() {
		client, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		r := bufio.NewReader(client)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			capturedReq.WriteString(line)
			if line == "\r\n" {
				break
			}
		}
		fmt.Fprint(client, "HTTP/1.1 200 Connection established\r\n\r\n")
		target, err := net.Dial("tcp", targetAddr)
		if err != nil {
			return
		}
		defer target.Close()
		done := make(chan struct{}, 2)
		go func() { _, _ = copyConn(target, r); done <- struct{}{} }()
		go func() { _, _ = copyConn(client, target); done <- struct{}{} }()
		<-done
	}()

	proxyURL := fmt.Sprintf("http://alice:secret@%s", proxyLn.Addr().String())
	SendWithOptions("127.0.0.1", srv.port(), nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: proxyURL})
	_ = waitForMessages(srv, 3)

	got := capturedReq.String()
	if !strings.Contains(got, "Proxy-Authorization: Basic YWxpY2U6c2VjcmV0") {
		t.Errorf("Proxy-Authorization header missing or wrong: %q", got)
	}
}

// SOCKS5 with username/password auth: x/net/proxy が Auth method 0x02 を
// negotiate するので fake server も対応する。
func TestSendWithOptions_SOCKS5WithAuth(t *testing.T) {
	srv := newFakeSMTP(t, false)
	targetPort := srv.port()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })

	go func() {
		client, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		// Greeting: read VER + NMETHODS
		hdr := make([]byte, 2)
		if _, err := client.Read(hdr); err != nil {
			return
		}
		methods := make([]byte, int(hdr[1]))
		_, _ = client.Read(methods)
		// Select user/pass auth (0x02)
		_, _ = client.Write([]byte{0x05, 0x02})

		// Read sub-negotiation: VER=1, ULEN, UNAME, PLEN, PASSWD
		auth := make([]byte, 256)
		n, _ := client.Read(auth)
		_ = n
		// 認証成功: VER=1, STATUS=0
		_, _ = client.Write([]byte{0x01, 0x00})

		// CONNECT (VER+CMD+RSV+ATYP+ADDR+PORT = 10 bytes for IPv4)
		req := make([]byte, 10)
		_, _ = client.Read(req)
		_, _ = client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		target, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", targetPort))
		if err != nil {
			return
		}
		defer target.Close()
		done := make(chan struct{}, 2)
		go func() { _, _ = copyConn(target, client); done <- struct{}{} }()
		go func() { _, _ = copyConn(client, target); done <- struct{}{} }()
		<-done
	}()

	proxyURL := fmt.Sprintf("socks5://alice:secret@%s", proxyLn.Addr().String())
	SendWithOptions("127.0.0.1", targetPort, nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: proxyURL})
	if len(waitForMessages(srv, 3)) < 3 {
		t.Fatalf("expected SMTP delivery via authenticated SOCKS5")
	}
}

// CONNECT 応答が malformed (CRLF 無し) のとき接続は close される。
func TestSendWithOptions_HTTPConnectMalformedResponse(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	go func() {
		client, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		// Read CONNECT (until empty line) then return malformed (no CRLF)
		r := bufio.NewReader(client)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				break
			}
		}
		fmt.Fprint(client, "BLOB-NOT-HTTP")
	}()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send panicked: %v", r)
		}
	}()
	SendWithOptions("127.0.0.1", 25, nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: "http://" + proxyLn.Addr().String()})
}

// proxy URL に port 省略 → scheme 既定 (http=80) を補う経路。port 80 への
// 接続は dial timeout か connection refused になるが、SendWithOptions は
// best-effort で panic せず終了する。
func TestSendWithOptions_HTTPProxyDefaultPort(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send panicked on default port path: %v", r)
		}
	}()
	SendWithOptions("127.0.0.1", 25, nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: "http://127.0.0.1"}) // no port → default 80
}

// HTTPS proxy URL は tls.DialWithDialer 経路に入る。proxy 側に TLS 立てる
// テストは大変なので、TLS handshake 失敗 (= proxy が plain TCP) で
// 想定通り dial error になることを確認する (= HTTPS branch コード実行
// 確認のみ、実 SMTP は届かない)。
func TestSendWithOptions_HTTPSProxyTLSHandshakeFails(t *testing.T) {
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	go func() {
		c, _ := proxyLn.Accept()
		if c != nil {
			c.Close()
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Send panicked: %v", r)
		}
	}()
	SendWithOptions("127.0.0.1", 25, nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: "https://" + proxyLn.Addr().String()})
}

// SOCKS5 proxy 経由の dial 経路を確認。fake SOCKS5 server が CONNECT を
// 受けて target SMTP に proxy する。
func TestSendWithOptions_SOCKS5Proxy(t *testing.T) {
	srv := newFakeSMTP(t, false)
	targetPort := srv.port()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks5: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })

	go func() {
		client, err := proxyLn.Accept()
		if err != nil {
			return
		}
		defer client.Close()
		// SOCKS5 hand-shake (no-auth)
		hdr := make([]byte, 2)
		if _, err := client.Read(hdr); err != nil {
			return
		}
		nMethods := int(hdr[1])
		methods := make([]byte, nMethods)
		_, _ = client.Read(methods)
		// 0x05 0x00 = no-auth selected
		_, _ = client.Write([]byte{0x05, 0x00})

		// CONNECT request: VER=5, CMD=1 (CONNECT), RSV=0, ATYP=1 (IPv4),
		// DST.ADDR=4 bytes, DST.PORT=2 bytes
		req := make([]byte, 10)
		if _, err := client.Read(req); err != nil {
			return
		}
		// Reply: VER=5, REP=0 (success), RSV=0, ATYP=1, BND.ADDR=0.0.0.0, BND.PORT=0
		_, _ = client.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})

		// Pipe to target SMTP server.
		target, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", targetPort))
		if err != nil {
			return
		}
		defer target.Close()
		done := make(chan struct{}, 2)
		go func() { _, _ = copyConn(target, client); done <- struct{}{} }()
		go func() { _, _ = copyConn(client, target); done <- struct{}{} }()
		<-done
	}()

	proxyURL := fmt.Sprintf("socks5://%s", proxyLn.Addr().String())
	SendWithOptions("127.0.0.1", targetPort, nil, nil,
		"from@example.com", "to@example.com", "subj", "body",
		Options{ProxyURL: proxyURL})
	if len(waitForMessages(srv, 3)) < 3 {
		t.Fatalf("expected SMTP delivery via SOCKS5 proxy")
	}
}

// copyConn shuttles bytes from src to dst. Local helper for the proxy test
// (avoids pulling in io.Copy's interface dance).
func copyConn(dst net.Conn, src interface{ Read([]byte) (int, error) }) (int64, error) {
	buf := make([]byte, 4096)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			return total, err
		}
	}
}

// buildMessage は internal helper だが、multipart/alternative の構造が
// 正しく組まれているかを直接 unit test しておく (#600 item 4)。SendMessage 経由
// は fake SMTP server で end-to-end カバーできるが boundary や header の
// 詳細は文字列単位の assertion が早い。
func TestBuildMessage_TextOnly(t *testing.T) {
	got := buildMessage("from@example.test", "to@example.test", "Hi", "plain body", "")
	if !strings.Contains(got, "Content-Type: text/plain; charset=UTF-8") {
		t.Errorf("expected text/plain header, got: %s", got)
	}
	if strings.Contains(got, "multipart") {
		t.Errorf("html 空のとき multipart を使ってはいけない")
	}
	if !strings.Contains(got, "plain body") {
		t.Errorf("body should be embedded")
	}
}

func TestBuildMessage_Multipart(t *testing.T) {
	got := buildMessage("from@example.test", "to@example.test", "Hi", "plain body", "<p>html body</p>")
	if !strings.Contains(got, "Content-Type: multipart/alternative") {
		t.Errorf("expected multipart/alternative, got: %s", got)
	}
	if !strings.Contains(got, `boundary="----=_MK_GO_BOUNDARY_`) {
		t.Errorf("expected randomized boundary header (prefix ----=_MK_GO_BOUNDARY_)")
	}
	// text / html part の本体検証 (Transfer-Encoding は quoted-printable、#3280)
	if !strings.Contains(got, "Content-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nplain body") {
		t.Errorf("text part missing or malformed")
	}
	if !strings.Contains(got, "Content-Type: text/html; charset=UTF-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n<p>html body</p>") {
		t.Errorf("html part missing or malformed")
	}
	// closing boundary は randomized なので prefix で確認
	if !strings.Contains(got, "----=_MK_GO_BOUNDARY_") || !strings.Contains(got, "--\r\n") {
		t.Errorf("closing boundary marker missing")
	}
}

// 非 ASCII subject (#600 item 4 review): RFC 2047 encoded-word で
// `=?UTF-8?B?<base64>?=` 形式に encode されること。
func TestBuildMessage_NonASCIISubjectIsEncoded(t *testing.T) {
	got := buildMessage("from@example.test", "to@example.test", "確認メール", "body", "")
	if !strings.Contains(got, "=?UTF-8?b?") && !strings.Contains(got, "=?UTF-8?B?") {
		t.Errorf("non-ASCII subject must be RFC 2047 encoded-word, got: %s", got)
	}
	// 生 UTF-8 bytes が subject 行に残っていない
	if strings.Contains(got, "Subject: 確認メール") {
		t.Errorf("raw UTF-8 subject must not appear; got: %s", got)
	}
}

// ASCII-only subject はそのまま (encoded-word でラップしない)
func TestBuildMessage_ASCIISubjectIsNotEncoded(t *testing.T) {
	got := buildMessage("from@example.test", "to@example.test", "Hello", "body", "")
	if !strings.Contains(got, "Subject: Hello\r\n") {
		t.Errorf("ASCII subject should be passed verbatim, got: %s", got)
	}
}

// Content-Transfer-Encoding は quoted-printable (#3280)。8bit のまま折り返さずに
// 書くと、長い行が RFC 5322 の 998 オクテットの上限を破る。
func TestBuildMessage_TransferEncodingIsQuotedPrintable(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		html string
	}{
		{"text-only", "body", ""},
		{"multipart", "body", "<p>x</p>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildMessage("f@e.test", "t@e.test", "S", tc.text, tc.html)
			if !strings.Contains(got, "Content-Transfer-Encoding: quoted-printable") {
				t.Errorf("must declare quoted-printable transfer encoding, got: %s", got)
			}
			if strings.Contains(got, "Content-Transfer-Encoding: 8bit") {
				t.Errorf("must not declare 8bit, got: %s", got)
			}
			if strings.Contains(got, "Content-Transfer-Encoding: 7bit") {
				t.Errorf("must not declare 7bit (incorrect for UTF-8), got: %s", got)
			}
		})
	}
}

// PR for #1111: Options.Secure=true で client 側が SMTP greeting を受ける
// 前に TLS handshake を開始することを確認する。TLS record の最初のバイト
// は 0x16 (= ContentTypeHandshake) なので、server 側で最初の 1 byte を
// 読んでこれが 0x16 なら implicit TLS を attempt したと判定する。
//
// 自己署名 cert + RootCAs 注入は test の複雑度が大きいのでここでは
// handshake 完了は問わない (= server 側が cert を提示しないので client
// 側は handshake error で接続を閉じる、それで OK — 目的は「TLS を
// 開始する経路に入った」ことの検証)。
func TestSendMessage_Secure_InitiatesTLSHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	sawClientHello := make(chan bool, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			sawClientHello <- false
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, _ := conn.Read(buf)
		sawClientHello <- (n == 1 && buf[0] == 0x16)
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("SendMessage panicked: %v", r)
		}
	}()
	SendMessage("127.0.0.1", port, nil, nil, "f@e.test", "t@e.test",
		Message{Subject: "s", Text: "b"}, Options{Secure: true})

	select {
	case got := <-sawClientHello:
		if !got {
			t.Error("expected first byte to be 0x16 (TLS ClientHello) when Secure=true")
		}
	case <-time.After(3 * time.Second):
		t.Error("server side timed out waiting for client bytes")
	}
}

// Secure=false の対照 test: client は greeting を待ってから EHLO を送るので、
// server 側が greeting を送らなければ client は何も送って来ない (= 接続を
// timeout で閉じる)。最初に受信したいずれかのバイトが 0x16 (TLS) でない
// ことを確認する。
func TestSendMessage_PlainTCP_DoesNotInitiateTLSHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	gotClientHello := make(chan bool, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			gotClientHello <- false
			return
		}
		defer conn.Close()
		// greeting 送らず client が何を送ってくるかだけ観察。implicit TLS
		// 経路なら 0x16 が即届く、plain SMTP 経路なら greeting 待ちなので
		// timeout まで何も来ない。
		_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		buf := make([]byte, 1)
		n, _ := conn.Read(buf)
		gotClientHello <- (n == 1 && buf[0] == 0x16)
	}()

	_, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	SendMessage("127.0.0.1", port, nil, nil, "f@e.test", "t@e.test",
		Message{Subject: "s", Text: "b"}, Options{Secure: false})

	select {
	case got := <-gotClientHello:
		if got {
			t.Error("Secure=false path should not start with TLS ClientHello")
		}
	case <-time.After(3 * time.Second):
		t.Error("server side timed out")
	}
}

// random boundary (#600 item 4 review): 同じ入力でも 2 回呼ぶと boundary が変わる
func TestBuildMessage_RandomBoundary(t *testing.T) {
	a := buildMessage("f@e.test", "t@e.test", "S", "x", "<p>x</p>")
	b := buildMessage("f@e.test", "t@e.test", "S", "x", "<p>x</p>")
	// boundary 行を抽出して比較
	pickBoundary := func(s string) string {
		for _, line := range strings.Split(s, "\r\n") {
			if strings.HasPrefix(line, "Content-Type: multipart/alternative; boundary=") {
				return line
			}
		}
		return ""
	}
	ba, bb := pickBoundary(a), pickBoundary(b)
	if ba == "" || bb == "" {
		t.Fatalf("boundary header missing")
	}
	if ba == bb {
		t.Errorf("boundary should be randomized per message; both=%s", ba)
	}
}

// maxLineOctets reports the longest line (without CRLF) in a message.
func maxLineOctets(msg string) int {
	longest := 0
	for _, line := range strings.Split(msg, "\r\n") {
		if len(line) > longest {
			longest = len(line)
		}
	}
	return longest
}

// 長い本文と長い件名でも、メールの各行は RFC 5322 の 998 オクテットに収まり、
// 元の文字列に戻せる (#3280)。通報のコメントは最大 2048 文字で、日本語なら
// 約 333 文字で 1 行が上限を超える。
func TestBuildMessage_LongLinesStayWithinLimitAndRoundTrip(t *testing.T) {
	longText := strings.Repeat("通報の本文。", 400) + "\n2 行目\nhttps://remote.example/notes/" + strings.Repeat("a", 1200)
	longHTML := "<p>" + strings.Repeat("あ", 2048) + "</p>"
	for _, subject := range []string{
		strings.Repeat("件名", 300),
		strings.Repeat("s", 2000),
		"New Abuse Report",
	} {
		for _, html := range []string{"", longHTML} {
			got := buildMessage("f@e.test", "t@e.test", subject, longText, html)
			if n := maxLineOctets(got); n > 998 {
				t.Fatalf("line of %d octets exceeds RFC 5322 limit (subject len %d, html %v)", n, len(subject), html != "")
			}

			header, body, ok := strings.Cut(got, "\r\n\r\n")
			if !ok {
				t.Fatal("no header/body separator")
			}
			// 件名は折り返しを外して decode すると元に戻る。
			var rawSubject string
			for _, line := range strings.Split(strings.ReplaceAll(header, "\r\n ", " "), "\r\n") {
				if v, ok := strings.CutPrefix(line, "Subject: "); ok {
					rawSubject = v
				}
			}
			decoded, err := new(mime.WordDecoder).DecodeHeader(rawSubject)
			if err != nil || decoded != subject {
				t.Fatalf("subject did not round-trip: err=%v, got %q", err, decoded)
			}

			if html == "" {
				text, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
				if err != nil {
					t.Fatal(err)
				}
				want := strings.ReplaceAll(longText, "\n", "\r\n")
				if string(text) != want {
					t.Fatalf("text body did not round-trip")
				}
			}
		}
	}
}

// 件名の encoded-word は 1 つずつが 75 文字以内で、UTF-8 の文字を途中で切らない
// (RFC 2047 は 1 つの encoded-word の中で文字が完結することを求める)。
func TestEncodeSubject_WordsAreShortAndWholeCharacters(t *testing.T) {
	// 先頭に ASCII を 1 文字置き、3 バイトの文字の並びを区切りの位置からずらす
	// (39 バイトは 3 の倍数なので、日本語だけだと区切りが偶然文字の境目に乗る)。
	got := encodeSubject("a" + strings.Repeat("件名あいう", 100))
	words := strings.Split(got, "\r\n ")
	if len(words) < 2 {
		t.Fatalf("long subject must be split into several encoded-words: %q", got)
	}
	dec := new(mime.WordDecoder)
	for _, w := range words {
		if len(w) > 75 {
			t.Errorf("encoded-word longer than 75 chars: %d", len(w))
		}
		s, err := dec.Decode(w)
		if err != nil || !utf8.ValidString(s) {
			t.Errorf("encoded-word %q does not hold whole characters (err=%v)", w, err)
		}
	}
}

// 不正な UTF-8 の件名でも止まらずに符号化する (継続バイトが並ぶと区切りが
// 進まなくなる形を防ぐ)。
func TestEncodeSubject_InvalidUTF8Terminates(t *testing.T) {
	done := make(chan string, 1)
	go func() { done <- encodeSubject("a" + strings.Repeat("\x80", 200)) }()
	select {
	case got := <-done:
		if maxLineOctets(got) > 998 {
			t.Fatalf("line too long")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("encodeSubject did not terminate on invalid UTF-8")
	}
}

// ASCII の件名は 900 バイトまでそのまま、901 バイトから符号化する。どちらでも
// 件名の行は 998 オクテットに収まる。
func TestEncodeSubject_ASCIIBoundary(t *testing.T) {
	at := strings.Repeat("s", maxASCIISubject)
	if got := encodeSubject(at); got != at {
		t.Errorf("subject of %d bytes must be sent as-is", maxASCIISubject)
	}
	if n := len("Subject: " + at); n > 998 {
		t.Errorf("the as-is limit itself exceeds the line limit: %d", n)
	}
	over := at + "s"
	got := encodeSubject(over)
	if !strings.HasPrefix(got, "=?UTF-8?B?") {
		t.Errorf("subject of %d bytes must be encoded", len(over))
	}
	if n := maxLineOctets("Subject: " + got); n > 76 {
		t.Errorf("encoded subject line of %d chars exceeds 76", n)
	}
}
