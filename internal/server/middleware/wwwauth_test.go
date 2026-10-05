package middleware

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/apierr"
)

// doWWWAuth runs one request through the WWWAuthenticate middleware with the
// given handler and returns the recorded response.
func doWWWAuth(t *testing.T, h echo.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/test", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	require.NoError(t, WWWAuthenticate()(h)(c))
	return rec
}

func TestWWWAuthenticate_ClientKindGetsInvalidRequest(t *testing.T) {
	// upstream #sendApiError: kind 'client' -> error="invalid_request" with
	// the error message as error_description.
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam())
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_request", error_description="Invalid param."`,
		rec.Header().Get("WWW-Authenticate"))
	// body は無加工で透過する。
	assert.JSONEq(t,
		`{"error":{"message":"Invalid param.","code":"INVALID_PARAM","id":"3d81ceae-475f-4600-b2a8-2bc116157532","kind":"client"}}`,
		rec.Body.String())
}

func TestWWWAuthenticate_ClientKind404AlsoGetsHeader(t *testing.T) {
	// 401 以外の status では kind が判定基準。404 でも kind 'client' なら
	// invalid_request が付く (upstream は httpStatusCode 404 を明示する
	// NO_SUCH_* でも kind=client 分岐を通る)。
	rec := doWWWAuth(t, func(c echo.Context) error {
		return apierr.JSONNoSuchNote(c)
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_request", error_description="No such note."`,
		rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_PermissionDeniedGetsInsufficientScope(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusForbidden, apierr.PermissionDenied())
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t,
		`Bearer realm="Misskey", error="insufficient_scope", error_description="Your app does not have the necessary permissions to use this endpoint."`,
		rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_RolePermissionDeniedGetsNoHeader(t *testing.T) {
	// upstream #sendApiError は kind 'permission' のうち code が
	// PERMISSION_DENIED のときだけヘッダを付ける (ROLE_PERMISSION_DENIED は
	// 「(ROLE_PERMISSION_DENIEDは関係ない)」と明示コメントされている)。
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusForbidden, apierr.RolePermissionDenied())
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_ServerKindGetsNoHeader(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_Status401GetsRealmOnly(t *testing.T) {
	// 401 は kind 分岐より先に評価され、realm のみのヘッダになる
	// (CREDENTIAL_REQUIRED は kind client だが invalid_request にはならない)。
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusUnauthorized, apierr.Error(
			"CREDENTIAL_REQUIRED", "Credential required.",
			"1384574d-a912-4b81-8601-c7b1c4085df1"))
	})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Equal(t, `Bearer realm="Misskey"`, rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_AuthenticationFailedGetsInvalidToken(t *testing.T) {
	// upstream #sendAuthenticationError 相当の分岐。
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusUnauthorized, apierr.Error(
			"AUTHENTICATION_FAILED",
			"Authentication failed. Please ensure your token is correct.",
			"b0a7f5f8-dc2f-4171-b91f-de88ad238e14"))
	})
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_token", error_description="Authentication failed. Please ensure your token is correct."`,
		rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_RateLimitExceededGetsNoHeader(t *testing.T) {
	// upstream は RATE_LIMIT_EXCEEDED を Retry-After 分岐で処理し
	// WWW-Authenticate は付けない。Retry-After は rate limiter が設定済の
	// ものを透過する。
	rec := doWWWAuth(t, func(c echo.Context) error {
		c.Response().Header().Set("Retry-After", "30")
		return c.JSON(http.StatusTooManyRequests, apierr.RateLimitExceeded())
	})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.Equal(t, "30", rec.Header().Get("Retry-After"))
}

func TestWWWAuthenticate_UnknownAPIEndpointGetsNoHeader(t *testing.T) {
	// UNKNOWN_API_ENDPOINT は body に kind:'client' を持つが、本家は
	// reply.send 直書きで #sendApiError を通らないためヘッダ無し。
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusNotFound, apierr.Error(
			"UNKNOWN_API_ENDPOINT", "Unknown API endpoint.",
			"2ca3b769-540a-4f08-9dd5-b5a825b6d0f1"))
	})
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_FastifyEnvelopeGetsNoHeader(t *testing.T) {
	// signup/signin 系の Fastify-style envelope や FST_ERR_CTP_* は kind を
	// 持たないので対象外 (upstream でも ApiCallService を通らない経路)。
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusBadRequest, map[string]any{
			"statusCode": 400,
			"error":      "Bad Request",
			"message":    "Error: DUPLICATED_USERNAME",
		})
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.JSONEq(t,
		`{"statusCode":400,"error":"Bad Request","message":"Error: DUPLICATED_USERNAME"}`,
		rec.Body.String())
}

