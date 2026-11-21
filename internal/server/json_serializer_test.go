package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withSerializer prepares an *echo.Context with the fastJSONSerializer
// installed, mimicking what server.New does at boot. Currently backed by
// stdlib encoding/json (#542 で goccy 0.10.x の VM compile bug が
// timeline 経路で踏まれたため一時的に巻き戻し)。
func withSerializer(body string) (*echo.Context, *httptest.ResponseRecorder) {
	e := echo.New()
	e.JSONSerializer = fastJSONSerializer{}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	return c, rec
}

func TestFastJSONSerializer_SerializeMatchesStdlib(t *testing.T) {
	c, rec := withSerializer("")
	payload := map[string]any{"name": "alice", "n": 42, "ok": true}

	require.NoError(t, c.JSON(http.StatusOK, payload))

	assert.Equal(t, http.StatusOK, rec.Code)
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "alice", got["name"])
	assert.Equal(t, float64(42), got["n"])
	assert.Equal(t, true, got["ok"])
}

// /api/meta は serialize 済みの bytes をキャッシュして c.JSONBlob で返す
// (#2649)。その bytes は `json.Marshal(v)` に改行を足したもので、`?pretty` は
// それを `json.Indent` で整形したものとして組み立てている。つまり
// **この serializer の出力形式がその前提そのもの**なので、ここで固定する。
//
// gate を meta 側に置けないのは、api/meta が internal/server を import できない
// ため (逆向きの依存)。serializer を差し替えるときはこのテストが落ちる。
func TestFastJSONSerializer_OutputIsMarshalPlusNewline(t *testing.T) {
	payload := map[string]any{
		"name": "alice",
		"n":    42,
		"ok":   true,
		"html": "<a>&</a>", // HTML escape の有無まで見る
		"arr":  []any{1, nil, "x"},
		"obj":  map[string]any{"k": "v"},
	}

	c, rec := withSerializer("")
	require.NoError(t, c.JSON(http.StatusOK, payload))
	want, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.Equal(t, string(append(want, '\n')), rec.Body.String())

	// ?pretty 相当。echo の defaultIndent は 2 スペース。
	c2, rec2 := withSerializer("")
	require.NoError(t, c2.JSONPretty(http.StatusOK, payload, "  "))
	var indented bytes.Buffer
	require.NoError(t, json.Indent(&indented, append(want, '\n'), "", "  "))
	assert.Equal(t, indented.String(), rec2.Body.String())
}

func TestFastJSONSerializer_SerializeIndent(t *testing.T) {
	c, rec := withSerializer("")
	require.NoError(t, c.JSONPretty(http.StatusOK, map[string]any{"x": 1}, "  "))
	assert.Contains(t, rec.Body.String(), "\n", "indent should produce newlines")
}

func TestFastJSONSerializer_DeserializeOK(t *testing.T) {
	c, _ := withSerializer(`{"name":"bob","n":7}`)
	var dst struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	require.NoError(t, c.Bind(&dst))
	assert.Equal(t, "bob", dst.Name)
	assert.Equal(t, 7, dst.N)
}

// 不正な JSON は echo.HTTPError(400) に翻訳されること (stdlib 実装と同じ)。
func TestFastJSONSerializer_DeserializeSyntaxError(t *testing.T) {
	c, _ := withSerializer(`{"name": "bob"`) // closing brace 無し
	var dst struct {
		Name string `json:"name"`
	}
	err := c.Bind(&dst)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok, "want *echo.HTTPError, got %T", err)
	assert.Equal(t, http.StatusBadRequest, httpErr.Code)
}

// 型不一致 (string 期待のフィールドに number) も同様に 400 になること。
func TestFastJSONSerializer_DeserializeTypeError(t *testing.T) {
	c, _ := withSerializer(`{"name": 42}`)
	var dst struct {
		Name string `json:"name"`
	}
	err := c.Bind(&dst)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok, "want *echo.HTTPError, got %T", err)
	assert.Equal(t, http.StatusBadRequest, httpErr.Code)
}

// TestSerialize_PrettyQueryIndents pins that ?pretty indents the JSON the way
// echo v4's Context.JSON did, and that the answer is compact otherwise
// (#4239). echo の v5 の Context.JSON は ?pretty を見ないので、serializer が
// 字下げする。
func TestSerialize_PrettyQueryIndents(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   string
	}{
		{"/api/x?pretty", "{\n  \"a\": 1\n}\n"},
		{"/api/x?pretty=0", "{\n  \"a\": 1\n}\n"},
		{"/api/x", "{\"a\":1}\n"},
	} {
		e := echo.New()
		e.JSONSerializer = fastJSONSerializer{}
		rec := httptest.NewRecorder()
		c := e.NewContext(httptest.NewRequest(http.MethodPost, tc.target, nil), rec)
		require.NoError(t, c.JSON(http.StatusOK, map[string]int{"a": 1}))
		assert.Equal(t, tc.want, rec.Body.String(), tc.target)
	}
}
