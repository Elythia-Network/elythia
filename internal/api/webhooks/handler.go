package webhooks

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/optional"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"

	"github.com/shiroha-a/mk/internal/core/role"
)

// webhookEventTypes mirrors upstream Misskey TS の webhookEventTypes constant
// (third_party/misskey/.../models/Webhook.ts)。i/webhooks/test の type enum
// validation で使用 (#937)。
var webhookEventTypes = map[string]struct{}{
	"mention":  {},
	"unfollow": {},
	"follow":   {},
	"followed": {},
	"note":     {},
	"reply":    {},
	"renote":   {},
	"reaction": {},
}

func isValidWebhookEventType(t string) bool {
	_, ok := webhookEventTypes[t]
	return ok
}

// validateOnArray は upstream Misskey TS の paramDef.on.items.enum と同等の
// 検証を行う (#939)。各要素が webhookEventTypes に含まれていなければ false
// を返す。空配列 / nil は valid (paramDef では required ではない)。
func validateOnArray(on []string) bool {
	for _, e := range on {
		if !isValidWebhookEventType(e) {
			return false
		}
	}
	return true
}

// TestDispatcher is the minimal interface the Test endpoint uses to enqueue
// a synthetic webhook payload. 循環依存を避けるため interface で受ける
// (実装は core/webhook.Service 経由)。DispatchUserTest は指定 webhook 1 件だけに
// 送り、overrideURL/Secret 非空時は保存済 webhook でなくそちらへ送る (#1546)。
type TestDispatcher interface {
	DispatchUserTest(webhookID, userID, eventType string, body any, overrideURL, overrideSecret string)
}

// Handler handles i/webhooks/* endpoints.
type Handler struct {
	repo               repository.WebhookRepository
	idGen              id.Generator
	dispatcher         TestDispatcher
	rolePolicyProvider role.PolicyProvider
}

// NewHandler creates a new webhooks handler.
func NewHandler(repo repository.WebhookRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// SetDispatcher wires a TestDispatcher so that /api/i/webhooks/test can fire
// a synthetic test payload through the production pipeline.
func (h *Handler) SetDispatcher(d TestDispatcher) {
	h.dispatcher = d
}

// SetRolePolicyProvider wires a role policy source so Create enforces the
// `webhookLimit` role policy (#1029).
func (h *Handler) SetRolePolicyProvider(p role.PolicyProvider) {
	h.rolePolicyProvider = p
}

func packWebhook(w *model.Webhook) map[string]any {
	// upstream の webhook.on は常に string[]。nil (旧 row 等) は [] に倒して
	// response が null にならないようにする (#2027)。
	on := w.On
	if on == nil {
		on = []string{}
	}
	return map[string]any{
		"id":     w.ID,
		"userId": w.UserID,
		"name":   w.Name,
		"on":     on,
		"url":    w.URL,
		"secret": w.Secret,
		"active": w.Active,
		// upstream i/webhooks/list.ts:54 は latestSentAt ? toISOString() : null。
		// raw *time.Time だと RFC3339Nano になるため .000Z/null に揃える (#1948-10)。
		"latestSentAt": entity.ISOMillisPtr(w.LatestSentAt),
		"latestStatus": w.LatestStatus,
	}
}

// Create handles POST /api/i/webhooks/create.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Name   string   `json:"name"`
		URL    string   `json:"url"`
		Secret string   `json:"secret"`
		On     []string `json:"on"`
	}
	if err := c.Bind(&req); err != nil || req.Name == "" || req.URL == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "name and url are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	// upstream i/webhooks/create.ts は required:['name','url','on']。on 欠落 (nil) は
	// ajv 同様 400 で弾く (空配列 [] は present 扱いで許容、#2027)。
	if req.On == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "on is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if !validateOnArray(req.On) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "on must contain only webhookEventTypes values.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	// webhookLimit role policy gate (#1029)。policy 経由で取得した上限と
	// 現在保有数を比較。provider 未配線 / policy 不在は gate skip。
	if h.rolePolicyProvider != nil {
		if limit, ok := role.PolicyNumber(h.rolePolicyProvider.GetUserPolicies(user.ID)["webhookLimit"]); ok && limit >= 0 {
			count, err := h.repo.CountByUserID(user.ID)
			if err != nil {
				return apierr.JSONInternalError(c)
			}
			if float64(count) >= limit {
				return apierr.JSONTooManyWebhooks(c)
			}
		}
	}

	webhook := &model.Webhook{
		ID:     h.idGen.Generate(time.Now()),
		UserID: user.ID,
		Name:   req.Name,
		URL:    req.URL,
		Secret: req.Secret,
		On:     req.On,
		Active: true,
	}
	if err := h.repo.Create(webhook); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	return c.JSON(http.StatusOK, packWebhook(webhook))
}

// List handles POST /api/i/webhooks/list.
func (h *Handler) List(c echo.Context) error {
	user := middleware.GetUser(c)
	webhooks, err := h.repo.ListByUserID(user.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	result := make([]map[string]any, len(webhooks))
	for i, w := range webhooks {
		result[i] = packWebhook(w)
	}
	return c.JSON(http.StatusOK, result)
}

// Show handles POST /api/i/webhooks/show.
func (h *Handler) Show(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		WebhookID string `json:"webhookId"`
	}
	if err := c.Bind(&req); err != nil || req.WebhookID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "webhookId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	w, err := h.repo.FindByIDAndUserID(req.WebhookID, user.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_WEBHOOK", "No such webhook.", "50f614d9-3047-4f7e-90d8-ad6b2d5fb098"))
	}

	return c.JSON(http.StatusOK, packWebhook(w))
}

