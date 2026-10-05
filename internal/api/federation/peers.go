package federation

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/labstack/echo/v4"
)

// Peers handles GET /api/v1/instance/peers.
//
// Mastodon-compatible peer listing: a flat JSON array of the hosts this
// instance knows about, excluding suspended ones. Used by fediverse crawlers
// and statistics sites (fediverse.observer など) to map federation topology.
//
// upstream Misskey は endpoints/ 配下ではなく ApiServerService.ts で fastify に
// 直接登録している (#2245)。実装も `select: {host: true}` /
// `where: {suspensionState: 'none'}` の全件走査のみで、blocked / silenced や
// isNotResponding では絞らず limit も持たない。ここで独自のフィルタを足すと
// 統計サイトから見たピア一覧が TS 実装と食い違うので、同じ条件のままにする。
//
// federation/instances (Misskey 独自 API) とは別物で、あちらは instance
// オブジェクトの詳細を返す。こちらは host 文字列のみ。
func (h *Handler) Peers(c echo.Context) error {
	if h.svc == nil {
		return c.JSON(http.StatusOK, []string{})
	}
	hosts, err := h.svc.PeerHosts()
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// nil を返すと JSON が null になる。upstream は必ず配列を返すので
	// 空でも [] になるよう正規化する。
	if hosts == nil {
		hosts = []string{}
	}
	return c.JSON(http.StatusOK, hosts)
}
