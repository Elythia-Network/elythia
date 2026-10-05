package app

import (
	"net/http"
	"regexp"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/misc"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// legacyPermRe matches the legacy `account/read` / `notes-write` permission
// form so it can be migrated to the canonical `read:account` / `write:notes`
// (upstream app/create.ts の `v.replace(/^(.+)(\/|-)(read|write)$/, '$3:$1')`)。
var legacyPermRe = regexp.MustCompile(`^(.+)(/|-)(read|write)$`)

// normalizeAppPermissions migrates legacy permission strings to the canonical
// `<read|write>:<entity>` form and de-duplicates (preserving first-occurrence
// order), mirroring upstream app/create.ts の `unique(ps.permission.map(...))`
// (#1557)。
func normalizeAppPermissions(perms []string) model.StringArray {
	seen := make(map[string]struct{}, len(perms))
	out := make(model.StringArray, 0, len(perms))
	for _, p := range perms {
		if m := legacyPermRe.FindStringSubmatch(p); m != nil {
			p = m[3] + ":" + m[1]
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// Handler handles app-related API endpoints.
type Handler struct {
	repo  repository.AuthSessionRepository
	idGen id.Generator
}

// isAuthorizedFor reports the App.isAuthorized value for a viewer (#1557)。
// Returns nil when me is nil so the caller omits the field (upstream は me!=null
// のときだけ含める)。non-nil のときは access_token が存在するか
// (= accessTokensRepository.countBy({appId,userId}) > 0) を返す。
func (h *Handler) isAuthorizedFor(appID string, me *model.User) *bool {
	if me == nil {
		return nil
	}
	_, err := h.repo.FindAccessTokenByAppAndUser(appID, me.ID)
	v := err == nil
	return &v
}

// NewHandler creates a new app Handler.
func NewHandler(repo repository.AuthSessionRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// Create handles POST /api/app/create.
func (h *Handler) Create(c echo.Context) error {
	var req struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Permission  []string `json:"permission"`
		CallbackURL *string  `json:"callbackUrl"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" || req.Description == "" || req.Permission == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name, description, and permission are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	// 認証済みユーザーのIDを取得（任意）
	me := middleware.GetUser(c)
	var userID *string
	if me != nil {
		userID = &me.ID
	}

	a := &model.App{
		ID:          h.idGen.Generate(time.Now()),
		UserID:      userID,
		Secret:      misc.SecureRandomHex(32),
		Name:        req.Name,
		Description: req.Description,
		// legacy permission の正規化 + de-dup (upstream app/create.ts、#1557)。
		Permission:  normalizeAppPermissions(req.Permission),
		CallbackURL: req.CallbackURL,
	}
	if err := h.repo.CreateApp(a); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// upstream app/create.ts は pack(app, null, ...) と me を null 固定で渡すため
	// isAuthorized を常に省略する (authenticated でも含めない、#1557)。me は
	// userID 取得にのみ使う。
	return c.JSON(http.StatusOK, packApp(a, true, nil))
}

// Show handles POST /api/app/show.
func (h *Handler) Show(c echo.Context) error {
	var req struct {
		AppID string `json:"appId"`
	}
	if err := c.Bind(&req); err != nil || req.AppID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "appId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	a, err := h.repo.FindAppByID(req.AppID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_APP", "No such app.", "dce83913-2dc6-4093-8a7b-71dbb11718a3"))
	}

	// upstream app/show は isSecure = (user != null && token == null) を計算し、
	// native session (= users.token) かつ自分の app のときだけ secret を返す。
	// OAuth/MiAuth の app access token (token != null) では owner でも secret を
	// 返さない: secret は access token hash 生成に使う app credential であり、
	// app token に渡すと privilege escalation になる (#1829)。RequireSecure と
	// 同じ native-session 判定 (raw token == users.token) を inline で行う。
	me := middleware.GetUser(c)
	includeSecret := false
	if me != nil && a.UserID != nil && *a.UserID == me.ID &&
		me.Token != nil && *me.Token == middleware.GetToken(c) {
		includeSecret = true
	}

	return c.JSON(http.StatusOK, packApp(a, includeSecret, h.isAuthorizedFor(a.ID, me)))
}

// MyApps handles POST /api/my/apps.
func (h *Handler) MyApps(c echo.Context) error {
	u := middleware.GetUser(c)
	var req struct {
		Limit  *int `json:"limit"`
		Offset *int `json:"offset"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	limit := 10
	if req.Limit != nil {
		limit = *req.Limit
		if limit < 1 {
			limit = 1
		}
		if limit > 100 {
			limit = 100
		}
	}
	offset := 0
	if req.Offset != nil && *req.Offset > 0 {
		offset = *req.Offset
	}

	apps, err := h.repo.ListAppsByUserID(u.ID, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// upstream my/apps は appEntityService.pack(app, me, {detail:true}) を呼び
	// includeSecret 未指定 (= false) なので secret を返さない。secret 取得は
	// app/show (secure session) 経由のみ。app token でも叩ける my/apps で
	// secret を返すと全 app の credential が漏れる (#1900)。
	result := make([]map[string]any, len(apps))
	for i, a := range apps {
		result[i] = packApp(a, false, h.isAuthorizedFor(a.ID, u))
	}
	return c.JSON(http.StatusOK, result)
}

// packApp builds the App entity response. isAuthorized is included only when
// non-nil (upstream は me!=null のときだけ含める、#1557)。
func packApp(a *model.App, includeSecret bool, isAuthorized *bool) map[string]any {
	resp := map[string]any{
		"id":          a.ID,
		"name":        a.Name,
		"callbackUrl": a.CallbackURL,
		"permission":  a.Permission,
	}
	if isAuthorized != nil {
		resp["isAuthorized"] = *isAuthorized
	}
	if includeSecret {
		resp["secret"] = a.Secret
	}
	return resp
}
