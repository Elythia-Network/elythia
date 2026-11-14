package paramshadow

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
)

// benchRequests are the bodies of representative requests, shaped like the
// ones upstream's frontend sends.
var benchRequests = []struct{ endpoint, body string }{
	{"notes/create", `{"visibility":"public","cw":null,"localOnly":false,"reactionAcceptance":null,` +
		`"noExtractMentions":false,"noExtractHashtags":false,"noExtractEmojis":false,"replyId":null,` +
		`"renoteId":null,"channelId":null,"text":"` + strings.Repeat("今日はいい天気ですね。#misskey :smile: ", 8) + `",` +
		`"fileIds":["a1b2c3d4e5f6g7h8","a1b2c3d4e5f6g7h9"],"poll":null,"i":"0123456789abcdef0123456789abcdef"}`},
	{"notes/timeline", `{"limit":10,"untilId":"a1b2c3d4e5f6g7h8","allowPartial":true,"withRenotes":true,` +
		`"withFiles":false,"i":"0123456789abcdef0123456789abcdef"}`},
	{"i/update", `{"name":"しろは","description":"` + strings.Repeat("自己紹介の文です。", 40) + `",` +
		`"location":"Tokyo","birthday":"2000-01-01","lang":"ja-JP","isLocked":false,"isExplorable":true,` +
		`"hideOnlineStatus":false,"publicReactions":true,"carefulBot":false,"autoAcceptFollowed":true,` +
		`"noCrawle":false,"preventAiLearning":true,"isBot":false,"isCat":true,"injectFeaturedNote":true,` +
		`"receiveAnnouncementEmail":true,"alwaysMarkNsfw":false,"autoSensitive":false,` +
		`"followingVisibility":"public","followersVisibility":"public","chatScope":"mutual",` +
		`"fields":[{"name":"Website","value":"https://example.com"},{"name":"Fediverse","value":"@a@remote.example"}],` +
		`"mutedWords":[["foo"],["bar","baz"],"/regex/i"],"hardMutedWords":[["spam"]],` +
		`"mutedInstances":["remote.example"],"emailNotificationTypes":["follow","receiveFollowRequest"],` +
		`"notificationRecieveConfig":{"follow":{"type":"all"},"mention":{"type":"following"},` +
		`"reply":{"type":"all"},"renote":{"type":"list","userListId":"a1b2c3d4e5f6g7h8"}},` +
		`"i":"0123456789abcdef0123456789abcdef"}`},
}

func benchEcho(b *testing.B, eps map[string]paramschema.Endpoint, endpoint string, shadow bool) *echo.Echo {
	b.Helper()
	e := echo.New()
	var mws []echo.MiddlewareFunc
	if shadow {
		mws = append(mws, Middleware(eps))
	}
	// handlerはbodyを読み切って200を返すだけにし、差を検証の分にする。
	e.POST("/api/"+endpoint, func(c echo.Context) error {
		_, _ = io.Copy(io.Discard, c.Request().Body)
		return c.NoContent(http.StatusOK)
	}, mws...)
	return e
}

// BenchmarkShadow measures one request through the route with and without
// the middleware. The difference is what the middleware adds to a request.
func BenchmarkShadow(b *testing.B) {
	eps, err := paramschema.Upstream()
	if err != nil {
		b.Fatal(err)
	}
	for _, r := range benchRequests {
		if _, ok := eps[r.endpoint]; !ok {
			b.Fatalf("%s is not an upstream endpoint", r.endpoint)
		}
		for _, shadow := range []bool{false, true} {
			name := r.endpoint + "/off"
			if shadow {
				name = r.endpoint + "/on"
			}
			b.Run(name, func(b *testing.B) {
				e := benchEcho(b, eps, r.endpoint, shadow)
				b.ReportAllocs()
				b.SetBytes(int64(len(r.body)))
				for b.Loop() {
					req := httptest.NewRequest(http.MethodPost, "/api/"+r.endpoint, strings.NewReader(r.body))
					req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
					e.ServeHTTP(httptest.NewRecorder(), req)
				}
			})
		}
	}
}

