package admin

import (
	"context"
	"net/http"

	"github.com/elythia-network/elythia/internal/core/selfcheck"
	"github.com/labstack/echo/v4"
)

// SelfCheckRunner runs the federation / dependency self-checks.
// 循環依存を避けるため interface で受け取る。実装は server の adapter。
type SelfCheckRunner interface {
	RunSelfCheck(ctx context.Context) selfcheck.Report
}

// SetSelfCheckRunner wires the self-check source (#2463)。
func (h *Handler) SetSelfCheckRunner(r SelfCheckRunner) {
	h.selfCheck = r
}

// SelfCheck handles POST /api/admin/self-check.
//
// **mk-go 独自 endpoint** (#2463)。upstream に対応物は無い。
//
// 検査の宛先は config の `url` に固定されており、**リクエストから指定できない**。
// 自ホストは loopback / private IP に解決されうるため検査用の client は SSRF
// ガードを通さないので、宛先を外から与えられる口を作らないことが安全性の前提。
func (h *Handler) SelfCheck(c echo.Context) error {
	if h.selfCheck == nil {
		return c.JSON(http.StatusOK, selfcheck.Report{Results: []selfcheck.Result{}, OK: true})
	}
	return c.JSON(http.StatusOK, h.selfCheck.RunSelfCheck(c.Request().Context()))
}
