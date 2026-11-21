package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// doJSONBody runs one request through JSONBodyParse with a handler that
// binds {"value": string} and echoes it back, mimicking how API handlers
// answer Bind failures with the Misskey INVALID_PARAM envelope.
func doJSONBody(t *testing.T, method, contentType, body string) (*httptest.ResponseRecorder, *bool) {
	t.Helper()
	e := echo.New()
	handlerRan := false
	h := func(c *echo.Context) error {
		handlerRan = true
		var req struct {
			Value string `json:"value"`
		}
		if err := c.Bind(&req); err != nil {
			return c.JSON(http.StatusBadRequest, map[string]any{
				"error": map[string]any{
					"message": "Invalid param.",
					"code":    "INVALID_PARAM",
					"id":      "3d81ceae-475f-4600-b2a8-2bc116157532",
				},
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"value": req.Value})
	}
	req := httptest.NewRequest(method, "/api/test", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	err := JSONBodyParse()(h)(c)
	require.NoError(t, err)
	return rec, &handlerRan
}

const (
	invalidEnvelope = `{"statusCode":400,"code":"FST_ERR_CTP_INVALID_JSON_BODY","error":"Bad Request","message":"Body is not valid JSON but content-type is set to 'application/json'"}`
	emptyEnvelope   = `{"statusCode":400,"code":"FST_ERR_CTP_EMPTY_JSON_BODY","error":"Bad Request","message":"Body cannot be empty when content-type is set to 'application/json'"}`
)

func TestJSONBodyParse_MalformedJSONReturnsFastifyEnvelope(t *testing.T) {
	rec, ran := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, "{not json")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, invalidEnvelope, rec.Body.String())
	assert.False(t, *ran, "handler must not run on parse error")
}

func TestJSONBodyParse_EmptyBodyReturnsEmptyJSONEnvelope(t *testing.T) {
	rec, ran := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, emptyEnvelope, rec.Body.String())
	assert.False(t, *ran)
}

func TestJSONBodyParse_ValidJSONPassesThroughToBind(t *testing.T) {
	rec, ran := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, `{"value":"hello"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, *ran)
	// middleware が body を復元して下流の c.Bind が読めること
	assert.JSONEq(t, `{"value":"hello"}`, rec.Body.String())
}

// TestJSONBodyParse_TypeMismatchStaysInvalidParam は #1609 の区別要件:
// 構文的に正しいJSONの型ミスマッチは middleware を通過し、handler 側の
// INVALID_PARAM (= 本家の schema validation エラー) で返ることを固定する。
func TestJSONBodyParse_TypeMismatchStaysInvalidParam(t *testing.T) {
	rec, ran := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, `{"value":123}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.True(t, *ran, "type mismatch must reach the handler")
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.NotContains(t, rec.Body.String(), "FST_ERR")
}

func TestJSONBodyParse_CharsetParameterIsStillJSON(t *testing.T) {
	rec, ran := doJSONBody(t, http.MethodPost, "application/json; charset=utf-8", "{broken")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, invalidEnvelope, rec.Body.String())
	assert.False(t, *ran)
}

func TestJSONBodyParse_NonJSONContentTypePassesThrough(t *testing.T) {
	for _, ct := range []string{"text/plain", "multipart/form-data; boundary=x", "application/x-www-form-urlencoded"} {
		rec, ran := doJSONBody(t, http.MethodPost, ct, "{definitely not json")
		// Fastifyの対象外content-typeはJSON parserを通らない。handler 側の
		// 既存挙動 (echo Bind の可否) はこの middleware の関心外。
		assert.True(t, *ran, "content-type %s must pass through", ct)
		assert.NotContains(t, rec.Body.String(), "FST_ERR", "content-type %s", ct)
	}
}

func TestJSONBodyParse_NoContentTypePassesThrough(t *testing.T) {
	_, ran := doJSONBody(t, http.MethodPost, "", "{garbage")
	assert.True(t, *ran)
}

func TestJSONBodyParse_BodylessMethodsSkipBodyParsing(t *testing.T) {
	// FastifyのbodylessセットはGET/HEAD/TRACE (fastify.js bodylessMethods)
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodTrace} {
		_, ran := doJSONBody(t, m, echo.MIMEApplicationJSON, "{garbage")
		assert.True(t, *ran, "method %s must skip body parsing", m)
	}
}

// TestJSONBodyParse_MalformedContentTypeParams は Fastify が content-type の
// パラメータ構文不正を無視して essence で JSON parser を選ぶ挙動の互換性を
// 固定する。Go の mime.ParseMediaType はこれらをエラーにするため、essence
// 自前抽出の fallback が無いと素通しして本家とずれる (#1609 レビュー指摘)。
func TestJSONBodyParse_MalformedContentTypeParams(t *testing.T) {
	jsonCTs := []string{
		"application/json; charset",          // 値なしパラメータ
		"application/json; foo=bar; foo=baz", // 重複パラメータ
		`application/json; charset="utf-8`,   // 未終端 quote
		"application/json; =nokey",           // キー無し
	}
	for _, ct := range jsonCTs {
		rec, ran := doJSONBody(t, http.MethodPost, ct, "{broken")
		assert.Equal(t, http.StatusBadRequest, rec.Code, "content-type %q", ct)
		assert.JSONEq(t, invalidEnvelope, rec.Body.String(), "content-type %q", ct)
		assert.False(t, *ran, "content-type %q", ct)
	}
	// type/subtype 自体が壊れている場合は fastify でも JSON parser に
	// 到達しない (415系) ので素通し (mk-go 既存挙動) を維持する。
	_, ran := doJSONBody(t, http.MethodPost, "application / json", "{broken")
	assert.True(t, *ran, "broken type/subtype must pass through")
}