// TestBenchRequestsAreAccepted keeps the benchmark honest: its bodies must be
// valid upstream, so the benchmark measures the full validation rather than
// an early first error.
func TestBenchRequestsAreAccepted(t *testing.T) {
	eps, err := paramschema.Upstream()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range benchRequests {
		in := jsonInput([]byte(r.body))
		if in.skip != "" {
			t.Fatalf("%s: skip %s", r.endpoint, in.skip)
		}
		if e := eps[r.endpoint].Schema.Validate(in.data); e != nil {
			t.Errorf("%s: %v", r.endpoint, e)
		}
		// 上限を超えるbodyは検証せずに通すので、測っても検証の費用にならない。
		if len(r.body) > DefaultMaxBody || tooManyValues([]byte(r.body), DefaultMaxValues) {
			t.Errorf("%s: the body is over the limits and is not validated", r.endpoint)
		}
	}
}

// enforcedWorstBodies are the costliest bodies clips/notes validates, around
// the token limit DefaultMaxValues, and the costliest string body: \u
// escapes up to DefaultMaxBody.
func enforcedWorstBodies() []struct{ name, body string } {
	arr := func(objs int) string {
		return `{"clipId":"a","x":[{}` + strings.Repeat(",{}", objs-1) + `]}`
	}
	keys := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"clipId":"a","x":{`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"k%05d":0`, i)
		}
		b.WriteString(`}}`)
		return b.String()
	}
	escaped := func(size int) string {
		head, tail := `{"clipId":"a","x":"`, `"}`
		return head + strings.Repeat(`\u3042`, (size-len(head)-len(tail))/6) + tail
	}
	// {"clipId":"a","x":[ ... ]} の外側のトークンは7。
	at := (DefaultMaxValues - 7) / 2
	return []struct{ name, body string }{
		{"values-at-limit", arr(at)},
		{"keys-at-limit", keys(at)},
		{"values-over-limit", arr(at + 1)},
		{"escaped-string-at-limit", escaped(DefaultMaxBody)},
		{"escaped-string-64KiB", escaped(64 << 10)},
	}
}

// BenchmarkEnforcedWorstCase measures enforcedWorstBodies on clips/notes
// with and without the middleware. The validation has neither a budget nor
// a concurrency limit, so its cost per request is bounded by the limits
// alone (#3742).
func BenchmarkEnforcedWorstCase(b *testing.B) {
	eps, err := paramschema.Upstream()
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range enforcedWorstBodies() {
		for _, on := range []bool{false, true} {
			name := c.name + "/off"
			if on {
				name = c.name + "/on"
			}
			b.Run(name, func(b *testing.B) {
				e := echo.New()
				var mws []echo.MiddlewareFunc
				if on {
					mws = append(mws, Middleware(eps))
				}
				e.POST("/api/clips/notes", func(c echo.Context) error {
					_, _ = io.Copy(io.Discard, c.Request().Body)
					return c.NoContent(http.StatusOK)
				}, mws...)
				b.ReportAllocs()
				b.SetBytes(int64(len(c.body)))
				for b.Loop() {
					req := httptest.NewRequest(http.MethodPost, "/api/clips/notes", strings.NewReader(c.body))
					req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
					e.ServeHTTP(httptest.NewRecorder(), req)
				}
			})
		}
	}
}

// TestEnforcedWorstBodiesHitTheLimits keeps BenchmarkEnforcedWorstCase
// honest: each body lands on the side of the limits its name says.
func TestEnforcedWorstBodiesHitTheLimits(t *testing.T) {
	for _, c := range enforcedWorstBodies() {
		over := tooManyValues([]byte(c.body), DefaultMaxValues) || len(c.body) > DefaultMaxBody
		if over != (strings.Contains(c.name, "over") || strings.Contains(c.name, "64KiB")) || !json.Valid([]byte(c.body)) {
			t.Errorf("%s (%d bytes): over the limits %v", c.name, len(c.body), over)
		}
		// 大きさで名付けたbodyは、その大きさの近くまで埋まっていること(escapeの
		// つもりが1文字の文字になっていて、半分の大きさで測っていた。#3742の
		// レビュー2周目M-1)。
		for _, size := range []struct {
			tag  string
			size int
		}{{"at-limit", DefaultMaxBody}, {"64KiB", 64 << 10}} {
			if strings.HasPrefix(c.name, "escaped-string-") && strings.Contains(c.name, size.tag) &&
				(len(c.body) > size.size || len(c.body) <= size.size-6) {
				t.Errorf("%s is %d bytes, want just under %d", c.name, len(c.body), size.size)
			}
		}
	}
}
