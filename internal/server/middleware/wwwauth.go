package middleware

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
)

// apiErrorEnvelope is the subset of the Misskey API error envelope needed to
// decide the WWW-Authenticate header. Bodies that do not carry a kind (the
// Fastify-style signup/signin replies, FST_ERR_CTP_* parse errors, the sparse
// signin {"error":{"id":...}} shape) unmarshal with Error==nil or Kind==""
// and are passed through untouched.
type apiErrorEnvelope struct {
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
		Kind    string `json:"kind"`
	} `json:"error"`
}

// WWWAuthenticate mirrors upstream ApiCallService's `#sendApiError` /
// `#sendAuthenticationError` WWW-Authenticate behaviour for the /api scope
// (#1608). Upstream attaches, in priority order:
//
//  1. status 401 -> `Bearer realm="Misskey"` (realm only; for
//     AUTHENTICATION_FAILED the dedicated #sendAuthenticationError path uses
//     error="invalid_token" with the message as error_description)
//  2. RATE_LIMIT_EXCEEDED -> no WWW-Authenticate (Retry-After is set by the
//     rate limiter where the error originates)
//  3. kind "client" -> error="invalid_request" with the message
//  4. kind "permission" -> error="insufficient_scope", but only for code
//     PERMISSION_DENIED (ROLE_PERMISSION_DENIED gets no header)
//  5. kind "server" -> no header
//
// mk-go の error 送出は handler が c.JSON で直接 envelope を書く分散型なので、
// 本家のような送出口での一括付与ができない。代わりに response writer を
// 仲介し、status >= 400 の応答 body を一旦 buffer して kind 付き envelope
// なら該当ヘッダを差してから書き戻す。error 応答以外 (status < 400) は
// 素通しで buffering コストもかからない。
func WWWAuthenticate() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			res := c.Response()
			w := &wwwAuthWriter{ResponseWriter: res.Writer}
			res.Writer = w
			// handler が error を返して何も書かなかった場合は echo の
			// HTTPErrorHandler が middleware chain の外で応答を書くため、
			// 元の writer に戻してから返す (buffer に取り込むと flush の
			// 機会が無く応答が消える)。
			defer func() {
				w.finalize()
				res.Writer = w.ResponseWriter
			}()
			return next(c)
		}
	}
}

// wwwAuthWriter buffers error responses (status >= 400) so the
// WWW-Authenticate header can be decided from the serialized envelope before
// the header section is committed. Non-error responses pass through directly.
type wwwAuthWriter struct {
	http.ResponseWriter
	status    int
	buf       bytes.Buffer
	buffering bool
	direct    bool
}

func (w *wwwAuthWriter) WriteHeader(code int) {
	if w.buffering || w.direct {
		return
	}
	if code >= http.StatusBadRequest {
		w.status = code
		w.buffering = true
		return
	}
	w.direct = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *wwwAuthWriter) Write(b []byte) (int, error) {
	if w.buffering {
		return w.buf.Write(b)
	}
	w.direct = true
	return w.ResponseWriter.Write(b)
}

// finalize commits a buffered error response: it inspects the buffered body,
// attaches the WWW-Authenticate header when the upstream rules call for one,
// and writes the deferred header + body to the underlying writer.
func (w *wwwAuthWriter) finalize() {
	if !w.buffering {
		return
	}
	w.buffering = false
	w.direct = true
	if v := wwwAuthenticateValue(w.status, w.Header().Get(echo.HeaderContentType), w.buf.Bytes()); v != "" {
		w.Header().Set("WWW-Authenticate", v)
	}
	w.ResponseWriter.WriteHeader(w.status)
	if w.buf.Len() > 0 {
		_, _ = w.ResponseWriter.Write(w.buf.Bytes())
	}
}

// Flush implements http.Flusher. A flush during buffering commits the error
// response first so streaming writers cannot deadlock on a held-back header.
func (w *wwwAuthWriter) Flush() {
	w.finalize()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker so WebSocket upgrades keep working if this
// middleware ever wraps an upgradable route.
func (w *wwwAuthWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.finalize()
	w.direct = true
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("wwwauth: underlying writer does not support hijacking")
}

// quoteAuthParam escapes s for use inside an RFC 7235 quoted-string.
//
// **upstream は無加工でテンプレートに挿入する** (`error_description="${err.message}"`)。
// `"` が 1 つ入るだけで quoted-string が閉じ、以降が別の auth-param として
// 読まれるので、クライアントは error_description を取り違える。実測では
// mk-go にも upstream にも「利用者が制御する文字列がここへ届く経路」は
// 見つからなかった (message は全てソース中のリテラル) が、**エラー文言に
// 変数を混ぜた瞬間に注入になる**構造なので、出す側で閉じておく。
//
// 意図的な upstream 乖離 (docs/divergence.md)。
func quoteAuthParam(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			// quoted-pair で逃がす (RFC 7230 §3.2.6)。
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// 制御文字は quoted-string に置けない。改行は net/http が
			// space へ潰すが、それ以外は潰されないのでここで落とす。
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// wwwAuthenticateValue returns the WWW-Authenticate value for one error
// response, or "" when no header is due. The branch order replicates
// upstream `#sendApiError` (401 check first, then RATE_LIMIT_EXCEEDED, then
// kind). error_description は quoteAuthParam で escape してから埋め込む
// (upstream は無加工。quoteAuthParam の GoDoc 参照)。
func wwwAuthenticateValue(status int, contentType string, body []byte) string {
	if !strings.HasPrefix(contentType, echo.MIMEApplicationJSON) {
		return ""
	}
	var env apiErrorEnvelope
	if json.Unmarshal(body, &env) != nil || env.Error == nil || env.Error.Kind == "" {
		return ""
	}
	e := env.Error
	// UNKNOWN_API_ENDPOINT は body に kind:'client' を持つが、本家では
	// ApiServerService の catch-all が reply.send 直書きするため
	// #sendApiError を通らず WWW-Authenticate は付かない。
	if e.Code == "UNKNOWN_API_ENDPOINT" {
		return ""
	}
	switch {
	case status == http.StatusUnauthorized:
		// 本家 #sendAuthenticationError 相当。mk-go は現状 token 不正を
		// anonymous 扱いに落とすため AUTHENTICATION_FAILED の emission は
		// 無いが、追加時に header だけ drift しないよう先に揃えておく。
		if e.Code == "AUTHENTICATION_FAILED" {
			return `Bearer realm="Misskey", error="invalid_token", error_description="` + quoteAuthParam(e.Message) + `"`
		}
		return `Bearer realm="Misskey"`
	case e.Code == "RATE_LIMIT_EXCEEDED":
		return ""
	case e.Kind == apierr.KindClient:
		return `Bearer realm="Misskey", error="invalid_request", error_description="` + quoteAuthParam(e.Message) + `"`
	case e.Kind == apierr.KindPermission && e.Code == "PERMISSION_DENIED":
		return `Bearer realm="Misskey", error="insufficient_scope", error_description="` + quoteAuthParam(e.Message) + `"`
	}
	return ""
}
