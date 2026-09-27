package middleware

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- mock store ---

type mockCall struct {
	Key      string
	Duration time.Duration
	Max      int
}

type mockResult struct {
	Info LimitInfo
	Err  error
}

type mockLimitStore struct {
	calls   []mockCall
	results []mockResult
	idx     int
}

func (m *mockLimitStore) Check(_ context.Context, key string, duration time.Duration, max int) (LimitInfo, error) {
	m.calls = append(m.calls, mockCall{Key: key, Duration: duration, Max: max})
	i := m.idx
	m.idx++
	if i < len(m.results) {
		return m.results[i].Info, m.results[i].Err
	}
	return LimitInfo{Remaining: 999}, nil
}

// --- helpers ---

func setupEcho(rl *RateLimiter) (*echo.Echo, echo.HandlerFunc) {
	e := echo.New()
	handler := func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	}
	return e, handler
}

func doRequest(e *echo.Echo, mw echo.MiddlewareFunc, handler echo.HandlerFunc, path string, user *model.User) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.RemoteAddr = "192.168.1.100:12345"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath(path)
	if user != nil {
		c.Set(string(UserContextKey), user)
	}
	wrapped := mw(handler)
	_ = wrapped(c)
	return rec
}

// --- RateLimiter Middleware tests ---

func TestMiddleware_NoLimitDefined(t *testing.T) {
	store := &mockLimitStore{}
	rl := NewRateLimiter(store, true, map[string]*EndpointLimit{})
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/show", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, store.calls)
}

// Disable() を呼んだら、リミット定義があっても store を一切引かずに素通しする。
// 本家 RateLimiterService が NODE_ENV !== 'production' でやっているのと同じ。
func TestMiddleware_Disabled(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Hour).UnixMilli()}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 1},
	}
	rl := NewRateLimiter(store, true, limits)
	rl.Disable()
	e, h := setupEcho(rl)

	// 上限 1 に対して 3 回叩いても 429 にならない。
	for i := 0; i < 3; i++ {
		rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "user1"})
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Empty(t, store.calls, "無効時は store を引かないこと")
}

func TestMiddleware_WithinLimit(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 5, ResetMs: time.Now().Add(time.Hour).UnixMilli()}},
			{Info: LimitInfo{Remaining: 8, ResetMs: time.Now().Add(time.Hour).UnixMilli()}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "user1"})

	assert.Equal(t, http.StatusOK, rec.Code)
	// 認証済 → userID + IP bucket の 2 actor (#606 item 2)。X-RateLimit-Remaining
	// は最も逼迫した bucket (ここでは userID 側 5)。
	assert.Equal(t, "5", rec.Header().Get("X-RateLimit-Remaining"))
	require.Len(t, store.calls, 2)
	assert.Equal(t, "user1:notes/create", store.calls[0].Key)
	assert.Contains(t, store.calls[1].Key, "ip-")
	assert.Equal(t, time.Hour, store.calls[0].Duration)
	assert.Equal(t, 300, store.calls[0].Max)
}

func TestMiddleware_ExceedsLimit(t *testing.T) {
	resetMs := time.Now().Add(30 * time.Second).UnixMilli()
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 0, ResetMs: resetMs}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "user1"})

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "RATE_LIMIT_EXCEEDED", errObj["code"])
	assert.Equal(t, "d5826d14-3982-4d2e-8011-b9e9f02499ef", errObj["id"])
}

func TestMiddleware_AuthenticatedActor(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 10}}, // userID bucket
			{Info: LimitInfo{Remaining: 20}}, // IP bucket
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "myuserid"})

	// userID + IP bucket の 2 つに対して check が走る (#606 item 2)
	require.Len(t, store.calls, 2)
	assert.Equal(t, "myuserid:notes/create", store.calls[0].Key)
	assert.Contains(t, store.calls[1].Key, "ip-")
}

// 認証済 user の userID bucket が満杯でも IP bucket が余裕でも 429
// (どちらかのしきい値超過で reject、OR-bucket セマンティクス)。
func TestMiddleware_AuthenticatedActor_UserBucketExceeds(t *testing.T) {
	resetMs := time.Now().Add(30 * time.Second).UnixMilli()
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 0, ResetMs: resetMs}}, // userID bucket exceeded
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u1"})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Len(t, store.calls, 1, "userID bucket で reject なら IP bucket は check しない")
}

// 認証済 user の userID bucket は OK でも IP bucket が満杯なら 429
// (cross-account brute-force 防御 #606 item 2)。
func TestMiddleware_AuthenticatedActor_IPBucketExceeds(t *testing.T) {
	resetMs := time.Now().Add(30 * time.Second).UnixMilli()
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 50}},                  // userID bucket OK
			{Info: LimitInfo{Remaining: 0, ResetMs: resetMs}}, // IP bucket exceeded
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u1"})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Len(t, store.calls, 2)
}

