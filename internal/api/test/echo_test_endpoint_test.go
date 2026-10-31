package test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func callTest(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, NewEchoHandler().Test(e.NewContext(req, rec)))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	return got
}

// upstream `test.ts` は API framework のパラメータ検証そのものを写す endpoint。
// 本家の backend e2e (`test/e2e/endpoints.ts`) がここを叩くので、既定値と
// null の扱いを 1 つずつ固定する。
func TestTestEndpoint(t *testing.T) {
	t.Run("required only fills in both defaults", func(t *testing.T) {
		got := decode(t, callTest(t, `{"required":true}`))

		assert.Equal(t, true, got["required"])
		assert.Equal(t, "hello", got["default"])
		assert.Equal(t, "hello", got["nullableDefault"])
		// 省略した optional はキーごと出ない。
		assert.NotContains(t, got, "string")
		assert.NotContains(t, got, "id")
	})

	t.Run("explicit values win over defaults", func(t *testing.T) {
		got := decode(t, callTest(t, `{"required":false,"string":"s","default":"d","nullableDefault":"n","id":"abc"}`))

		assert.Equal(t, false, got["required"])
		assert.Equal(t, "s", got["string"])
		assert.Equal(t, "d", got["default"])
		assert.Equal(t, "n", got["nullableDefault"])
		assert.Equal(t, "abc", got["id"])
	})

	t.Run("explicit null keeps null, it does not fall back", func(t *testing.T) {
		// **キー省略とは別扱い。** 省略なら "hello"、`null` ならそのまま null。
		// ここを同一視すると upstream の nullable セマンティクスが壊れる。
		rec := callTest(t, `{"required":true,"nullableDefault":null}`)
		got := decode(t, rec)

		require.Contains(t, got, "nullableDefault")
		assert.Nil(t, got["nullableDefault"])
	})

	t.Run("missing required is a 400", func(t *testing.T) {
		rec := callTest(t, `{}`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	})

	t.Run("empty id is a 400", func(t *testing.T) {
		// format:'misskey:id' は空文字を許さない。
		rec := callTest(t, `{"required":true,"id":""}`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("non-string nullableDefault is a 400", func(t *testing.T) {
		rec := callTest(t, `{"required":true,"nullableDefault":123}`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("unknown fields are echoed back as is", func(t *testing.T) {
		// 本家は`return ps`なので、宣言に無い項目もそのまま返す (#3369)。
		got := decode(t, callTest(t, `{"required":true,"foo":1,"bar":{"x":[true]},"i":"token"}`))

		assert.Equal(t, float64(1), got["foo"])
		assert.Equal(t, map[string]any{"x": []any{true}}, got["bar"])
		assert.Equal(t, "token", got["i"])
		assert.Equal(t, "hello", got["default"], "known fields keep their handling")
	})

	t.Run("unreadable body is a 400", func(t *testing.T) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", iotest.ErrReader(errors.New("boom")))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		require.NoError(t, NewEchoHandler().Test(e.NewContext(req, rec)))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("malformed body is a 400", func(t *testing.T) {
		rec := callTest(t, `{`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}
