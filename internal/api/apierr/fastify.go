// Fastify-style error response helper. upstream Misskey TS の signup /
// signin 系 endpoint (= ApiServerService 経由ではなく Fastify post で直接
// register され、`throw new FastifyReplyError(status, code)` で error を
// 投げる経路) が返す reply shape を再現する。
//
// shape 例 (DUPLICATED_USERNAME):
//
//	{"statusCode":400,"error":"Bad Request","message":"Error: DUPLICATED_USERNAME"}
//
// `error` は status code に対する http.StatusText、`message` は Node.js
// `Error.toString()` の output (= "Error: <message>") を反映するため
// `"Error: " + code` で組み立てる。
//
// drop-in 互換上、Fastify-style 化が必要な endpoint 候補は 4 つ:
//   - POST /api/signup            ← #802 Phase 1 で対応済 (= 本ファイルの初版)
//   - POST /api/signup-pending    ← Phase 2 (status drift も並行で別途整理)
//   - POST /api/signin-flow       ← Phase 3 (rate-limit / captcha 等の混在 shape を整理)
//   - POST /api/signin-with-passkey ← Phase 3
//
// それ以外の `/api/*` endpoint は upstream 上流でも Misskey misc 形式を
// 返すため、引き続き Error()/ErrorEcho() を使うこと。

package apierr

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// FastifyReply writes a Fastify-style error reply to the given context.
// `code` is the upstream error code string (e.g. "DUPLICATED_USERNAME").
func FastifyReply(c *echo.Context, status int, code string) error {
	return c.JSON(status, fastifyReplyBody{
		StatusCode: status,
		Error:      http.StatusText(status),
		Message:    "Error: " + code,
	})
}

type fastifyReplyBody struct {
	StatusCode int    `json:"statusCode"`
	Error      string `json:"error"`
	Message    string `json:"message"`
}