// EnableIPRateLimit=false なら認証済でも IP bucket は走らない
// (operator 配線の意図を尊重)。
func TestMiddleware_AuthenticatedActor_IPDisabled(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 10}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, false, limits)
	e, h := setupEcho(rl)

	doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u1"})
	require.Len(t, store.calls, 1, "IP bucket は EnableIPRateLimit=false で skip")
	assert.Equal(t, "u1:notes/create", store.calls[0].Key)
}

// PolicyProvider 経由で rateLimitFactor=2.0 が反映されると Max が upstream 同様
// max/factor で scale される (#606 item 4 / #2106 N28: factor=2.0 で 1/2 に厳格化)。
func TestMiddleware_PolicyProviderScalesMax(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 100}},
			{Info: LimitInfo{Remaining: 100}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	rl.SetPolicyProvider(stubPolicy{factor: 2.0})
	e, h := setupEcho(rl)

	doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "vip"})

	require.Len(t, store.calls, 2)
	// userID bucket は factor=2.0 で 150 に scale (300/2.0)、IP bucket は factor=1.0
	assert.Equal(t, 150, store.calls[0].Max, "userID bucket: 300 / 2.0 (factor は除数)")
	assert.Equal(t, 300, store.calls[1].Max, "IP bucket は factor 適用外")
}

// factor が 0 / 負 / 不正型なら 1.0 (= base) に fallback
func TestMiddleware_PolicyProviderInvalidFactor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy stubPolicy
	}{
		{"zero", stubPolicy{factor: 0}},
		{"negative", stubPolicy{factor: -1}},
		{"missing", stubPolicy{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockLimitStore{
				results: []mockResult{{Info: LimitInfo{Remaining: 10}}, {Info: LimitInfo{Remaining: 10}}},
			}
			limits := map[string]*EndpointLimit{
				"notes/create": {Duration: time.Hour, Max: 300},
			}
			rl := NewRateLimiter(store, true, limits)
			rl.SetPolicyProvider(tc.policy)
			e, h := setupEcho(rl)
			doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u"})
			require.GreaterOrEqual(t, len(store.calls), 1)
			assert.Equal(t, 300, store.calls[0].Max, "不正 factor は 1.0 fallback")
		})
	}
}

// stubPolicy は PolicyProvider のテスト double。factor=0 なら policies map
// に rateLimitFactor を含めない (= 「未設定」を再現)。
type stubPolicy struct{ factor float64 }

func (s stubPolicy) GetUserPolicies(_ string) map[string]any {
	if s.factor == 0 {
		return map[string]any{}
	}
	return map[string]any{"rateLimitFactor": s.factor}
}

// scaledMax は upstream RateLimiterService の max/factor (除数) semantics (#2106 N28)。
// factor が大きいほど実効 max が小さく (厳格)、小さいほど大きく (緩和) なる。
func TestScaledMax(t *testing.T) {
	assert.Equal(t, 300, scaledMax(300, 1.0))
	assert.Equal(t, 150, scaledMax(300, 2.0), "factor=2.0 (200%) で 1/2 に厳格化")
	assert.Equal(t, 600, scaledMax(300, 0.5), "factor=0.5 (50%) で 2x に緩和")
	assert.Equal(t, 1, scaledMax(10, 1000.0), "極大 factor でも 1 にクランプ")
	assert.Equal(t, 300, scaledMax(300, 0), "factor=0 は 1.0 扱い (= base)")
	assert.Equal(t, 300, scaledMax(300, -1), "負数も 1.0 扱い (防御)")
	assert.Equal(t, math.MaxInt, scaledMax(300, math.SmallestNonzeroFloat64))
	assert.Equal(t, 300, scaledMax(300, math.NaN()))
}

// scaledMinInterval は upstream の minInterval*factor semantics (#2106 N28)。
func TestScaledMinInterval(t *testing.T) {
	base := 1000 * time.Millisecond
	assert.Equal(t, base, scaledMinInterval(base, 1.0))
	assert.Equal(t, 2000*time.Millisecond, scaledMinInterval(base, 2.0), "factor=2.0 で window 2x (厳格)")
	assert.Equal(t, 500*time.Millisecond, scaledMinInterval(base, 0.5), "factor=0.5 で window 半分 (緩和)")
	assert.Equal(t, base, scaledMinInterval(base, 0), "factor=0 は base")
	assert.Equal(t, base, scaledMinInterval(base, -1))
	assert.Equal(t, base, scaledMinInterval(base, math.NaN()))
	assert.Equal(t, base, scaledMinInterval(base, math.MaxFloat64))
	boundary := float64(math.MaxInt64) / float64(base)
	assert.Equal(t, base, scaledMinInterval(base, boundary))
}