// Update handles POST /api/i/webhooks/update.
func (h *Handler) Update(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		WebhookID string                    `json:"webhookId"`
		Name      string                    `json:"name"`
		URL       string                    `json:"url"`
		Secret    optional.Nullable[string] `json:"secret"`
		On        []string                  `json:"on"`
		Active    *bool                     `json:"active"`
	}
	if err := c.Bind(&req); err != nil || req.WebhookID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "webhookId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if !validateOnArray(req.On) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "on must contain only webhookEventTypes values.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	w, err := h.repo.FindByIDAndUserID(req.WebhookID, user.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_WEBHOOK", "No such webhook.", "fb0fea69-da18-45b1-828d-bd4fd1612518"))
	}

	if req.Name != "" {
		w.Name = req.Name
	}
	if req.URL != "" {
		w.URL = req.URL
	}
	// upstream i/webhooks/update.ts: secret===null ? '' : secret。absent は維持、
	// explicit null は空文字 reset、値は設定 (#1948-15)。
	if req.Secret.Present {
		if req.Secret.Value == nil {
			w.Secret = ""
		} else {
			w.Secret = *req.Secret.Value
		}
	}
	if req.On != nil {
		w.On = req.On
	}
	if req.Active != nil {
		w.Active = *req.Active
	}

	if err := h.repo.Update(w); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// upstream Misskey TS の i/webhooks/update は handler 内で値を return しない
	// ため Endpoint base が 204 を返す (#936)。drop-in 互換のため body 無しの
	// 204 に揃える。caller は更新後 row が必要なら i/webhooks/show で取り直す。
	return c.NoContent(http.StatusNoContent)
}

// Delete handles POST /api/i/webhooks/delete.
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		WebhookID string `json:"webhookId"`
	}
	if err := c.Bind(&req); err != nil || req.WebhookID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "webhookId is required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	if _, err := h.repo.FindByIDAndUserID(req.WebhookID, user.ID); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_WEBHOOK", "No such webhook.", "bae73e5a-5522-4965-ae19-3a8688e71d82"))
	}

	if err := h.repo.Delete(req.WebhookID, user.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	return c.NoContent(http.StatusNoContent)
}

// Test handles POST /api/i/webhooks/test.
// 本家互換: req.type で指定されたイベント種別のテストペイロードを dispatcher
// に渡し、通常の配信パイプラインを通して登録済み webhook に送信する。
//
// upstream Misskey TS の paramDef は webhookId + type を required + type に
// webhookEventTypes enum check を強制している (third_party/misskey/.../i/
// webhooks/test.ts、#937)。
func (h *Handler) Test(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		WebhookID string `json:"webhookId"`
		Type      string `json:"type"`
		// Override (#1546): 保存済みの webhook に重ねて、別の URL / secret へ
		// テスト送信する (upstream test.ts)。重ね方は下の dispatch の直前を参照。
		Override *struct {
			URL    *string `json:"url"`
			Secret *string `json:"secret"`
		} `json:"override"`
	}
	if err := c.Bind(&req); err != nil || req.WebhookID == "" || req.Type == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "webhookId and type are required.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}
	if !isValidWebhookEventType(req.Type) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "type must be one of: mention, unfollow, follow, followed, note, reply, renote, reaction.", "ed1d7571-a3ac-4370-899c-0dbe5e230cc8"))
	}

	webhook, err := h.repo.FindByIDAndUserID(req.WebhookID, user.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_WEBHOOK", "No such webhook.", "0c52149c-e913-18f8-5dc7-74870bfe0cf9"))
	}

	if h.dispatcher != nil {
		// 本家は保存済みの webhook に override を重ねる ({...webhook, ...override})。
		// 片方だけ指定したときは、もう片方に保存済みの値を使う (#3278。
		// admin/system-webhook/test の #3262 と同じ扱い)。url の空文字は送り先に
		// ならないので、指定が無いのと同じに扱う。
		overrideURL, overrideSecret := "", ""
		if req.Override != nil && (req.Override.URL != nil || req.Override.Secret != nil) {
			overrideURL, overrideSecret = webhook.URL, webhook.Secret
			if req.Override.URL != nil && *req.Override.URL != "" {
				overrideURL = *req.Override.URL
			}
			if req.Override.Secret != nil {
				overrideSecret = *req.Override.Secret
			}
		}
		// upstream WebhookTestService は type ごとに pack した形の dummy note/user
		// payload を生成して送る (#1546、#3330)。テスト対象 webhook 1 件だけに、override 指定時は
		// その url/secret へ送る。
		h.dispatcher.DispatchUserTest(webhook.ID, user.ID, req.Type, dummyWebhookBody(req.Type, time.Now()), overrideURL, overrideSecret)
	}

	return c.NoContent(http.StatusNoContent)
}

// HasRolePolicyProvider reports whether the role policy provider was wired.
//
// 未配線だと `webhookLimit` が効かず、webhook を無制限に作れる。起動時検査に
// 使う (#2683)。
func (h *Handler) HasRolePolicyProvider() bool { return h.rolePolicyProvider != nil }
