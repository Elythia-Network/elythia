package paramshadow

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// enforcedResult is what one request to an enforcing middleware produced.
type enforcedResult struct {
	rec    *httptest.ResponseRecorder
	called bool
	// read is the body the handler read.
	read string
}

// serveEnforced routes one request through the middleware (with opts) to a
// handler that answers 200.
func serveEnforced(t *testing.T, opts []Option, method, target, contentType, body string) enforcedResult {
	t.Helper()
	e := echo.New()
	var res enforcedResult
	h := func(c echo.Context) error {
		res.called = true
		b, _ := io.ReadAll(c.Request().Body)
		res.read = string(b)
		return ok(c)
	}
	mw := Middleware(testEndpoints(t), opts...)
	for _, p := range []string{"/api/t/post", "/api/t/get", "/api/t/file", "/api/t/broken"} {
		e.Match([]string{http.MethodGet, http.MethodPost, http.MethodPut}, p, h, mw)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set(echo.HeaderContentType, contentType)
	}
	res.rec = httptest.NewRecorder()
	e.ServeHTTP(res.rec, req)
	return res
}

func wantInvalidParam(t *testing.T, res enforcedResult, param, reason string) {
	t.Helper()
	if res.called {
		t.Error("the handler was called for a rejected request")
	}
	if res.rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", res.rec.Code)
	}
	want, err := json.Marshal(apierr.InvalidParamClient(param, reason))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(res.rec.Body.String()); got != string(want) {
		t.Errorf("body\n got %s\nwant %s", got, want)
	}
}

func wantHandler(t *testing.T, res enforcedResult, body string) {
	t.Helper()
	if !res.called || res.rec.Code != http.StatusOK {
		t.Errorf("handler called %v, status %d; want the handler's 200", res.called, res.rec.Code)
	}
	if res.read != body {
		t.Errorf("the handler read %q, want %q", res.read, body)
	}
}

func TestEnforcedRejectsWithUpstreamsAnswer(t *testing.T) {
	for _, tt := range []struct {
		name, body, param, reason string
	}{
		{"format", `{"noteId":"a-b"}`, "#/properties/noteId/format", `must match format "misskey:id"`},
		{"required", `{}`, "#/required", "must have required property 'noteId'"},
		{"null for a non-nullable property", `{"noteId":"abc","flag":null}`, "#/properties/flag/type", "must be boolean"},
		{"maximum", `{"noteId":"abc","limit":101}`, "#/properties/limit/maximum", "must be <= 100"},
		{"not an object", `[]`, "#/type", "must be object"},
		// 対になっていないサロゲートと不正なUTF-8は、U+FFFDに置き換えた値で判定する
		// (2026-10-31の監査)。飛ばすと1バイト混ぜるだけで拒否の検証を迂回できた。
		{"lone surrogate", `{"noteId":"\ud800"}`, "#/properties/noteId/format", `must match format "misskey:id"`},
		{"invalid utf-8", "{\"noteId\":\"abc\",\"limit\":101,\"x\":\"\xff\"}", "#/properties/limit/maximum", "must be <= 100"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantInvalidParam(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, tt.body), tt.param, tt.reason)
		})
	}
}

// TestEnforcedAcceptedGoesToTheHandlerUnchanged checks that an accepted
// request reaches the handler with its own body: the default the validator
// inserts (limit: 10) stays in the validator's copy.
func TestEnforcedAcceptedGoesToTheHandlerUnchanged(t *testing.T) {
	for _, body := range []string{`{"noteId":"abc"}`, `{"noteId":"abc","limit":10.0}`} {
		wantHandler(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, body), body)
	}
}