func TestMiddleware_UnauthenticatedIPActor(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 10}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, store.calls, 1)
	// キーがip-プレフィックスのIPハッシュを含む
	assert.Contains(t, store.calls[0].Key, "ip-")
	assert.Contains(t, store.calls[0].Key, ":notes/create")
}

func TestMiddleware_UnauthenticatedIPDisabled(t *testing.T) {
	store := &mockLimitStore{}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, false, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, store.calls)
}

func TestMiddleware_MinIntervalCheckFirst(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 1}},  // userID: minInterval OK
			{Info: LimitInfo{Remaining: 50}}, // userID: duration/max OK
			{Info: LimitInfo{Remaining: 1}},  // IP: minInterval OK
			{Info: LimitInfo{Remaining: 50}}, // IP: duration/max OK
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/delete": {Duration: time.Hour, Max: 300, MinInterval: time.Second},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/delete", &model.User{ID: "u1"})

	assert.Equal(t, http.StatusOK, rec.Code)
	// 認証済 → userID + IP の 2 actor、各 actor で minInterval + duration の
	// 2 チェック → 計 4 calls
	require.Len(t, store.calls, 4)
	// userID actor: minInterval が先、その後 duration/max
	assert.Equal(t, "u1:notes/delete:min", store.calls[0].Key)
	assert.Equal(t, time.Second, store.calls[0].Duration)
	assert.Equal(t, 1, store.calls[0].Max)
	assert.Equal(t, "u1:notes/delete", store.calls[1].Key)
	assert.Equal(t, time.Hour, store.calls[1].Duration)
	assert.Equal(t, 300, store.calls[1].Max)
	// IP actor: 同じ順序
	assert.Contains(t, store.calls[2].Key, "ip-")
	assert.Contains(t, store.calls[2].Key, ":notes/delete:min")
	assert.Contains(t, store.calls[3].Key, ":notes/delete")
}

func TestMiddleware_MinIntervalBlock(t *testing.T) {
	resetMs := time.Now().Add(500 * time.Millisecond).UnixMilli()
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 0, ResetMs: resetMs}}, // minInterval: blocked
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/delete": {Duration: time.Hour, Max: 300, MinInterval: time.Second},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/delete", &model.User{ID: "u1"})

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	// minIntervalでブロックされたらduration/maxチェックは呼ばれない
	require.Len(t, store.calls, 1)
}

func TestMiddleware_RedisError_FailOpen(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Err: assert.AnError},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u1"})

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddleware_RedisError_MinInterval_FailOpen(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Err: assert.AnError},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/delete": {Duration: time.Hour, Max: 300, MinInterval: time.Second},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/delete", &model.User{ID: "u1"})

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMiddleware_RetryAfterHeader(t *testing.T) {
	// 30秒後にリセット
	resetMs := time.Now().Add(30 * time.Second).UnixMilli()
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 0, ResetMs: resetMs}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u1"})

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	retryAfter := rec.Header().Get("Retry-After")
	assert.NotEmpty(t, retryAfter)
	// 30±2秒の範囲であること
	var secs int
	_, _ = fmt.Sscanf(retryAfter, "%d", &secs)
	assert.InDelta(t, 30, secs, 2)
}

func TestMiddleware_NonAPIPath(t *testing.T) {
	store := &mockLimitStore{}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	// /api/のないパスはエンドポイント名が一致しないのでスルー
	rec := doRequest(e, rl.Middleware(), h, "/healthz", nil)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, store.calls)
}

// --- ipHash tests ---

func TestIpHash_IPv4(t *testing.T) {
	h := ipHash("192.168.1.1")
	assert.True(t, len(h) > 3)
	assert.True(t, h[:3] == "ip-")
}

func TestIpHash_IPv4_DifferentAddresses(t *testing.T) {
	h1 := ipHash("192.168.1.1")
	h2 := ipHash("10.0.0.1")
	assert.NotEqual(t, h1, h2)
}

func TestIpHash_IPv6(t *testing.T) {
	h := ipHash("2001:db8::1")
	assert.True(t, len(h) > 3)
	assert.True(t, h[:3] == "ip-")
}

func TestIpHash_IPv6_SameSubnet(t *testing.T) {
	// 同一/64サブネットのIPは同じハッシュ
	h1 := ipHash("2001:db8:1234:5678::1")
	h2 := ipHash("2001:db8:1234:5678::ffff")
	assert.Equal(t, h1, h2)
}

func TestIpHash_IPv6_DifferentSubnet(t *testing.T) {
	h1 := ipHash("2001:db8:1234:5678::1")
	h2 := ipHash("2001:db8:1234:9999::1")
	assert.NotEqual(t, h1, h2)
}