func TestWWWAuthenticate_SparseSigninEnvelopeGetsNoHeader(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusForbidden, map[string]any{
			"error": map[string]any{"id": "932c904e-9460-45b7-9ce6-7ed33be7eb2c"},
		})
	})
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_NonJSONErrorPassesThrough(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.String(http.StatusBadRequest, "bad request")
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.Equal(t, "bad request", rec.Body.String())
}

func TestWWWAuthenticate_SuccessResponsePassesThrough(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusOK, map[string]any{"ok": true})
	})
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.JSONEq(t, `{"ok":true}`, rec.Body.String())
}

func TestWWWAuthenticate_NoContentPassesThrough(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.NoContent(http.StatusNoContent)
	})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
}

func TestWWWAuthenticate_EmptyErrorBodyPassesThrough(t *testing.T) {
	// c.NoContent(4xx) のような body 無し error はヘッダ無しで status を
	// そのまま透過する。
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.NoContent(http.StatusBadRequest)
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.Zero(t, rec.Body.Len())
}

func TestWWWAuthenticate_HandlerErrorFallsThroughToEchoHandler(t *testing.T) {
	// handler が応答を書かず error を返した場合、echo の HTTPErrorHandler が
	// middleware chain の外で応答を書く。writer を元に戻しているので応答が
	// 消えないことを確認する (echo 標準の {"message":...} shape、ヘッダ無し)。
	e := echo.New()
	e.POST("/api/test", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusBadRequest, "bind failed")
	}, WWWAuthenticate())
	req := httptest.NewRequest(http.MethodPost, "/api/test", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Empty(t, rec.Header().Get("WWW-Authenticate"))
	assert.JSONEq(t, `{"message":"bind failed"}`, rec.Body.String())
}

func TestWWWAuthenticate_MultiWriteErrorBodyIsConcatenated(t *testing.T) {
	// echo の c.JSON は 1 write だが、手書き handler が複数回 Write しても
	// buffer 経由で結合されて正しく書き戻ることを確認する。
	rec := doWWWAuth(t, func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		c.Response().WriteHeader(http.StatusForbidden)
		if _, err := c.Response().Write([]byte(`{"error":{"message":"m","code":"PERMISSION_DENIED",`)); err != nil {
			return err
		}
		_, err := c.Response().Write([]byte(`"id":"x","kind":"permission"}}`))
		return err
	})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t,
		`Bearer realm="Misskey", error="insufficient_scope", error_description="m"`,
		rec.Header().Get("WWW-Authenticate"))
	assert.JSONEq(t, `{"error":{"message":"m","code":"PERMISSION_DENIED","id":"x","kind":"permission"}}`, rec.Body.String())
}