// TestEnforcedLeavesWhatItCannotValidateToTheHandler covers the inputs the
// validator cannot judge like upstream: they go to the handler, whose own
// checks stay, rather than being rejected without upstream's verdict.
func TestEnforcedLeavesWhatItCannotValidateToTheHandler(t *testing.T) {
	large := `{"noteId":"a-b","pad":"` + strings.Repeat("x", 100) + `"}`
	for _, tt := range []struct {
		name                        string
		opts                        []Option
		method, target, ctype, body string
	}{
		{"too large", []Option{WithMaxBody(64)}, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, large},
		{"too many values", []Option{WithMaxValues(4)}, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"a-b","x":[1,2,3]}`},
		{"not json", nil, http.MethodPost, "/api/t/post", echo.MIMETextPlain, `{"noteId":"a-b"}`},
		{"no content type", nil, http.MethodPost, "/api/t/post", "", `{"noteId":"a-b"}`},
		{"empty json body", nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, ""},
		{"requireFile", nil, http.MethodPost, "/api/t/file", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`},
		{"GET without allowGet", nil, http.MethodGet, "/api/t/post?noteId=a-b", "", ""},
		// JSONBodyParseが先に弾くので本番では届かないが、decodeできないbodyと
		// url.ParseQueryが拒否するqueryも、判定を持たないのでhandlerへ渡す。
		{"undecodable json", nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":`},
		{"unparsable query", nil, http.MethodGet, "/api/t/get?noteId=a-b;x", "", ""},
		{"method", nil, http.MethodPut, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`},
		{"GET cast failure", nil, http.MethodGet, "/api/t/get?noteId=abc&limit=x", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			wantHandler(t, serveEnforced(t, tt.opts, tt.method, tt.target, tt.ctype, tt.body), tt.body)
		})
	}
	// GETの検証そのものは効く(変換を通った値が本家の検証で落ちる)。
	res := serveEnforced(t, nil, http.MethodGet, "/api/t/get?noteId=abc&limit=0", "", "")
	wantInvalidParam(t, res, "#/properties/limit/minimum", "must be >= 1")
}

func TestEnforcedPanicGoesToTheHandler(t *testing.T) {
	orig := validate
	t.Cleanup(func() { validate = orig })
	validate = func(*paramschema.Schema, any) *paramschema.Error { panic("validator bug") }
	wantHandler(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`), `{"noteId":"a-b"}`)
}

func TestEnforcedCompileErrorGoesToTheHandler(t *testing.T) {
	wantHandler(t, serveEnforced(t, nil, http.MethodPost, "/api/t/broken", echo.MIMEApplicationJSON, `{"noteId":"a-b"}`), `{"noteId":"a-b"}`)
}

// TestEnforcedTokenLimit pins the token limit: a body at the limit is
// validated, and one token more goes to the handler unvalidated.
func TestEnforcedTokenLimit(t *testing.T) {
	// {"noteId":"a-b","x":[0,0,...]} のトークンは、括弧4つとキー2つと値1つの
	// 7に、配列の要素の数を足したもの。
	body := func(tokens int) string {
		return `{"noteId":"a-b","x":[` + strings.TrimSuffix(strings.Repeat("0,", tokens-7), ",") + `]}`
	}
	at, over := body(DefaultMaxValues), body(DefaultMaxValues+1)
	if tooManyValues([]byte(at), DefaultMaxValues) || !tooManyValues([]byte(over), DefaultMaxValues) {
		t.Fatal("the bodies do not sit on the limit")
	}
	wantInvalidParam(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, at),
		"#/properties/noteId/format", `must match format "misskey:id"`)
	res := serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, over)
	wantHandler(t, res, over)

	// GETのqueryにも同じ上限が掛かる。
	q := "/api/t/get?noteId=a-b" + strings.Repeat("&x=0", DefaultMaxValues)
	wantHandler(t, serveEnforced(t, nil, http.MethodGet, q, "", ""), "")
	// 上限は差し替えられる(上限2に対して3項目)。
	small := serveEnforced(t, []Option{WithMaxValues(2)}, http.MethodGet, "/api/t/get?noteId=a-b&limit=1&x=1", "", "")
	wantHandler(t, small, "")
}

// TestEnforcedSizeLimit pins the size limit: a body of exactly the limit is
// validated, and one byte more goes to the handler unvalidated.
func TestEnforcedSizeLimit(t *testing.T) {
	body := func(size int) string {
		head, tail := `{"noteId":"a-b","pad":"`, `"}`
		return head + strings.Repeat("x", size-len(head)-len(tail)) + tail
	}
	at, over := body(DefaultMaxBody), body(DefaultMaxBody+1)
	wantInvalidParam(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, at),
		"#/properties/noteId/format", `must match format "misskey:id"`)
	wantHandler(t, serveEnforced(t, nil, http.MethodPost, "/api/t/post", echo.MIMEApplicationJSON, over), over)

	// GETのqueryにも同じ上限が掛かる(上限16バイトに対して17バイト)。
	q := "noteId=a-b&x=123" // 16バイト
	wantInvalidParam(t, serveEnforced(t, []Option{WithMaxBody(len(q))}, http.MethodGet, "/api/t/get?"+q, "", ""),
		"#/properties/noteId/format", `must match format "misskey:id"`)
	wantHandler(t, serveEnforced(t, []Option{WithMaxBody(len(q))}, http.MethodGet, "/api/t/get?"+q+"4", "", ""), "")
}