func TestIpHash_Invalid(t *testing.T) {
	h := ipHash("not-an-ip")
	assert.True(t, len(h) > 3)
	assert.True(t, h[:3] == "ip-")
}

func TestIpHash_Deterministic(t *testing.T) {
	h1 := ipHash("192.168.1.1")
	h2 := ipHash("192.168.1.1")
	assert.Equal(t, h1, h2)
}

// --- parseIntFromString ---

func TestParseIntFromString(t *testing.T) {
	assert.Equal(t, int64(12345), parseIntFromString("12345"))
	assert.Equal(t, int64(0), parseIntFromString(""))
	assert.Equal(t, int64(0), parseIntFromString("abc"))
}

// --- DefaultEndpointLimits ---

func TestDefaultEndpointLimits_NotEmpty(t *testing.T) {
	assert.Greater(t, len(DefaultEndpointLimits), 50)
}

func TestDefaultEndpointLimits_KnownEndpoints(t *testing.T) {
	cases := []struct {
		endpoint string
		max      int
	}{
		{"notes/create", 300},
		{"following/create", 100},
		{"blocking/create", 20},
		{"drive/files/create", 120},
		{"channels/create", 10},
		{"ap/show", 30},
		{"federation/update-remote-user", 30},
		// Auth / password reset (#600 item 3): signup spam / brute-force 対策。
		{"signup", 5},
		{"signup-pending", 30},
		// 承認制の登録も同格。**ここが抜けると未認証の入口に上限が 1 つも
		// 無くなる** — 承認制は /api/signup を塞ぐので signup 側の上限が効かない。
		{"signup-application/apply", 5},
		{"signup-application/register", 5},
		{"signup-application/status", 30},
		// apply を守るトークンの発行 (#2806)。読み込み直しは正常な操作なので
		// apply より緩いが、無制限にはしない。
		{"signup-application/form-token", 30},
		// 初回セットアップの窓は credential 無しで通るので setupPassword の
		// 試行に上限が要る。
		{"admin/accounts/create", 30},
		{"signin", 10},
		{"signin-flow", 10},
		{"signin-with-passkey", 200},
		{"request-reset-password", 3},
		{"reset-password", 30},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			limit, ok := DefaultEndpointLimits[tc.endpoint]
			require.True(t, ok, "endpoint %s not found in limits", tc.endpoint)
			assert.Equal(t, tc.max, limit.Max)
		})
	}
}

// TestDefaultEndpointLimits_UnauthenticatedEntryPoints guards the endpoints a
// stranger can reach without any credential.
//
// **テーブルに載っていないエンドポイントは無制限になる** (Middleware は
// `limits[endpoint]` が引けなければ即 next へ抜ける)。無制限であること自体は
// 大半のエンドポイントで正しいので、`DefaultEndpointLimits` を眺めても抜けに
// 気づけない。未認証で叩けてサーバー側に行やメールを作る入口だけは、ここで
// 名指しで固定する。
//
// **router.go の実パスから引ける形で確かめる。** テーブルのキー名を直接見ると、
// `/api` prefix の剥がし方が変わったときに「キーはあるのに引けない」状態を
// 見逃す。
func TestDefaultEndpointLimits_UnauthenticatedEntryPoints(t *testing.T) {
	paths := []string{
		"/api/signup",
		"/api/signup-pending",
		"/api/signup-application/apply",
		"/api/signup-application/register",
		"/api/signup-application/status",
		// 発行 endpoint も未認証で叩ける (#2806)。apply を守るトークンを配る
		// 側なので、ここが無制限だと gate の前段が無制限になる。
		"/api/signup-application/form-token",
		"/api/signin",
		"/api/signin-flow",
		"/api/request-reset-password",
		"/api/reset-password",
		// 初回セットアップの窓だけ credential 無しで通る。
		"/api/admin/accounts/create",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			store := &mockLimitStore{}
			rl := NewRateLimiter(store, true, DefaultEndpointLimits)
			e, h := setupEcho(rl)

			doRequest(e, rl.Middleware(), h, path, nil)

			require.NotEmpty(t, store.calls,
				"%s が無制限。DefaultEndpointLimits にキーが無いか、"+
					"path からキーへの変換が合っていない", path)
		})
	}
}

func TestDefaultEndpointLimits_RoleAssignmentShow(t *testing.T) {
	tests := []struct {
		endpoint string
		max      int
	}{
		{endpoint: "roles/assignment-show", max: 60},
		{endpoint: "admin/roles/assignment-show", max: 120},
	}
	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			limit, ok := DefaultEndpointLimits[tc.endpoint]
			require.True(t, ok)
			assert.Equal(t, time.Minute, limit.Duration)
			assert.Equal(t, tc.max, limit.Max)
		})
	}
}