func TestWWWAuthenticate_HijackOnNonHijackableWriterFails(t *testing.T) {
	// httptest.ResponseRecorder は http.Hijacker 未実装なので error 経路に
	// 落ちる。WebSocket upgrade 互換の interface 実装が壊れていないことの
	// 確認 (実 hijack は e2e 経路で検証される)。
	rec := doWWWAuth(t, func(c echo.Context) error {
		hj, ok := c.Response().Writer.(interface {
			Hijack() (net.Conn, *bufio.ReadWriter, error)
		})
		require.True(t, ok, "wwwAuthWriter must implement http.Hijacker")
		_, _, err := hj.Hijack()
		assert.Error(t, err)
		return c.NoContent(http.StatusOK)
	})
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestWWWAuthenticate_FlushDuringErrorCommitsBufferedBody(t *testing.T) {
	// streaming 系が error 中に Flush しても deadlock せず即時 commit する。
	rec := doWWWAuth(t, func(c echo.Context) error {
		if err := c.JSON(http.StatusBadRequest, apierr.InvalidParam()); err != nil {
			return err
		}
		c.Response().Flush()
		return nil
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_request", error_description="Invalid param."`,
		rec.Header().Get("WWW-Authenticate"))
	assert.True(t, rec.Flushed)
}

// --- quoted-string の閉じ (セキュリティ監査 2026-09-21) ---

// **`"` で quoted-string を閉じさせない。**
//
// upstream は `error_description="${err.message}"` と無加工で埋めるので、
// message に `"` が 1 つ入るだけで値が途中で閉じ、以降が別の auth-param と
// して読まれる。実測では利用者が制御する文字列がここへ届く経路は無かったが、
// エラー文言に変数を混ぜた瞬間に注入になるので出す側で閉じておく。
func TestWWWAuthenticate_MessageQuotesAreEscaped(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", `a" error="x`, "3d81ceae-475f-4600-b2a8-2bc116157532"))
	})
	// 期待値はリテラルで書く (escape 関数を参照すると、escape を緩める変異と
	// 一緒に期待値も動いて空虚になる)。
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_request", error_description="a\" error=\"x"`,
		rec.Header().Get("WWW-Authenticate"))
}

// **3 経路すべてで escape する。** `error_description` を出すのは
// invalid_token (401) / invalid_request (client) / insufficient_scope の 3 つで、
// 1 つだけ外す変異が素通りしないようにここで全部見る。
func TestWWWAuthenticate_EveryDescriptionPathEscapes(t *testing.T) {
	const raw = `x"y`
	const want = `x\"y`
	t.Run("invalid_token", func(t *testing.T) {
		rec := doWWWAuth(t, func(c echo.Context) error {
			return c.JSON(http.StatusUnauthorized, apierr.Error(
				"AUTHENTICATION_FAILED", raw, "b0a7f5f8-dc2f-4171-b91f-de88ad238e14"))
		})
		assert.Equal(t,
			`Bearer realm="Misskey", error="invalid_token", error_description="`+want+`"`,
			rec.Header().Get("WWW-Authenticate"))
	})
	t.Run("invalid_request", func(t *testing.T) {
		rec := doWWWAuth(t, func(c echo.Context) error {
			return c.JSON(http.StatusBadRequest, apierr.Error(
				"INVALID_PARAM", raw, "3d81ceae-475f-4600-b2a8-2bc116157532"))
		})
		assert.Equal(t,
			`Bearer realm="Misskey", error="invalid_request", error_description="`+want+`"`,
			rec.Header().Get("WWW-Authenticate"))
	})
	t.Run("insufficient_scope", func(t *testing.T) {
		rec := doWWWAuth(t, func(c echo.Context) error {
			return c.JSON(http.StatusForbidden, apierr.ErrorWithKind(
				"PERMISSION_DENIED", raw, "1370e5b7-d4eb-4566-bb1d-7748ad9a1bfd",
				apierr.KindPermission))
		})
		assert.Equal(t,
			`Bearer realm="Misskey", error="insufficient_scope", error_description="`+want+`"`,
			rec.Header().Get("WWW-Authenticate"))
	})
}

// **バックスラッシュも逃がす。** 逃がさないと、末尾の `\` が閉じ引用符を
// quoted-pair にしてしまい、やはり値が閉じない。
func TestWWWAuthenticate_MessageBackslashIsEscaped(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", `end\`, "3d81ceae-475f-4600-b2a8-2bc116157532"))
	})
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_request", error_description="end\\"`,
		rec.Header().Get("WWW-Authenticate"))
}

// **制御文字は quoted-string に置けない。**
func TestWWWAuthenticate_MessageControlCharsAreStripped(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "a\x01b\x7fc", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	})
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_request", error_description="a b c"`,
		rec.Header().Get("WWW-Authenticate"))
}

// **普通の文言は素通しする。** 上 3 つが「全部落とす」実装でも緑にならない
// ようにする。日本語 (multi-byte) がバイト単位の処理で壊れないことも見る。
func TestWWWAuthenticate_PlainMessageIsUntouched(t *testing.T) {
	rec := doWWWAuth(t, func(c echo.Context) error {
		return c.JSON(http.StatusBadRequest, apierr.Error(
			"INVALID_PARAM", "パラメータが不正です (a-b_c).", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	})
	assert.Equal(t,
		`Bearer realm="Misskey", error="invalid_request", error_description="パラメータが不正です (a-b_c)."`,
		rec.Header().Get("WWW-Authenticate"))
}