// TestJSONBodyParse_BOMStrippedAndBindable は secure-json-parse の BOM 除去
// 互換: UTF-8 BOM 付き有効JSONを受理し、下流の c.Bind にはBOM抜きの body
// が渡ることを確認する (Goの encoding/json はBOMを受理しないため必須)。
func TestJSONBodyParse_BOMStrippedAndBindable(t *testing.T) {
	rec, ran := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, "\xEF\xBB\xBF{\"value\":\"bom\"}")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.True(t, *ran)
	assert.JSONEq(t, `{"value":"bom"}`, rec.Body.String())
}

func TestJSONBodyParse_ScalarJSONIsValid(t *testing.T) {
	// JSON.parse は scalar も受理する。middleware は通過させ、Bind の失敗は
	// handler 側 INVALID_PARAM に委ねる。
	rec, ran := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, `123`)
	assert.True(t, *ran)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
}

func TestJSONBodyParse_ProtoPoisoningRejected(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool // true = rejected with invalid envelope
	}{
		{name: "top-level __proto__ key", body: `{"__proto__":{"x":1}}`, want: true},
		{name: "nested __proto__ key", body: `{"a":{"__proto__":1}}`, want: true},
		{name: "__proto__ inside array element", body: `{"a":[{"__proto__":1}]}`, want: true},
		// JSON escape 形式のキーも decode 後に "__proto__" になるため reject
		// される (本家 suspectProtoRx も _ 形式を検出する)
		{name: "escaped unicode proto key", body: `{"\u005f\u005fproto\u005f\u005f":1}`, want: true},
		{name: "__proto__ as string value is fine", body: `{"a":"__proto__"}`, want: false},
		{name: "constructor with prototype", body: `{"constructor":{"prototype":{}}}`, want: true},
		{name: "nested constructor with prototype", body: `{"a":{"constructor":{"prototype":1}}}`, want: true},
		{name: "constructor with scalar value is fine", body: `{"constructor":"x"}`, want: false},
		{name: "constructor object without prototype is fine", body: `{"constructor":{"x":1}}`, want: false},
		// float64 範囲外の数値は JSON.parse では Infinity として成功する。
		// UseNumber decode で誤って poisoned 扱いしないこと
		{name: "constructor with out-of-range number is fine", body: `{"constructor":1e999}`, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, ran := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, tc.body)
			if tc.want {
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.JSONEq(t, invalidEnvelope, rec.Body.String())
				assert.False(t, *ran)
			} else {
				assert.True(t, *ran, "body %s must pass", tc.body)
				assert.NotContains(t, rec.Body.String(), "FST_ERR")
			}
		})
	}
}

func TestJSONBodyParse_EnvelopeFieldOrderMatchesFastify(t *testing.T) {
	// Fastifyの生成serializerは statusCode, code, error, message の順で
	// 出力する。drop-in 比較で見やすいよう同順を維持する (JSONとしては
	// 順序非依存だが、念のため raw 文字列でも固定する)。
	rec, _ := doJSONBody(t, http.MethodPost, echo.MIMEApplicationJSON, "{broken")
	raw := strings.TrimSpace(rec.Body.String())
	assert.Equal(t, invalidEnvelope, raw)
}

func TestHasPoisonedKey_NonObjectValues(t *testing.T) {
	assert.False(t, hasPoisonedKey("string"))
	assert.False(t, hasPoisonedKey(123.0))
	assert.False(t, hasPoisonedKey(nil))
	assert.False(t, hasPoisonedKey([]any{"a", 1.0}))
	assert.True(t, hasPoisonedKey([]any{map[string]any{"__proto__": 1.0}}))
}

// errReader simulates a request body whose read fails mid-stream.
type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, assert.AnError }

func TestJSONBodyParse_BodyReadErrorReturnsInvalidEnvelope(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/test", errReader{})
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	handlerRan := false
	err := JSONBodyParse()(func(*echo.Context) error {
		handlerRan = true
		return nil
	})(c)
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.JSONEq(t, invalidEnvelope, rec.Body.String())
	assert.False(t, handlerRan)
}

func TestIsPoisonedJSON_UnmarshalErrorIsPoisoned(t *testing.T) {
	// json.Valid 通過後にしか呼ばれない前提だが、防御分岐として suspect
	// パターンを含む不正JSONは poisoned 扱いにする
	assert.True(t, isPoisonedJSON([]byte(`{"__proto__":`)))
}

func TestIsPoisonedJSON_FastPathSkipsWalk(t *testing.T) {
	// 怪しいパターンを含まない body は正規表現スキャンのみで通過する
	assert.False(t, isPoisonedJSON([]byte(`{"text":"hello world"}`)))
	// 文字列値中の escaped-quote 形式は正規表現にもマッチしない (JSの
	// suspectProtoRx と同じ挙動)
	assert.False(t, isPoisonedJSON([]byte(`{"text":"say \"__proto__\": hi"}`)))
	// 正規表現にはマッチするが walk で安全と判定されるケース
	assert.False(t, isPoisonedJSON([]byte(`{"constructor":"x"}`)))
}