// adminUserForLimit は UserBucketOnly の endpoint を認証済みで叩くための利用者。
var adminUserForLimit = &model.User{ID: "01hzzzzzzzzzzzzzzzzzzzzzzz"}

// IP を返す 4 endpoint に上限があること (#3106)。
//
// **値と path 解決の両方を見る。** 値だけ見ると route を rename したときに
// 「キーはあるのに引けない」状態を見逃すし、path 解決だけ見ると上限を
// 実質無制限 (`Max: 100000`) に緩めても通る。
//
// ここが抜けると**完了条件の「検索頻度制限」を固定するものが何も無くなる** —
// 実際、初版は 3 行を削除しても全テストと `make gates` が緑だった (敵対的
// レビュー 1 周目で実測)。limiter は未登録のキーを**素通しする**ので、
// 気付ける経路が他に無い。
func TestDefaultEndpointLimits_IPLookups(t *testing.T) {
	paths := []string{
		"/api/admin/ip/accounts",
		"/api/admin/ip/related-accounts",
		"/api/admin/ip/lookup-log",
		// upstream の口。**同じ「利用者 ↔ IP の対応」を返す**ので、ここだけ
		// 無制限だと mk-go 側に上限を置いた意味が無い (#3106)。
		"/api/admin/get-user-ips",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			key := strings.TrimPrefix(path, "/api/")
			limit, ok := DefaultEndpointLimits[key]
			require.True(t, ok, "%s に上限が無い", path)
			assert.Equal(t, time.Hour, limit.Duration)
			assert.Equal(t, 120, limit.Max)

			assert.True(t, limit.UserBucketOnly,
				"%s は user bucket だけで数えること。IP bucket も見ると、\n"+
					"同じ出口 IP から未認証で叩き続けるだけで正当なモデレーターを"+
					"締め出せる (#3106)", path)

			// path からキーが引けることまで見る。**認証済みで叩く** —
			// この 4 本は UserBucketOnly なので未認証では bucket を消費しない。
			store := &mockLimitStore{}
			rl := NewRateLimiter(store, true, DefaultEndpointLimits)
			e, h := setupEcho(rl)
			doRequest(e, rl.Middleware(), h, path, adminUserForLimit)
			require.NotEmpty(t, store.calls,
				"%s が無制限。path からキーへの変換が合っていない", path)
			for _, call := range store.calls {
				assert.False(t, strings.HasPrefix(call.Key, "ip-"),
					"%s が IP bucket を消費している (key=%s)", path, call.Key)
			}

			// **未認証では消費しない。** ここが消費すると締め出しが作れる。
			store2 := &mockLimitStore{}
			rl2 := NewRateLimiter(store2, true, DefaultEndpointLimits)
			e2, h2 := setupEcho(rl2)
			doRequest(e2, rl2.Middleware(), h2, path, nil)
			assert.Empty(t, store2.calls,
				"%s が未認証のリクエストで bucket を消費している。\n"+
					"limiter は権限検査より前に走るので、403 になる相手が\n"+
					"正当なモデレーターの窓を食い潰せる (#3106)", path)
		})
	}
}

func TestDefaultEndpointLimits_MinIntervalEndpoints(t *testing.T) {
	cases := []struct {
		endpoint    string
		minInterval time.Duration
	}{
		{"notes/delete", time.Second},
		{"notes/unrenote", time.Second},
		{"notes/reactions/delete", 3 * time.Second},
		{"bubble-game/register", 30 * time.Second},
		{"signin", time.Second},
		{"signin-flow", time.Second},
		{"signin-with-passkey", 250 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.endpoint, func(t *testing.T) {
			limit, ok := DefaultEndpointLimits[tc.endpoint]
			require.True(t, ok)
			assert.Equal(t, tc.minInterval, limit.MinInterval)
		})
	}
}

func TestMiddleware_RetryAfterNegativeClampsToZero(t *testing.T) {
	// リセット時刻が過去の場合、Retry-Afterは0にクランプ
	resetMs := time.Now().Add(-10 * time.Second).UnixMilli()
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 0, ResetMs: resetMs}},
		},
	}
	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 300},
	}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u1"})

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.Equal(t, "0", rec.Header().Get("Retry-After"))
}

