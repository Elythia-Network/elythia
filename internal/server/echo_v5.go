package server

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// anyMethods are the methods echo v4's Any registered a route for (#4239).
//
// echo の v5 の Any は、全メソッドに合う 1 本のルート (echo.RouteAny) を登録する。
// ルートの一覧 (/api/endpoints、dump-routes、openapi) にメソッドとして出ず、
// 表に無いメソッド (MKCOL など) も handler に届き、405 の Allow も変わる。v4 と
// 同じ応答に保つため、Any の代わりに v4 の Any と同じメソッドを Match で登録する。
var anyMethods = []string{
	http.MethodConnect,
	http.MethodDelete,
	http.MethodGet,
	http.MethodHead,
	http.MethodOptions,
	http.MethodPatch,
	http.MethodPost,
	"PROPFIND",
	http.MethodPut,
	http.MethodTrace,
	"REPORT",
}

// echoResponse returns echo's Response under c.Response(), or nil when a
// writer in between does not unwrap to it.
//
// echo の v5 では c.Response() が http.ResponseWriter を返し、gzip や c.JSON が
// その間に自分の writer を挟む (#4239)。Before の hook や Status は、その下にある
// echo の Response のものを使う。
//
// **Before の hook は、全体の gzip と WWW-Authenticate の判定より後に走る。** v4 では
// echo の Response が一番外側にいて、handler の WriteHeader の時点で走っていた。
// v5 では Response が一番内側に来るので、gzip が圧縮するかを決め、Content-Length を
// 消した後になる。その判定に効くヘッダ (Content-Type・Content-Encoding・
// Content-Length) は、hook ではなく handler の中で付けること。
func echoResponse(c *echo.Context) *echo.Response {
	res, err := echo.UnwrapResponse(c.Response())
	if err != nil {
		return nil
	}
	return res
}
