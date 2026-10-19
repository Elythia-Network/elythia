package server

import (
	"github.com/elythia-network/elythia/internal/api/endpoints"
	"github.com/elythia-network/elythia/internal/api/meta"
	"github.com/elythia-network/elythia/internal/api/stats"
	apitest "github.com/elythia-network/elythia/internal/api/test"
)

// registerMetaRoutes registers the server metadata and endpoint catalogue
// routes (meta, ping, stats, server-info, endpoints, endpoint, test) on the
// /api group.
func registerMetaRoutes(api apiRoutes, metaHandler *meta.Handler, statsHandler *stats.Handler, endpointsHandler *endpoints.Handler, echoTestHandler *apitest.Handler) {
	api.POST("/meta", metaHandler.Meta)
	api.POST("/ping", metaHandler.Ping)

	api.POST("/stats", statsHandler.Stats)

	// server-info (公開版) — サーバー情報
	// frontend の server-metric widget は `misskeyApiGet` で GET 呼び出し、
	// それ以外の MkVisitorDashboard 等は POST で呼ぶため両メソッドを登録。
	api.POST("/server-info", metaHandler.ServerInfo)
	api.GET("/server-info", metaHandler.ServerInfo)

	// endpoints — 登録済みAPIエンドポイント一覧
	api.POST("/endpoints", endpointsHandler.Endpoints)

	// endpoint — 指定 endpoint の情報を返す (#1695)。upstream endpoint.ts は
	// 未知 endpoint で null、既知なら { params: [{name,type}, ...] } を返す。
	// mk-go は per-endpoint の paramDef メタデータを保持しない (handler は
	// param schema を構造化していない) ため、
	// param 名/型までは導出できない。ここでは response の shape (params は配列)
	// と未知 endpoint の null だけ upstream に揃える。params 内容の導出は
	// endpoint registry の新設が必要で本 endpoint の用途 (API introspection) に
	// 対して費用対効果が低いため非対応 (#1695 に明記)。
	api.POST("/endpoint", endpointsHandler.Endpoint)

	// test — paramDef の検証挙動そのものを確認するための endpoint
	// (upstream test.ts、#1551)。validate 済の ps をそのまま echo する。
	//
	//   - required:['required'] が未指定なら 400
	//   - `id` は format:'misskey:id' なので空文字は 400
	//   - default 付きの param はキー省略時のみ既定値。`nullableDefault: null`
	//     は明示 null として通す (default で潰さない)
	api.POST("/test", echoTestHandler.Test)
}