// TestMiddleware_SigninReturnsTooManyAuthFailures verifies the signin endpoint's
// 429 carries the upstream TOO_MANY_AUTHENTICATION_FAILURES code/id rather than
// the generic RATE_LIMIT_EXCEEDED (#1829)。signin は minInterval(1s) と
// duration/max(10/1h) の 2 gate を持つので、両方の reject 経路で custom error が
// 返ることを確認する (1 件目で minInterval gate、2 件目で minInterval pass →
// duration gate)。
func TestMiddleware_SigninReturnsTooManyAuthFailures(t *testing.T) {
	cases := []struct {
		name    string
		results []mockResult
	}{
		{
			name:    "minInterval gate",
			results: []mockResult{{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Hour).UnixMilli()}}},
		},
		{
			name: "duration gate",
			results: []mockResult{
				{Info: LimitInfo{Remaining: 1, ResetMs: time.Now().Add(time.Second).UnixMilli()}}, // minInterval pass
				{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Hour).UnixMilli()}},   // duration reject
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &mockLimitStore{results: tc.results}
			// 実際の wired def を使い、RejectResponse 配線まで含めて検証する。
			limits := map[string]*EndpointLimit{"signin": DefaultEndpointLimits["signin"]}
			rl := NewRateLimiter(store, true, limits)
			e, h := setupEcho(rl)

			rec := doRequest(e, rl.Middleware(), h, "/api/signin", &model.User{ID: "u1"})

			assert.Equal(t, http.StatusTooManyRequests, rec.Code)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			errObj, _ := body["error"].(map[string]any)
			require.NotNil(t, errObj)
			assert.Equal(t, "TOO_MANY_AUTHENTICATION_FAILURES", errObj["code"])
			assert.Equal(t, "22d05606-fbcf-421a-a2db-b32610dcfd1b", errObj["id"])
		})
	}
}

// TestMiddleware_GenericEndpointReturnsRateLimitExceeded confirms endpoints
// without a RejectResponse override still return the generic error.
func TestMiddleware_GenericEndpointReturnsRateLimitExceeded(t *testing.T) {
	store := &mockLimitStore{
		results: []mockResult{
			{Info: LimitInfo{Remaining: 0, ResetMs: time.Now().Add(time.Hour).UnixMilli()}},
		},
	}
	limits := map[string]*EndpointLimit{"notes/create": {Duration: time.Hour, Max: 300}}
	rl := NewRateLimiter(store, true, limits)
	e, h := setupEcho(rl)

	rec := doRequest(e, rl.Middleware(), h, "/api/notes/create", &model.User{ID: "u1"})

	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj, _ := body["error"].(map[string]any)
	require.NotNil(t, errObj)
	assert.Equal(t, "RATE_LIMIT_EXCEEDED", errObj["code"])
}

// --- Redis integration test ---

func TestRedisRateLimitStore_Integration(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer tr.Teardown(ctx)
	tr.FlushAll(ctx)

	store := NewRedisRateLimitStore(tr.Client)

	// 5リクエスト/10秒のリミット
	const maxReqs = 5
	dur := 10 * time.Second

	// ZCARDはZADD前に実行されるので、remaining=max-count(before add)。
	// maxReqs回は通る（remaining: max, max-1, ..., 1）
	for i := 0; i < maxReqs; i++ {
		info, err := store.Check(ctx, "test-user:test-ep", dur, maxReqs)
		require.NoError(t, err)
		assert.Equal(t, maxReqs-i, info.Remaining, "iteration %d", i)
	}

	// maxReqs+1回目はremaining=0（ブロック対象）
	info, err := store.Check(ctx, "test-user:test-ep", dur, maxReqs)
	require.NoError(t, err)
	assert.Equal(t, 0, info.Remaining)
	assert.Greater(t, info.ResetMs, time.Now().UnixMilli())
}

func TestRedisRateLimitStore_SeparateKeys(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer tr.Teardown(ctx)
	tr.FlushAll(ctx)

	store := NewRedisRateLimitStore(tr.Client)
	dur := 10 * time.Second

	// 異なるキーは独立してカウント
	// ZCARDはZADD前なので1回目のremaining=max(=1)
	info1, err := store.Check(ctx, "user1:ep", dur, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, info1.Remaining) // max=1で1回目、ZCARD=0 → remaining=1

	info2, err := store.Check(ctx, "user2:ep", dur, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, info2.Remaining) // 別キーなのでこちらも1回目

	// 2回目はremaining=0
	info3, err := store.Check(ctx, "user1:ep", dur, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, info3.Remaining)
}

// TestRateLimiter_NilLimitsBypassesAllEndpoints verifies that passing a nil
// limits map (used when disableEndpointRateLimits=true) lets every endpoint
// through, even ones that would normally be capped (e.g. notes/create).
func TestRateLimiter_NilLimitsBypassesAllEndpoints(t *testing.T) {
	store := &mockLimitStore{}
	rl := NewRateLimiter(store, true, nil)

	e := echo.New()
	handler := func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	}
	mw := rl.Middleware()

	for i := range 100 {
		req := httptest.NewRequest(http.MethodPost, "/api/notes/create", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath("/api/notes/create")
		c.Set(string(UserContextKey), &model.User{ID: "testuser"})
		_ = mw(handler)(c)
		require.Equal(t, http.StatusOK, rec.Code, "iter %d should always pass when limits is nil", i)
	}

	assert.Empty(t, store.calls, "store.Check must not be invoked when limits is nil")
}

