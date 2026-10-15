package middleware

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"
)

// APICacheSec maps an API endpoint name (the route path without "/api/") to
// the `cacheSec` its upstream definition declares. Every entry is also
// `allowGet: true` upstream. TestCacheSecDrift (internal/entitycompat) checks
// this table against upstream's endpoint meta.
//
// 本家の ApiCallService は、資格情報の無い GET の成功応答に
// `Cache-Control: public, max-age=<cacheSec>` を付ける。handler ごとに書くと
// 付け忘れが起きる (#3593 の時点で 8 本が抜けていた) ので、表を 1 か所に置く。
var APICacheSec = map[string]int{
	"bubble-game/ranking":    60,
	"charts/active-users":    3600,
	"charts/ap-request":      3600,
	"charts/drive":           3600,
	"charts/federation":      3600,
	"charts/instance":        3600,
	"charts/notes":           3600,
	"charts/user/drive":      3600,
	"charts/user/following":  3600,
	"charts/user/notes":      3600,
	"charts/user/pv":         3600,
	"charts/user/reactions":  3600,
	"charts/users":           3600,
	"emoji":                  3600,
	"emojis":                 3600,
	"federation/instances":   3600,
	"federation/stats":       3600,
	"fetch-rss":              180,
	"get-online-users-count": 60,
	"hashtags/trend":         60,
	"notes/featured":         3600,
	"retention":              3600,
	"server-info":            60,
	"users/featured-notes":   3600,
}

// APICacheControl sets `Cache-Control: public, max-age=<cacheSec>` on the
// successful (2xx) response to a GET without credentials for the endpoints in
// APICacheSec, mirroring upstream ApiCallService.handleRequest
// (`GET && cacheSec && !token && !user`). Mount it on the /api group after
// the default `private, max-age=0, must-revalidate` so it wins.
//
// 判定は応答を書く直前 (Response.Before) に行う。本家はエラー応答
// (#sendApiError) には付けないので、status で成功だけに絞る。token は解決
// できたかに関係なく、送られてきたかで見る (無効な token でも付けない、#2049)。
func APICacheControl() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			if c.Request().Method != http.MethodGet {
				return next(c)
			}
			sec, ok := APICacheSec[strings.TrimPrefix(c.Path(), "/api/")]
			if !ok {
				return next(c)
			}
			res := c.Response()
			res.Before(func() {
				if res.Status < 200 || res.Status >= 300 {
					return
				}
				if GetUser(c) != nil || HasRawToken(c) || publicCacheSkipped(c) {
					return
				}
				res.Header().Set(echo.HeaderCacheControl, "public, max-age="+strconv.Itoa(sec))
			})
			return next(c)
		}
	}
}

// skipPublicCacheKey marks a request whose response must not get the public
// Cache-Control. See SkipPublicCache.
const skipPublicCacheKey = "misskeySkipPublicCache"

// SkipPublicCache keeps APICacheControl from making this response public.
// Handlers call it when they answer a failure with a 200 fallback (an empty
// list, a zero count): upstream answers those with an error, which is never
// cached, so a shared cache must not keep the fallback for cacheSec.
func SkipPublicCache(c echo.Context) {
	c.Set(skipPublicCacheKey, true)
}

func publicCacheSkipped(c echo.Context) bool {
	v, _ := c.Get(skipPublicCacheKey).(bool)
	return v
}
