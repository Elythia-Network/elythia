package admin

import (
	"net/http"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/procstats"
	"github.com/labstack/echo/v4"
)

// SetProcStatsDeps wires the providers used by admin/server-metrics (#2395).
// 未配線でも endpoint は動き、取れない section が省かれるだけ。
func (h *Handler) SetProcStatsDeps(deps procstats.Deps) {
	h.procStats = deps
}

// ServerMetrics handles POST /api/admin/server-metrics.
//
// mk-go プロセス自身の状態 (Go ランタイム / DB 接続プール / Redis 接続プール) を
// 返す mk-go 独自エンドポイント。upstream には無い。
//
// admin/server-info の additive 拡張にしなかったのは、あちらが upstream にも同名で
// 存在し misskey-js の型に載っているため。本家互換のレスポンスと mk-go 固有の運用
// 情報を同じ器に入れると、後からどちらの都合で触ってよいか分からなくなる。別
// endpoint なら shape drift の対象にもならない。
//
// Prometheus /metrics を admin から読ませる案は採っていない。無認証公開のまま
// ブラウザへ晒すことになるうえ、production では reverse proxy の ACL で塞がって
// いて到達できない。
func (h *Handler) ServerMetrics(c echo.Context) error {
	stats := procstats.Collect(h.procStats, config.MisskeyVersion, config.MkGoVersion)
	return c.JSON(http.StatusOK, stats)
}