// TestRateLimiter_EmptyLimitsBypassesAllEndpoints is structurally similar to
// the nil-map case but exercises the explicit empty-map path so refactors
// that swap nil for map[...]{} don't regress.
func TestRateLimiter_EmptyLimitsBypassesAllEndpoints(t *testing.T) {
	store := &mockLimitStore{}
	rl := NewRateLimiter(store, true, map[string]*EndpointLimit{})

	e := echo.New()
	handler := func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	}
	mw := rl.Middleware()

	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/notes/create")
	c.Set(string(UserContextKey), &model.User{ID: "u"})
	_ = mw(handler)(c)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, store.calls)
}

func TestNewRedisRateLimiter_Integration(t *testing.T) {
	ctx := context.Background()
	tr, err := testutil.SetupRedis(ctx)
	if err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	defer tr.Teardown(ctx)
	tr.FlushAll(ctx)

	limits := map[string]*EndpointLimit{
		"notes/create": {Duration: time.Hour, Max: 2},
	}
	rl := NewRedisRateLimiter(tr.Client, true, limits)

	e := echo.New()
	handler := func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	}
	mw := rl.Middleware()

	// 2回は通る
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/notes/create", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetPath("/api/notes/create")
		c.Set(string(UserContextKey), &model.User{ID: "testuser"})
		_ = mw(handler)(c)
		assert.Equal(t, http.StatusOK, rec.Code, "request %d", i)
	}

	// 3回目は429
	req := httptest.NewRequest(http.MethodPost, "/api/notes/create", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/notes/create")
	c.Set(string(UserContextKey), &model.User{ID: "testuser"})
	_ = mw(handler)(c)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

// chat のメッセージ送信は 3 つの path から同じ handler に入る
// (`create` は `create-to-user` / `create-to-room` の alias)。**limiter のキーは
// route path なので、1 つでも登録から漏れると上限そのものを迂回できる** —
// 未登録の path はフォールバック無しで素通しになる。
func TestDefaultEndpointLimits_ChatMessageSendPathsAgree(t *testing.T) {
	paths := []string{
		"chat/messages/create",
		"chat/messages/create-to-user",
		"chat/messages/create-to-room",
	}
	want := DefaultEndpointLimits[paths[0]]
	require.NotNil(t, want, "%s が未登録", paths[0])
	for _, p := range paths[1:] {
		got := DefaultEndpointLimits[p]
		require.NotNil(t, got, "%s が未登録", p)
		assert.Equal(t, want.Duration, got.Duration, "%s の Duration が %s と違う", p, paths[0])
		assert.Equal(t, want.Max, got.Max, "%s の Max が %s と違う", p, paths[0])
	}
}

// TestDefaultEndpointLimits_RelationListEndpoints guards the public relation
// lists against bulk harvesting (#2953).
//
// **UnauthenticatedEntryPoints に相乗りしない。** あちらの doc は「サーバー側に
// 行やメールを作る入口」を守ると宣言しているが、こちらは**作らない、漏らす**
// 側なので、混ぜると既存テストの説明が嘘になる。
//
// **テーブルに載っていないエンドポイントは無制限**になる。この 2 つは未認証で
// 全件を引けるので、載っていないと収集の速度に上限が無い。
func TestDefaultEndpointLimits_RelationListEndpoints(t *testing.T) {
	paths := []string{
		"/api/users/following",
		"/api/users/followers",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			store := &mockLimitStore{}
			rl := NewRateLimiter(store, true, DefaultEndpointLimits)
			e, h := setupEcho(rl)

			doRequest(e, rl.Middleware(), h, path, nil)

			require.NotEmptyf(t, store.calls,
				"%s が無制限。DefaultEndpointLimits にキーが無いか、"+
					"path からキーへの変換が合っていない", path)
		})
	}
}

// **窓は短く保つ。** store は拒否したリクエストも記録するので、429 を無視して
// 叩き続けるクライアントは Retry-After を 1 窓ぶんに押し戻し続ける。長い窓を
// 置くと、行儀の悪いタブ 1 つで CGNAT 配下が丸ごとその時間だけ閲覧不能になる。
func TestRelationListLimitsUseAShortWindow(t *testing.T) {
	for _, key := range []string{"users/following", "users/followers"} {
		limit, ok := DefaultEndpointLimits[key]
		require.Truef(t, ok, "%s の定義が無い", key)
		require.LessOrEqualf(t, limit.Duration, time.Minute,
			"%s の窓が長すぎる (%s)。拒否も記録されるので、長い窓は"+
				"共有 IP の巻き添えを長引かせる", key, limit.Duration)
		// 人間のスクロール (1 リクエスト 30 行) を明確に上回ること。
		require.GreaterOrEqualf(t, limit.Max, 30,
			"%s の上限が低すぎる。通常の閲覧が 429 になる", key)
	}
}

