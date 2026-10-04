// Package charts provides the /api/charts/* and /api/charts/user/*
// endpoints. Each endpoint reads from a single chart engine instance
// and returns the nested-object form of the requested time series.
//
// The handlers are intentionally thin: validation, span/limit/offset
// parsing and JSON shaping live here, while the actual aggregation
// logic stays in internal/core/chart/charts.
package charts

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/core/chart"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Charts is the construction-time bundle of every chart instance the
// handler serves. router.go builds it once when wiring the chart
// management service and passes it into NewHandler.
type Charts struct {
	Notes            *chart.Chart
	Users            *chart.Chart
	Drive            *chart.Chart
	Federation       *chart.Chart
	Instance         *chart.Chart
	ApRequest        *chart.Chart
	ActiveUsers      *chart.Chart
	PerUserNotes     *chart.Chart
	PerUserDrive     *chart.Chart
	PerUserFollowing *chart.Chart
	PerUserPv        *chart.Chart
	PerUserReaction  *chart.Chart
}

// Handler serves every chart endpoint. Each chart pointer is allowed
// to be nil — endpoints whose chart is not configured return a 503-
// equivalent JSON error so wiring bugs are visible to clients without
// crashing the server.
type Handler struct {
	c Charts
	// retentionRepo は POST /api/retention 用。chart engine ではなく
	// 集計済みテーブルを読むだけなので Charts バンドルとは別に持つ。
	retentionRepo repository.RetentionAggregationRepository
}

// NewHandler constructs a handler from a Charts bundle.
func NewHandler(c Charts) *Handler {
	return &Handler{c: c}
}

// Request is the shared request body for every chart endpoint.
// `Limit` defaults to 30 and is clamped to [1, 500]; `Offset` is an
// epoch-milliseconds cursor anchoring the END of the chart window
// (matching upstream `ps.offset ? new Date(ps.offset) : null`). 0 / null
// means "no cursor" so the engine uses its own clock.
type Request struct {
	// Misskey フロントは `misskeyApiGet('charts/...')` で chart を引きに来る
	// ため、`json` タグだけだと GET 経路で query string が bind されず
	// span="" → INVALID_PARAM になる (#421)。POST 経路の挙動を変えない
	// よう既存 `json` タグを残しつつ `query` も付与する。
	//
	// Offset は epoch-ms タイムスタンプ。upstream paramDef は
	// `{type:'integer', nullable:true, default:null}` で、新ミリ秒値が大きい
	// (~1.7e12 で 32bit を超える) ため int64 で受ける。
	Span   string `json:"span" query:"span"`
	Limit  *int   `json:"limit" query:"limit"`
	Offset *int64 `json:"offset" query:"offset"`
	UserID string `json:"userId" query:"userId"`
	Host   string `json:"host" query:"host"`
}

// paramError carries an upstream-compatible (param, reason) pair for a
// failed validation, used to build the INVALID_PARAM `info` object.
type paramError struct {
	param  string
	reason string
}

// --- instance-wide endpoints -------------------------------------------------

// Notes handles POST /api/charts/notes.
func (h *Handler) Notes(c echo.Context) error {
	return h.serveInstance(c, h.c.Notes, false)
}

// Users handles POST /api/charts/users.
func (h *Handler) Users(c echo.Context) error {
	return h.serveInstance(c, h.c.Users, false)
}

// Drive handles POST /api/charts/drive.
func (h *Handler) Drive(c echo.Context) error {
	return h.serveInstance(c, h.c.Drive, false)
}

// Federation handles POST /api/charts/federation.
func (h *Handler) Federation(c echo.Context) error {
	return h.serveInstance(c, h.c.Federation, false)
}

// Instance handles POST /api/charts/instance. The chart is grouped by
// host so the request must supply a non-empty host.
func (h *Handler) Instance(c echo.Context) error {
	return h.serveInstance(c, h.c.Instance, true)
}

// ApRequest handles POST /api/charts/ap-request.
func (h *Handler) ApRequest(c echo.Context) error {
	return h.serveInstance(c, h.c.ApRequest, false)
}

// ActiveUsers handles POST /api/charts/active-users.
func (h *Handler) ActiveUsers(c echo.Context) error {
	return h.serveInstance(c, h.c.ActiveUsers, false)
}

// --- per-user endpoints ------------------------------------------------------

// UserNotes handles POST /api/charts/user/notes.
func (h *Handler) UserNotes(c echo.Context) error {
	return h.servePerUser(c, h.c.PerUserNotes)
}

// UserDrive handles POST /api/charts/user/drive.
func (h *Handler) UserDrive(c echo.Context) error {
	return h.servePerUser(c, h.c.PerUserDrive)
}

// UserFollowing handles POST /api/charts/user/following.
func (h *Handler) UserFollowing(c echo.Context) error {
	return h.servePerUser(c, h.c.PerUserFollowing)
}

// UserPv handles POST /api/charts/user/pv.
func (h *Handler) UserPv(c echo.Context) error {
	return h.servePerUser(c, h.c.PerUserPv)
}

// UserReactions handles POST /api/charts/user/reactions.
func (h *Handler) UserReactions(c echo.Context) error {
	return h.servePerUser(c, h.c.PerUserReaction)
}

// --- shared serving logic ----------------------------------------------------

