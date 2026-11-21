package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"
)

// cspReportMaxBody bounds the accepted report size.
//
// CSP report は数百バイト。これを超えるのは異常なので早期に切る。認証不要の
// POST なので、上限が無いと**任意サイズの body をログ増幅に使える**。
const cspReportMaxBody = 64 * 1024

// cspReport is the browser-sent violation payload (CSP Level 2 shape).
//
// 参照するのは調査に要る 4 つだけ。仕様外の field が増えても壊れないよう
// 未知キーは無視する。
type cspReport struct {
	Body struct {
		DocumentURI        string `json:"document-uri"`
		ViolatedDirective  string `json:"violated-directive"`
		EffectiveDirective string `json:"effective-directive"`
		BlockedURI         string `json:"blocked-uri"`
	} `json:"csp-report"`
}

// cspReportHandler logs violation reports posted by browsers.
//
// **常に 204 を返す。** 内容が妥当だったかを攻撃者に返さない。ブラウザ側も
// 応答本文を見ないので、失敗を伝える意味がそもそも無い。
//
// # 濫用について
//
// 認証不要の POST なので、放っておくとログ増幅に使える。対策は 2 段:
//
//   - body を 64KiB で切る (下の cspReportMaxBody)
//   - **CSP が off のときは route ごと生やさない** (router 側)。既定は off なので、
//     opt-in した operator 以外には endpoint 自体が存在しない
//
// API の rate limiter は流用していない。あちらは `/api/` を剥がした path を
// キーにする API 専用の仕組みで、しかも `enableIPRateLimit` が false だと
// 未認証リクエストに効かない。「有効化した人だけが晒す」方が確実に効く。
// 公開インスタンスで有効にする場合は、前段の nginx / CDN でこの path に
// レート制限を掛けることを推奨する (docs/configuration.md に記載)。
func cspReportHandler() echo.HandlerFunc {
	return func(c *echo.Context) error {
		body, err := io.ReadAll(io.LimitReader(c.Request().Body, cspReportMaxBody+1))
		if err != nil {
			return c.NoContent(http.StatusNoContent)
		}
		if len(body) > cspReportMaxBody {
			slog.Warn("csp-report: body too large; dropped",
				"limit", cspReportMaxBody, "remoteAddr", c.RealIP())
			return c.NoContent(http.StatusNoContent)
		}
		var r cspReport
		if err := json.Unmarshal(body, &r); err != nil {
			// 壊れた JSON は無視する。ブラウザ以外が投げてきたか、仕様の違う
			// 形式。件数だけ分かれば十分なので本文はログに出さない (任意の
			// 文字列をログへ書き込ませないため)。
			slog.Debug("csp-report: unparseable payload", "bytes", len(body))
			return c.NoContent(http.StatusNoContent)
		}
		directive := r.Body.ViolatedDirective
		if directive == "" {
			directive = r.Body.EffectiveDirective
		}
		if directive == "" && r.Body.BlockedURI == "" {
			// csp-report キーを持たない JSON。無視する。
			return c.NoContent(http.StatusNoContent)
		}
		// INFO で出す。report-only の間はこれが唯一の観測手段なので、既定の
		// ログレベルで見えないと意味がない。
		slog.Info("csp-report",
			"directive", directive,
			"blockedUri", r.Body.BlockedURI,
			"documentUri", r.Body.DocumentURI,
		)
		return c.NoContent(http.StatusNoContent)
	}
}
