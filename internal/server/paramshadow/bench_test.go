package paramshadow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		// 1件あたりの費用を測るので、予算は外す(続けて流すと予算に当たり、
		// ほとんどの件を飛ばした値になる)。
		mws = append(mws, Middleware(eps, NewRecorder(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour),
			WithBudget(1e9, time.Hour)))
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
	}
}

// worstBodies are the bodies that cost the decode the most per byte, around
// the two limits: arrays of empty objects (and an object of many keys) in an
// unknown key, padded with spaces to their size.
func worstBodies() []struct{ name, body string } {
	arr := func(objs, size int) string {
		b := `{"text":"a","x":[{}` + strings.Repeat(",{}", objs-1) + `]}`
		return b + strings.Repeat(" ", max(0, size-len(b)))
	}
	// {"text":"a","x":[ ... ]} の外側のトークンは7。{}は1つで2。
	atValues := (DefaultMaxValues - 7) / 2
	keys := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"text":"a","x":{`)
		for i := range n {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"k%05d":0`, i)
		}
		b.WriteString(`}}`)
		return b.String()
	}
	// 外側のトークンは7、キーと値で2。
	atKeys := (DefaultMaxValues - 7) / 2
	return []struct{ name, body string }{
		{"values-at-limit", arr(atValues, 0)},
		{"keys-at-limit", keys(atKeys)},
		{"values-at-limit-padded", arr(atValues, DefaultMaxBody)},
		{"values-over-limit", arr(atValues+1, 0)},
		{"size-at-limit", arr((DefaultMaxBody-21)/3, DefaultMaxBody)},
		{"size-over-limit", arr((DefaultMaxBody-21)/3, DefaultMaxBody+1)},
		{"1MiB", arr((1<<20-64)/3, 1<<20-64)},
	}
}

// BenchmarkShadowWorstCase measures worstBodies with and without the
// validation. The heaviest one validated is at the token limit; the others
// are counted as too_many_values or too_large without decoding.
func BenchmarkShadowWorstCase(b *testing.B) {
	eps, err := paramschema.Upstream()
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range worstBodies() {
		for _, shadow := range []bool{false, true} {
			name := c.name + "/off"
			if shadow {
				name = c.name + "/on"
			}
			b.Run(name, func(b *testing.B) {
				e := benchEcho(b, eps, "notes/create", shadow)
				b.ReportAllocs()
				b.SetBytes(int64(len(c.body)))
				for b.Loop() {
					req := httptest.NewRequest(http.MethodPost, "/api/notes/create", strings.NewReader(c.body))
					req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
					e.ServeHTTP(httptest.NewRecorder(), req)
				}
			})
		}
	}
}

// TestWorstBodiesHitTheLimits keeps BenchmarkShadowWorstCase honest: each
// body lands on the side of the limits its name says.
func TestWorstBodiesHitTheLimits(t *testing.T) {
	want := map[string]string{
		"values-at-limit": "", "keys-at-limit": "", "values-at-limit-padded": "",
		"values-over-limit": SkipTooManyValues, "size-at-limit": SkipTooManyValues,
		"size-over-limit": SkipTooLarge, "1MiB": SkipTooLarge,
	}
	for _, c := range worstBodies() {
		var got string
		switch {
		case len(c.body) > DefaultMaxBody:
			got = SkipTooLarge
		case tooManyValues([]byte(c.body), DefaultMaxValues):
			got = SkipTooManyValues
		}
		if got != want[c.name] {
			t.Errorf("%s (%d bytes): %q, want %q", c.name, len(c.body), got, want[c.name])
		}
		if !json.Valid([]byte(c.body)) {
			t.Errorf("%s is not JSON", c.name)
		}
	}
}

// BenchmarkCountTokens compares the byte scanner of tooManyValues with
// json.Decoder.Token on 64KiB bodies.
func BenchmarkCountTokens(b *testing.B) {
	for _, c := range worstBodies() {
		if c.name != "size-at-limit" && c.name != "values-at-limit-padded" {
			continue
		}
		body := []byte(c.body)
		b.Run(c.name+"/scanner", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tooManyValues(body, DefaultMaxValues)
			}
		})
		b.Run(c.name+"/decoder-token", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				dec := json.NewDecoder(bytes.NewReader(body))
				dec.UseNumber()
				for n := 0; n <= DefaultMaxValues; n++ {
					if _, err := dec.Token(); err != nil {
						break
					}
				}
			}
		})
	}
}
