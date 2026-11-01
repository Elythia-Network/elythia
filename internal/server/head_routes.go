package server

import (
	"net/http"
	"sync"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/misc/headreq"
)

// headRoutes makes every GET route also answer HEAD, the way upstream's
// Fastify does with its default exposeHeadRoutes (#3925).
//
// Echo v4 は GET で登録したルートを HEAD に広げないので、何もしないと GET の
// ルートは HEAD に 405 を返す。ルートごとに HEAD を足すと足し忘れが出るので、
// ルーターの手前でまとめて扱う。3 つの部品で成り立つ:
//
//   - pre (e.Pre): HEAD のリクエストを、GET のルートへ振り分ける。Echo は
//     ルーティングに net/http から受け取った *http.Request そのものの Method を
//     使う (Pre の後で c.SetRequest しても見ない) ので、その Method を一時的に
//     GET に書き換える。HEAD を明示したルート (e.Any など) に当たるときは
//     書き換えない (細かい規則は routesAsGET)
//   - restore (最も外側の e.Use): ルーティングが済んだ直後に、元の
//     *http.Request の Method を HEAD に戻す。net/http はこの値を見て本文の
//     書き込みを捨てる (GET と同じ Content-Length も付ける) ので、本文を返さない
//     のは net/http に任せる。アクセスログもここから外側では HEAD を記録する
//   - presentAsGET (アクセスログの直後の e.Use): それより内側の middleware と
//     handler には、Method を GET にした複製を渡す。handler には
//     `Method == GET` で分岐するものがあり (認証、本文の読み方など)、HEAD のまま
//     渡すと GET と違う status を返しうる。RFC 9110 の「HEAD は本文が無いこと
//     以外 GET と同じ」に合わせて、handler からは GET として扱わせる。本文を
//     作らずに済ませたい handler は headreq.Is で HEAD を見分ける
//
// 明示した HEAD のルートを見分けるため、登録されたルートを OnAddRouteHandler
// で記録する。Echo の Routes() には handler が無く、group の 404 用の
// catch-all (RouteNotFound) も同じ位置に入るので、ルーターの引き直しだけでは
// 「本物の GET のルートに当たったか」が分からない。
type headRoutes struct {
	mu   sync.RWMutex
	get  map[string]struct{}
	head map[string]struct{}
}

// installHeadRoutes wires headRoutes into e. It must be called right after
// echo.New, before any route or other middleware is added: the route hook only
// sees routes added after it, and restore has to be the outermost middleware.
// The returned value's presentAsGET still has to be added by the caller.
func installHeadRoutes(e *echo.Echo) *headRoutes {
	h := &headRoutes{get: map[string]struct{}{}, head: map[string]struct{}{}}
	prev := e.OnAddRouteHandler
	e.OnAddRouteHandler = func(host string, route echo.Route, handler echo.HandlerFunc, mw []echo.MiddlewareFunc) {
		h.record(route)
		if prev != nil {
			prev(host, route, handler, mw)
		}
	}
	e.Pre(h.pre)
	e.Use(h.restore)
	return h
}

// record remembers a GET or HEAD route.
//
// Host ごとのルーター (e.Host) は使っていない。使うようになったら、pre は既定の
// ルーターだけを引くので、host で分けて記録する。
func (h *headRoutes) record(route echo.Route) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch route.Method {
	case http.MethodGet:
		h.get[route.Path] = struct{}{}
	case http.MethodHead:
		h.head[route.Path] = struct{}{}
	}
}

// routesAsGET reports whether a HEAD request for r should be served by the GET
// route it matches: it matches a GET route, and no HEAD route of its own at
// least as specific.
//
// Fastify はパスでルートを決めてからメソッドを見るので、GET のルートが
// `api.Any("/*")` のような HEAD も持つ catch-all より具体的なら、HEAD もその
// GET のルートに当たる。Echo は HEAD を持つノードだけを探すので、そのままだと
// catch-all に落ちる (本番で HEAD /api/* が 200 だったのはこれで、GET と違う
// 応答だった)。
//
// HEAD で当たったルートが GET も持つなら、GET で当たったルートはそれと同じか、
// より具体的 (Echo は GET を持つノードのうち最も具体的なものを返すので、HEAD
// 側が具体的なら GET でもそこに当たる)。同じなら HEAD を明示したルートなので
// そのまま通し、違えば GET のルートへ振り分ける。HEAD だけを持つルート
// (e.HEAD) に当たったときは、どちらが具体的か分からないので、明示した HEAD を
// 優先する。今はそういうルートは無い。
func (h *headRoutes) routesAsGET(e *echo.Echo, r *http.Request) bool {
	path := echo.GetPath(r)
	c := e.NewContext(r, nil)
	h.mu.RLock()
	defer h.mu.RUnlock()
	// Find はパスに一致したノードの位置を入れる。group の 404 用の catch-all
	// (RouteNotFound) に当たったときや、ノードにそのメソッドが無いとき (405) も
	// 位置は入るが、そのメソッドで登録したルートの集合には無い。集合にあれば、
	// そのルートに当たっている。
	e.Router().Find(http.MethodGet, path, c)
	getPath := c.Path()
	if _, ok := h.get[getPath]; !ok {
		return false
	}
	c.Reset(r, nil)
	e.Router().Find(http.MethodHead, path, c)
	headPath := c.Path()
	if _, ok := h.head[headPath]; !ok {
		return true
	}
	_, headHasGET := h.get[headPath]
	return headHasGET && headPath != getPath
}

func (h *headRoutes) pre(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		r := c.Request()
		// Upgrade を求める HEAD は振り分けない。handler には GET に見えるので、
		// /streaming が WebSocket へ切り替えてしまう。本家 (ws の handleUpgrade)
		// も RFC 6455 も、切り替えは GET に限る (#3925)。Header.Get は最初の値しか
		// 見ないので、空の値を先頭に置いた Upgrade を見逃す (gorilla/websocket は
		// 全部の値を見て切り替える)。値があるかどうかで判定する。
		if r.Method != http.MethodHead || len(r.Header.Values(echo.HeaderUpgrade)) > 0 || !h.routesAsGET(c.Echo(), r) {
			return next(c)
		}
		// HEAD に戻すのは restore。ルーティングの後に必ず通る (Echo は見つけた
		// handler に e.Use の middleware を必ず重ねる) ので、handler の error を
		// HTTPErrorHandler が書くときも HEAD に戻っている。
		r.Method = http.MethodGet
		headreq.Mark(c)
		return next(c)
	}
}

func (h *headRoutes) restore(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if headreq.Rerouted(c) {
			c.Request().Method = http.MethodHead
		}
		return next(c)
	}
}

func (h *headRoutes) presentAsGET(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if headreq.Rerouted(c) {
			r := c.Request()
			// 元の *http.Request は net/http と共有しているので書き換えず、
			// 浅い複製の Method だけを GET にする。
			asGET := r.WithContext(r.Context())
			asGET.Method = http.MethodGet
			c.SetRequest(asGET)
		}
		return next(c)
	}
}
