package meta

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callMeta invokes the handler once and returns the raw response bytes plus
// the Content-Type header, so tests can compare cached and uncached output at
// the byte level.
func callMeta(t *testing.T, h *Handler, body string) (int, string, []byte) {
	t.Helper()
	e := echo.New()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/api/meta", nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/api/meta", strings.NewReader(body))
	}
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))
	return rec.Code, rec.Header().Get(echo.HeaderContentType), rec.Body.Bytes()
}

// cache を通した応答が、通さなかったときと **bytes まで** 一致すること。
// 末尾の改行 (echo の json.Encoder.Encode が付ける) と Content-Type も含めて
// 見る。ここがずれると drop-in 互換の diff harness に出る。
func TestMeta_CachedResponseIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"detailed (default)", ""},
		{"detailed explicit", `{"detail":true}`},
		{"lite", `{"detail":false}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, metaRepo := newTestHandler()
			metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("Test")}

			// 1 回目は miss なので組み立てて marshal する。
			code1, ct1, body1 := callMeta(t, h, tc.body)
			// 2 回目は hit。
			code2, ct2, body2 := callMeta(t, h, tc.body)
			assert.Equal(t, code1, code2)
			assert.Equal(t, ct1, ct2)
			assert.Equal(t, string(body1), string(body2))

			// cache を落とすと再構築されるが、やはり同じ bytes になる。
			h.InvalidateResponseCache()
			code3, ct3, body3 := callMeta(t, h, tc.body)
			assert.Equal(t, code1, code3)
			assert.Equal(t, ct1, ct3)
			assert.Equal(t, string(body1), string(body3))

			assert.True(t, strings.HasSuffix(string(body1), "\n"),
				"echo の Encoder に合わせて末尾に改行を付ける")
		})
	}
}

// cache 経由の bytes が、同じ map を c.JSON に通したときの bytes と一致すること。
//
// **ここで比較しているのは echo 標準の serializer。** 本番は
// server.fastJSONSerializer に差し替わっており、api/meta からは internal/server
// を import できない (依存は server → api/meta の一方向) ので、本番 serializer
// との契約は internal/server 側の
// TestFastJSONSerializer_OutputIsMarshalPlusNewline が固定している。両方揃って
// 初めて「cache した bytes == 本番が書く bytes」になる。
func TestMeta_CachedBytesMatchEchoJSON(t *testing.T) {
	for _, detail := range []bool{true, false} {
		h, metaRepo := newTestHandler()
		metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("Test")}

		body := ""
		if !detail {
			body = `{"detail":false}`
		}
		_, gotCT, gotBody := callMeta(t, h, body)

		// 同じ map を echo にそのまま書かせる。
		resp, err := h.buildMeta(detail)
		require.NoError(t, err)
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/api/meta", nil)
		rec := httptest.NewRecorder()
		require.NoError(t, e.NewContext(req, rec).JSON(http.StatusOK, resp))

		assert.Equal(t, rec.Header().Get(echo.HeaderContentType), gotCT,
			"detail=%v: Content-Type が c.JSON と一致すること", detail)
		assert.Equal(t, rec.Body.String(), string(gotBody),
			"detail=%v: bytes が c.JSON と一致すること", detail)
	}
}

// cache hit ではリポジトリを叩かない。**この PR の目的そのもの**なので、
// レスポンスの中身ではなく呼び出し回数で直接固定する。レスポンスだけ cache から
// 返して裏で毎回組み立て直す実装は、body の契約を一切壊さないため他のテストでは
// 原理的に捕まらない。
func TestMeta_CacheHitDoesNotHitRepository(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	_, _, _ = callMeta(t, h, "")
	require.Equal(t, 1, metaRepo.FetchCalls)

	_, _, _ = callMeta(t, h, "")
	_ = callMetaPretty(t, h)
	assert.Equal(t, 1, metaRepo.FetchCalls, "hit では meta を引き直さない")

	// invalidate したら当然引き直す。
	h.InvalidateResponseCache()
	_, _, _ = callMeta(t, h, "")
	assert.Equal(t, 2, metaRepo.FetchCalls)
}

// detail の 2 変種は別々にキャッシュする。片方の応答がもう片方に混ざらない。
func TestMeta_CacheIsPerDetailVariant(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	_, _, detailed := callMeta(t, h, `{"detail":true}`)
	_, _, lite := callMeta(t, h, `{"detail":false}`)
	assert.Contains(t, string(detailed), `"features"`)
	assert.NotContains(t, string(lite), `"features"`)

	// もう一度ずつ引いても入れ替わらない。
	_, _, detailed2 := callMeta(t, h, `{"detail":true}`)
	_, _, lite2 := callMeta(t, h, `{"detail":false}`)
	assert.Equal(t, string(detailed), string(detailed2))
	assert.Equal(t, string(lite), string(lite2))
}

// meta が変わっても cache を落とすまでは古い応答が返り、落とせば反映される。
// これが「落とし忘れると stale が TTL 分残る」ことの裏返しなので、
// invalidation の配線が効いていることをここで固定する。
func TestMeta_InvalidateResponseCacheReflectsMetaChange(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("before")}
	_, _, first := callMeta(t, h, "")
	assert.Contains(t, string(first), "before")

	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("after")}
	_, _, cached := callMeta(t, h, "")
	assert.Contains(t, string(cached), "before", "invalidate するまでは cache が返る")

	h.InvalidateResponseCache()
	_, _, fresh := callMeta(t, h, "")
	assert.Contains(t, string(fresh), "after")
}

// TTL を過ぎたら再構築する。
func TestMeta_CacheExpires(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("before")}

	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	h.respCache.now = func() time.Time { return now }
	h.respCache.ttl = 5 * time.Second

	_, _, first := callMeta(t, h, "")
	assert.Contains(t, string(first), "before")

	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("after")}
	now = now.Add(4 * time.Second)
	_, _, stillCached := callMeta(t, h, "")
	assert.Contains(t, string(stillCached), "before", "TTL 内は cache")

	now = now.Add(2 * time.Second) // 通算 6 秒
	_, _, expired := callMeta(t, h, "")
	assert.Contains(t, string(expired), "after", "TTL を過ぎたら再構築")
}

// callMetaPretty invokes the handler with ?pretty.
func callMetaPretty(t *testing.T, h *Handler) []byte {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta?pretty", nil)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	require.NoError(t, h.Meta(e.NewContext(req, rec)))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.Bytes()
}

// ?pretty は echo が indent 付きで書くので、cache 済みの compact bytes を
// そのまま返してはいけない。かといって cache を素通しにもしない (/api/meta は
// 未認証で叩けて per-endpoint の rate limit も無いので、素通しだと ?pretty を
// 付けるだけで誰でもキャッシュを外せる)。cache から整形して返す。
func TestMeta_PrettyIsIndentedAndMatchesEchoJSON(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("Test")}

	got := callMetaPretty(t, h)
	assert.Contains(t, string(got), "\n  ", "?pretty は indent 付きで返す")

	// echo に同じ map を ?pretty で書かせた結果と bytes 一致すること。
	resp, err := h.buildMeta(true)
	require.NoError(t, err)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/meta?pretty", nil)
	rec := httptest.NewRecorder()
	require.NoError(t, e.NewContext(req, rec).JSON(http.StatusOK, resp))
	assert.Equal(t, rec.Body.String(), string(got))
}

// ?pretty のリクエストが cache を汚染しない。汚染すると、pretty を 1 本
// 投げるだけで以降 TTL 分すべてのクライアントに indent 付きが返る。
func TestMeta_PrettyDoesNotPoisonCache(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x"}

	_ = callMetaPretty(t, h)
	_, _, normal := callMeta(t, h, "")
	assert.NotContains(t, string(normal), "\n  ", "通常のリクエストは compact のまま")
}

// ?pretty は cache を **読む**。ここが緩いと「pretty のときだけ再構築する
// (put はする)」形の実装で M3 の穴が黙って戻る。/api/meta は未認証で叩けて
// per-endpoint の rate limit も無いので、pretty を付けるだけで毎回
// buildMeta + ad.ListActive + system_account の DB を叩かせられてしまう。
func TestMeta_PrettyReadsCache(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("before")}

	// 通常リクエストで cache を温める。
	_, _, warm := callMeta(t, h, "")
	assert.Contains(t, string(warm), "before")

	// meta を差し替えても、cache から読んでいれば古い値が返る。
	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("after")}
	got := callMetaPretty(t, h)
	assert.Contains(t, string(got), "before", "?pretty も cache から読む")

	// invalidate すれば当然新しくなる。
	h.InvalidateResponseCache()
	assert.Contains(t, string(callMetaPretty(t, h)), "after")
}

// ?pretty が最初の 1 本でも cache は温まる (バイパスしていない)。
func TestMeta_PrettyFillsCache(t *testing.T) {
	h, metaRepo := newTestHandler()
	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("before")}

	_ = callMetaPretty(t, h)

	// cache に入っていれば、meta を差し替えても invalidate するまでは古いまま。
	metaRepo.Meta = &model.Meta{ID: "x", Name: strPtr("after")}
	_, _, normal := callMeta(t, h, "")
	assert.Contains(t, string(normal), "before", "?pretty のリクエストが cache を埋める")
}

// TTL の短さがこの cache の安全性の根拠 (invalidation を取りこぼしても数秒で
// 追いつく) なので、定数を伸ばすときはここで気付けるようにしておく。
func TestMetaResponseCacheTTL_StaysShort(t *testing.T) {
	assert.LessOrEqual(t, metaResponseCacheTTL, 10*time.Second,
		"TTL を伸ばすなら invalidation の網羅性を先に上げること")
}

// 期限は「経過 >= TTL で切れる」。境界を緩めると stale が 1 tick 延びる。
func TestMetaResponseCache_ExpiryBoundary(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	c := &metaResponseCache{now: func() time.Time { return now }, ttl: 5 * time.Second}
	c.putIfCurrent(true, []byte("x"), 0)

	now = now.Add(5*time.Second - time.Nanosecond)
	body, _ := c.get(true)
	assert.NotNil(t, body, "TTL 未満は hit")

	now = now.Add(time.Nanosecond) // ちょうど TTL
	body, _ = c.get(true)
	assert.Nil(t, body, "経過が TTL ちょうどなら miss")
}

// 組み立て中に invalidate が走ったら、その結果は捨てる。これが無いと
// 更新前のレスポンスが TTL 分キャッシュに居座る。
func TestMetaResponseCache_DiscardsFillAfterInvalidate(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	c := &metaResponseCache{now: func() time.Time { return now }}
	_, gen := c.get(true) // 組み立て開始前の世代を控える
	c.invalidate()        // 組み立て中に admin が meta を更新した
	c.putIfCurrent(true, []byte("stale"), gen)

	body, _ := c.get(true)
	assert.Nil(t, body, "invalidate をまたいだ書き込みは捨てる")

	// 世代が一致していれば普通に入る。
	_, gen2 := c.get(true)
	c.putIfCurrent(true, []byte("fresh"), gen2)
	body, _ = c.get(true)
	assert.Equal(t, "fresh", string(body))
}

// SetXxx はレスポンスの中身を変えるので cache を落とす。3 つとも見る。
func TestMeta_SettersInvalidateCache(t *testing.T) {
	t.Run("SetChunkedUploadCapability", func(t *testing.T) {
		h, metaRepo := newTestHandler()
		metaRepo.Meta = &model.Meta{ID: "x"}
		// policies にも chunkedUploadMaxPendingMb 等があるので、トップレベルの
		// キーとして一致を見る。
		_, _, before := callMeta(t, h, "")
		assert.NotContains(t, string(before), `"chunkedUpload":`)

		h.SetChunkedUploadCapability(func() (int64, bool) { return 1024, true })
		_, _, after := callMeta(t, h, "")
		assert.Contains(t, string(after), `"chunkedUpload":{"chunkSize":1024}`)
	})

	t.Run("SetProxyAccountResolver", func(t *testing.T) {
		h, metaRepo := newTestHandler()
		metaRepo.Meta = &model.Meta{ID: "x"}
		_, _, before := callMeta(t, h, "")
		assert.Contains(t, string(before), `"proxyAccountName":null`)

		h.SetProxyAccountResolver(func() (string, bool) { return "proxy", true })
		_, _, after := callMeta(t, h, "")
		assert.Contains(t, string(after), `"proxyAccountName":"proxy"`)
	})

	t.Run("SetAdRepo", func(t *testing.T) {
		h, metaRepo := newTestHandler()
		metaRepo.Meta = &model.Meta{ID: "x"}
		_, _, before := callMeta(t, h, "")
		assert.Contains(t, string(before), `"ads":[]`)

		h.SetAdRepo(&stubAdRepo{ads: []*model.Ad{{
			ID: "ad1", URL: "https://x", ImageURL: "https://y", Place: "square", Ratio: 1,
		}}})
		_, _, after := callMeta(t, h, "")
		assert.Contains(t, string(after), `"ad1"`)
	})
}

func strPtr(s string) *string { return &s }
