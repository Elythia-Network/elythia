package server

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/server/paramshadow"
)

// paramShadowContextKey is the echo.Context key under which the /api group
// hands the param shadow validation (#3677) to withParamShadow.
const paramShadowContextKey = "mk.server.paramShadow"

// paramCountedOnlyEndpoints are the endpoints with a paramDef that are only
// validated and counted (#3677), not rejected. Every other endpoint with a
// paramDef, upstream's or Elythia's own (elythia_paramdefs.json, #3869), is
// validated and rejected with upstream's 400 (paramEnforcedEndpoints).
//
// reset-dbはテストのモードでだけ登録する。宣言(properties: {})はどのobjectも
// 通すので、拒否に載せても変わらない。手書きの検査との照合
// (TestEnforcedEndpointsMatchTheCorpus)で流すと、テストのサーバーのDBを実際に
// 消すので、数えるだけにしておく。
var paramCountedOnlyEndpoints = []string{"reset-db"}

// paramEnforcedEndpoints returns, sorted, the names of eps (a
// paramschema.Effective result) but paramCountedOnlyEndpoints: the endpoints
// whose params are validated with their paramDef and rejected with upstream's
// 400.
//
// 本家に揃える対象は、パッケージ単位で一覧に足して広げてきた(#3742から
// #3888まで、#3667の(3)と(4))。宣言のあるendpointが残らず載ったので、一覧を
// 手で並べるのをやめ、宣言から決める(#3890)。宣言を足せば、そのendpointは
// 拒否の対象になる。手書きの検査が本家より厳しくないかは、
// TestEnforcedEndpointsMatchTheCorpusがajvのcorpusで確かめる。宣言の無い
// ルートは、TestAPIRoutesHaveParamDefsが落とす。
func paramEnforcedEndpoints(eps map[string]paramschema.Endpoint) []string {
	out := make([]string, 0, len(eps))
	for name := range eps {
		if !slices.Contains(paramCountedOnlyEndpoints, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// paramShadowMiddleware returns the /api group middleware that enables the
// param validation for the request: rejecting for paramEnforcedEndpoints,
// and counting only (the param shadow validation) for the other upstream
// endpoints unless disableParamShadow. The validation itself runs in
// withParamShadow, at the end of the route's middleware. It returns nil only
// when the embedded upstream paramDefs do not load; the handlers' own checks
// then stay the only ones.
//
// It also adds the part paramShadowRecorder (#3699), whose stop writes the
// counts of this server generation.
func (s *Server) paramShadowMiddleware() echo.MiddlewareFunc {
	// 本家のparamDefに、Elythiaが本家の外で受ける値を重ねたものを使う(#3865)。
	// 数えるだけの検証も同じものを使うので、独自の値を差として数えない。
	eps, err := paramschema.Effective()
	if err != nil {
		slog.Warn("param validation disabled: the embedded upstream paramDefs or Elythia's overlays do not load", "err", err)
		return nil
	}
	enforced := paramEnforcedEndpoints(eps)
	if s.paramEnforced != nil {
		enforced = s.paramEnforced
	}
	opts := []paramshadow.Option{paramshadow.WithEnforced(enforced...)}
	if s.config.DisableParamShadow {
		// 止めるのは数えるだけの検証。本家に揃えた分の拒否は止めない。記録は
		// 検証器のpanicとcompileの失敗だけになる。
		opts = append(opts, paramshadow.WithoutShadow())
		slog.Info("param shadow validation disabled (disableParamShadow); the enforced endpoints are still validated",
			"enforced", len(enforced))
	}
	for name, ep := range eps {
		if ep.CompileErr != nil {
			slog.Warn("param validation: an upstream paramDef does not compile; its requests go to the handler and are counted as compile_error",
				"endpoint", name, "err", ep.CompileErr)
		}
	}
	interval := time.Duration(s.config.ParamShadowLogSeconds) * time.Second
	rec := paramshadow.NewRecorder(slog.Default(), interval)
	// 定期の要約は、リクエストのgoroutineではなく世代の箱(bgwork)で書く。
	// 箱が止まった後(停止の途中)は、下のhookが最後の分を書く。
	rec.SetAsync(func(flush func()) bool {
		// 箱が止まり始めていたら任せない(Recordが自分で書く)。GoAsに渡すと、
		// 捨てていないのに「捨てた」と警告し、Droppedを数えてしまう(#3677の
		// レビュー2周目L-1)。見てからGoAsまでの間に止まり始めた場合だけは残る。
		select {
		case <-s.background.Stopping():
			return false
		default:
		}
		return s.background.GoAs("paramshadow summary", func(context.Context) { flush() })
	})
	// 停止のときに、最後の要約からの分を出す。メンテナンスで作り直す世代も
	// それぞれが自分の分を出す。
	s.addCloser("paramShadowRecorder", rec)
	shadow := paramshadow.Middleware(eps, rec, opts...)
	if !s.config.DisableParamShadow {
		slog.Info("param shadow validation enabled", "endpoints", len(eps), "enforced", len(enforced), "logInterval", interval.String())
	}
	return paramValidationGroupMiddleware(shadow)
}

// paramValidationGroupMiddleware hands shadow to withParamShadow.
func paramValidationGroupMiddleware(shadow echo.MiddlewareFunc) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set(paramShadowContextKey, shadow)
			return next(c)
		}
	}
}

// withParamShadow runs the param shadow validation around next when the /api
// group enabled it for the request. apiRoutes appends it after the route's
// own middleware, so it sees the requests that passed the credential and
// permission checks: the requests upstream hands to ajv.
//
// **routeのmiddlewareの末尾で走らせる。** groupのmiddlewareはrouteの認証・
// 権限の検査より先に走るので、そこで検証すると、本家ならajvに届かない
// リクエスト(401 / 403になるもの)まで数えてしまう。
func withParamShadow(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if shadow, ok := c.Get(paramShadowContextKey).(echo.MiddlewareFunc); ok {
			return shadow(next)(c)
		}
		return next(c)
	}
}
