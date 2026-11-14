package server

import (
	"log/slog"
	"slices"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/server/paramshadow"
)

// paramShadowContextKey is the echo.Context key under which the /api group
// hands the param validation to withParamShadow.
const paramShadowContextKey = "mk.server.paramShadow"

// paramUnvalidatedEndpoints are the endpoints with a paramDef whose params
// are not validated with it. Every other endpoint with a paramDef, upstream's
// or Elythia's own (elythia_paramdefs.json, #3869), is validated and rejected
// with upstream's 400 (paramEnforcedEndpoints).
//
// reset-dbはテストのモードでだけ登録する。宣言(properties: {})はどのobjectも
// 通し、objectでないbodyはrequireObjectBodyが本家と同じ400にするので、検証に
// 載せても変わらない。手書きの検査との照合(TestEnforcedEndpointsMatchTheCorpus)
// で流すと、テストのサーバーのDBを実際に消すので、載せないでおく。#4172までは
// 検証だけを走らせて件数を数えていた(#3677)。
var paramUnvalidatedEndpoints = []string{"reset-db"}

// paramEnforcedEndpoints returns, sorted, the names of eps (a
// paramschema.Effective result) but paramUnvalidatedEndpoints: the endpoints
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
		if !slices.Contains(paramUnvalidatedEndpoints, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// paramShadowMiddleware returns the /api group middleware that enables the
// param validation for the request: a request to one of
// paramEnforcedEndpoints that its paramDef rejects gets upstream's 400. The
// validation itself runs in withParamShadow, at the end of the route's
// middleware. It returns nil only when the embedded upstream paramDefs do not
// load; the handlers' own checks then stay the only ones.
func (s *Server) paramShadowMiddleware() echo.MiddlewareFunc {
	// 本家のparamDefに、Elythiaが本家の外で受ける値を重ねたものを使う(#3865)。
	eps, err := paramschema.Effective()
	if err != nil {
		slog.Warn("param validation disabled: the embedded upstream paramDefs or Elythia's overlays do not load", "err", err)
		return nil
	}
	enforced := paramEnforcedEndpoints(eps)
	validated := make(map[string]paramschema.Endpoint, len(enforced))
	for _, name := range enforced {
		ep := eps[name]
		if ep.CompileErr != nil {
			slog.Warn("param validation: a paramDef does not compile; its requests go to the handler unvalidated",
				"endpoint", name, "err", ep.CompileErr)
		}
		validated[name] = ep
	}
	return paramValidationGroupMiddleware(paramshadow.Middleware(validated))
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

// withParamShadow runs the param validation around next when the /api group
// enabled it for the request. apiRoutes appends it after the route's
// own middleware, so it sees the requests that passed the credential and
// permission checks: the requests upstream hands to ajv.
//
// **routeのmiddlewareの末尾で走らせる。** groupのmiddlewareはrouteの認証・
// 権限の検査より先に走るので、そこで検証すると、本家ならajvに届かない
// リクエスト(401 / 403になるもの)を400で拒否してしまう。
func withParamShadow(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if shadow, ok := c.Get(paramShadowContextKey).(echo.MiddlewareFunc); ok {
			return shadow(next)(c)
		}
		return next(c)
	}
}