// TestDefaultEndpointLimits_ApplicationEntryPoints guards the authenticated
// endpoints that create rows in a moderation queue.
//
// **テーブルに載っていないエンドポイントは無制限になる** (上の
// UnauthenticatedEntryPoints と同じ理由)。認証が要るので誰でも叩けるわけでは
// ないが、**1 人が審査キューを無制限に積める**のは未認証の入口と同じ害になる。
// `emoji_application` の pending 一意制約は `(userId, name)` なので、名前を
// 変えれば何件でも通る。
//
// #2935 で**任意のノートの絵文字メニューから 2 クリック**になり、露出が大きく
// 変わった (従来は drive へ上げてから専用ページを開く必要があった)。
func TestDefaultEndpointLimits_ApplicationEntryPoints(t *testing.T) {
	// **値まで見る (#2958 レビュー L2)。** 「何らかの上限があるか」だけだと、
	// 1 分 5000 回のような実質無制限へ緩めても緑のまま通る。#2958 が足した
	// ロール別の期間上限は総量を絞るもので、こちらの連打の抑止とは層が違う
	// ため、片方が消えたときにもう片方が代わりを務めることはない。
	cases := []struct {
		path     string
		duration time.Duration
		max      int
	}{
		{"/api/emoji-application/create", time.Hour, 5},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			store := &mockLimitStore{}
			rl := NewRateLimiter(store, true, DefaultEndpointLimits)
			e, h := setupEcho(rl)

			doRequest(e, rl.Middleware(), h, tc.path, nil)

			require.NotEmpty(t, store.calls,
				"%s が無制限。DefaultEndpointLimits にキーが無いか、"+
					"path からキーへの変換が合っていない", tc.path)

			key := strings.TrimPrefix(tc.path, "/api/")
			limit, ok := DefaultEndpointLimits[key]
			require.Truef(t, ok, "DefaultEndpointLimits に %q が無い", key)
			require.Equalf(t, tc.duration, limit.Duration,
				"%s の窓が変わっている。緩めるなら期間上限 (#2958) との役割分担を先に確認すること", tc.path)
			require.Equalf(t, tc.max, limit.Max,
				"%s の上限が変わっている。緩めるなら期間上限 (#2958) との役割分担を先に確認すること", tc.path)
		})
	}
}

// **存在確認のオラクルに上限を置く (#3037)。**
//
// `username/available` / `email-address/available` はどちらも未認証で叩けて、
// 返るのは「使われているか」という真偽値そのもの。止めたいのは 1 件ずつの
// 確認ではなく総当たり — 辞書を回して実在する利用者名の一覧を作る、手持ちの
// メールアドレス一覧からこのサーバーの登録者を割り出す、といった使い方。
//
// upstream には上限が無い (`meta` に `limit` が無い) mk-go 独自の追加。
func TestDefaultEndpointLimits_AvailabilityOracles(t *testing.T) {
	for _, endpoint := range []string{"username/available", "email-address/available"} {
		t.Run(endpoint, func(t *testing.T) {
			limit, ok := DefaultEndpointLimits[endpoint]
			require.True(t, ok, "%s に上限が無い", endpoint)
			// **窓は短く。** 拒否も記録されるので、長い窓だと行儀の悪い
			// クライアント 1 つで共有 IP の配下が登録フォームを使えなくなる。
			assert.LessOrEqual(t, limit.Duration, time.Minute, "窓が長すぎる")
			// **実使用の 2 倍を要求する。** 同梱フロントは API 呼び出しだけを
			// debounce 1000ms しており、trailing なので打ち続けている間は
			// 0 回。最大化しても 1 タブ 1 分に 59-60 回が上限。
			//
			// 未認証は IP bucket しか無く NAT 配下で共有されるので、実使用と
			// 同値だと 2 人目で当たり、送信ボタンが押せなくなる (#3037
			// レビュー 2 周目で email 側が 60 = 余裕ゼロだった)。**下限は
			// 実使用そのものではなく倍で留めること** — 60 まで緩めると
			// 回帰した値がそのまま通る。
			const perTabCeiling = 60
			assert.GreaterOrEqual(t, limit.Max, perTabCeiling*2, "実使用に対する余裕が足りない")
			assert.LessOrEqual(t, limit.Max, 200, "総当たりの速度として緩すぎる")
		})
	}
}
