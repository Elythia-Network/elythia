package server

import (
	"bufio"
	"net"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"
	echomw "github.com/labstack/echo/v4/middleware"
)

// gzipMiddleware returns the global response compression middleware: echo's
// gzip with gzipConfig(), except that responses which gain nothing from it
// (see skipGzipForResponse) are written to the client untouched.
//
// echo の gzip は Content-Type を見ず、MinLength を超えた応答を全て圧縮する。
// /files や /proxy の画像・動画は既に圧縮されているので縮まず CPU だけを使い、
// Content-Length を消し、Range の 206 まで Content-Encoding: gzip で包む (#3966)。
// 経路では決めない。画像を返す経路は /files・/proxy のほかに identicon・
// twemoji-badge・/assets・プラグインのルートなどがあり、名前で並べると漏れる。
// 本家はアプリの側で圧縮しない。
func gzipMiddleware() echo.MiddlewareFunc {
	gz := echomw.GzipWithConfig(gzipConfig())
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			raw := c.Response().Writer
			return gz(func(c echo.Context) error {
				res := c.Response()
				wrapped := res.Writer
				if wrapped == raw {
					// Skipper に当たったか、クライアントが gzip を受け取らない。
					return next(c)
				}
				sw := &gzipSwitchWriter{gz: wrapped, raw: raw}
				res.Writer = sw
				err := next(c)
				sw.finish()
				return err
			})(c)
		}
	}
}

// gzipSwitchWriter holds the status line until the handler writes its body,
// then sends the response either through echo's gzip writer or straight to
// the client, depending on the response headers.
//
// echo の gzip writer は WriteHeader の時点で Content-Length を消すので、
// 判定がつくまで WriteHeader を預かる。直接書いた応答では gzip writer に何も
// 渡らないので、echo の gzip middleware は後始末で何も書かない。
type gzipSwitchWriter struct {
	gz  http.ResponseWriter
	raw http.ResponseWriter

	target  http.ResponseWriter
	code    int
	pending bool
}

func (w *gzipSwitchWriter) Header() http.Header { return w.raw.Header() }

func (w *gzipSwitchWriter) WriteHeader(code int) {
	if w.target != nil {
		w.target.WriteHeader(code)
		return
	}
	w.code = code
	w.pending = true
}

func (w *gzipSwitchWriter) Write(b []byte) (int, error) {
	if w.target == nil {
		if w.Header().Get(echo.HeaderContentType) == "" {
			// echo の gzip writer と同じく、無ければ本文から推定して付ける。
			// 推定した型でも判定するため、ここで付ける。
			w.Header().Set(echo.HeaderContentType, http.DetectContentType(b))
		}
		w.decide()
	}
	return w.target.Write(b)
}

// Flush sends what is buffered so far. Streaming responses (SSE など) are
// decided here when they flush before writing a body.
func (w *gzipSwitchWriter) Flush() {
	if w.target == nil {
		w.decide()
	}
	_ = http.NewResponseController(w.target).Flush()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *gzipSwitchWriter) Unwrap() http.ResponseWriter {
	if w.target != nil {
		return w.target
	}
	return w.gz
}

// Hijack takes over the connection, for callers that type-assert
// http.Hijacker instead of using http.ResponseController.
//
// echo の gzip writer は Hijack を持つので、包む前は型アサーションで取れていた。
// 内側の middleware (wwwauth など) はアサーションで下の writer を呼ぶので、
// ここで持たないと gzip の対象になる経路で接続を引き取れなくなる (#3966 のレビュー)。
func (w *gzipSwitchWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.Unwrap()).Hijack()
}

// finish forwards a status line the handler set without writing a body
// (a redirect, a 304, a HEAD request).
func (w *gzipSwitchWriter) finish() {
	if w.target == nil && w.pending {
		w.decide()
	}
}

func (w *gzipSwitchWriter) decide() {
	code := w.code
	if !w.pending {
		code = http.StatusOK
	}
	w.target = w.gz
	if skipGzipForResponse(code, w.Header()) {
		w.target = w.raw
	}
	if w.pending {
		w.target.WriteHeader(code)
	}
}

// skipGzipForResponse reports whether a response must be sent without gzip:
// already encoded bodies, partial content, and media that is compressed by
// its own format (image / video / audio except SVG).
//
// 206 は Content-Range が元の表現のバイト位置を指すので、本文だけを gzip で
// 包むと関係が崩れる。SVG は XML のテキストなので今どおり圧縮する。
func skipGzipForResponse(code int, h http.Header) bool {
	if code == http.StatusPartialContent || h.Get(echo.HeaderContentEncoding) != "" {
		return true
	}
	// mime.ParseMediaType は引数が壊れているとエラーを返し、型が空になる
	// こともある。圧縮するかは型だけで決まるので、`;` の前だけを見る。
	mediaType, _, _ := strings.Cut(h.Get(echo.HeaderContentType), ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType == "image/svg+xml" {
		return false
	}
	return strings.HasPrefix(mediaType, "image/") ||
		strings.HasPrefix(mediaType, "video/") ||
		strings.HasPrefix(mediaType, "audio/")
}