// serveInstance executes an instance-wide chart query. requireHost is
// true for the host-grouped "instance" chart and false otherwise.
func (h *Handler) serveInstance(c echo.Context, ch *chart.Chart, requireHost bool) error {
	if ch == nil {
		return chartUnavailable(c)
	}
	var req Request
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParamClient(c, "#", "Invalid request body.")
	}
	span, amount, cursor, perr := h.parseRequest(&req)
	if perr != nil {
		return invalidParam(c, perr)
	}
	group := ""
	if requireHost {
		if req.Host == "" {
			return invalidParam(c, &paramError{param: "#/required", reason: "must have required property 'host'"})
		}
		group = req.Host
	}
	return h.respond(c, ch, span, amount, cursor, group)
}

// servePerUser executes a per-user chart query. The chart is always
// grouped so the request must supply a non-empty userId.
func (h *Handler) servePerUser(c echo.Context, ch *chart.Chart) error {
	if ch == nil {
		return chartUnavailable(c)
	}
	var req Request
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParamClient(c, "#", "Invalid request body.")
	}
	span, amount, cursor, perr := h.parseRequest(&req)
	if perr != nil {
		return invalidParam(c, perr)
	}
	if req.UserID == "" {
		return invalidParam(c, &paramError{param: "#/required", reason: "must have required property 'userId'"})
	}
	return h.respond(c, ch, span, amount, cursor, req.UserID)
}

// respond runs the actual GetChart call and writes the JSON response.
func (h *Handler) respond(c echo.Context, ch *chart.Chart, span chart.Span, amount int, cursor *time.Time, group string) error {
	result, err := ch.GetChart(c.Request().Context(), span, amount, cursor, group)
	if err != nil {
		return internalError(c)
	}
	// upstream は cacheSec を成功応答 (call().then()) でのみ set し、
	// validation/exec 失敗 (catch) には付けない。同じく 200 経路だけで set する。
	setCacheControl(c)
	return c.JSON(http.StatusOK, chart.Unflatten(result))
}

// parseRequest validates the shared chart fields and derives the window
// cursor from the offset param.
//
// Validation rules (matching upstream paramDef + ajv):
//   - span is required and must be "hour" or "day"
//   - limit defaults to 30 and must be within [1, 500]
//   - offset is an epoch-milliseconds cursor (nullable, default null).
//     Following upstream `ps.offset ? new Date(ps.offset) : null`, a zero
//     or absent offset yields a nil cursor (engine uses its own clock);
//     any other value (including negative = pre-1970) anchors the window
//     end at that instant. The engine rounds it up (ceil) to the span
//     bucket the cursor falls into, matching upstream getChartRaw.
func (h *Handler) parseRequest(req *Request) (chart.Span, int, *time.Time, *paramError) {
	span := chart.Span(req.Span)
	if req.Span == "" {
		return "", 0, nil, &paramError{param: "#/required", reason: "must have required property 'span'"}
	}
	if span != chart.SpanHour && span != chart.SpanDay {
		return "", 0, nil, &paramError{param: "#/properties/span/enum", reason: "must be equal to one of the allowed values"}
	}
	amount := 30
	if req.Limit != nil {
		if *req.Limit < 1 {
			return "", 0, nil, &paramError{param: "#/properties/limit/minimum", reason: "must be >= 1"}
		}
		if *req.Limit > 500 {
			return "", 0, nil, &paramError{param: "#/properties/limit/maximum", reason: "must be <= 500"}
		}
		amount = *req.Limit
	}
	var cursor *time.Time
	if req.Offset != nil && *req.Offset != 0 {
		t := time.UnixMilli(*req.Offset)
		cursor = &t
	}
	return span, amount, cursor, nil
}

// setCacheControl mirrors upstream ApiCallService: an unauthenticated GET
// to a cacheSec endpoint receives `Cache-Control: public, max-age=<cacheSec>`.
// 全 chart endpoint は `allowGet:true, cacheSec:60*60` なので max-age=3600。
// POST / 認証済みリクエストには付けない。auth は e.Use(Authenticate()) で全
// request に optional 適用済みのため、RequireAuth 無しの chart route でも
// GetUser/GetToken が populate される (未認証なら nil / "")。
func setCacheControl(c echo.Context) {
	// upstream `!token && !user` の token は raw token なので、無効/suspended token を
	// 付けた GET でも cache を付けない (HasRawToken、#2049)。
	if c.Request().Method == http.MethodGet &&
		middleware.GetUser(c) == nil &&
		!middleware.HasRawToken(c) {
		c.Response().Header().Set("Cache-Control", "public, max-age=3600")
	}
}

// --- error helpers -----------------------------------------------------------

func invalidParam(c echo.Context, perr *paramError) error {
	return c.JSON(http.StatusBadRequest, apierr.InvalidParamClient(perr.param, perr.reason))
}

func internalError(c echo.Context) error {
	return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "9c2cb6b3-ec89-4ce4-9b3a-2f5c66c2af6b"))
}

func chartUnavailable(c echo.Context) error {
	return c.JSON(http.StatusServiceUnavailable, apierr.Error("CHART_UNAVAILABLE", "Chart not available.", "5e3a8d12-6c4f-4d70-b3bc-1f3dd0c7a37a"))
}
