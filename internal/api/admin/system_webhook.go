package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/elythia-network/elythia/internal/api/apierr"
	coreabuse "github.com/elythia-network/elythia/internal/core/abuse"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// packSystemWebhook serializes a SystemWebhook into the upstream
// SystemWebhookEntityService.pack shape (SystemWebhookEntityService.ts:33-43):
// the same 9 fields the model carries, but with updatedAt / latestSentAt
// rendered via Date.toISOString() (.000Z) rather than the model's raw time.Time
// (which encoding/json would emit as RFC3339Nano). Returning the model directly
// diverged on those two timestamp fields (#1948-10).
func packSystemWebhook(w *model.SystemWebhook) map[string]any {
	// on は string[] 必須 (non-null)。nil model.StringArray は null になるため
	// [] へ coalesce する (model 直返し時の model.StringArray と同じ array 表現を維持)。
	on := []string(w.On)
	if on == nil {
		on = []string{}
	}
	return map[string]any{
		"id":           w.ID,
		"isActive":     w.IsActive,
		"updatedAt":    entity.ISOMillis(w.UpdatedAt),
		"latestSentAt": entity.ISOMillisPtr(w.LatestSentAt),
		"latestStatus": w.LatestStatus,
		"name":         w.Name,
		"on":           on,
		"url":          w.URL,
		"secret":       w.Secret,
	}
}

// systemWebhookEventTypes is the allow-list of system webhook `on` event types,
// mirroring upstream models/SystemWebhook.ts systemWebhookEventTypes (#1542)。
var systemWebhookEventTypes = map[string]bool{
	"abuseReport":                             true,
	"abuseReportResolved":                     true,
	"userCreated":                             true,
	"inactiveModeratorsWarning":               true,
	"inactiveModeratorsInvitationOnlyChanged": true,
}

// containsAllEvents reports whether have contains every entry of want
// (PostgreSQL の `want <@ have`)。
func containsAllEvents(have, want []string) bool {
	for _, w := range want {
		found := false
		for _, h := range have {
			if h == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// validSystemWebhookEvents reports whether every entry of `on` is a known event
// type. 空配列も valid (= required 検証は別途行う)。
func validSystemWebhookEvents(on []string) bool {
	for _, e := range on {
		if !systemWebhookEventTypes[e] {
			return false
		}
	}
	return true
}

// errNoSuchSystemWebhook is upstream show.ts noSuchSystemWebhook。mk-go は
// update / delete でも同じものを返す (本家はそこで findOneByOrFail が投げて 500
// になる。docs/divergence/security.md 7、#3262)。
func errNoSuchSystemWebhook(c echo.Context) error {
	return c.JSON(http.StatusNotFound, apierr.ErrorWithKind("NO_SUCH_SYSTEM_WEBHOOK", "No such SystemWebhook.", "38dd1ffe-04b4-6ff5-d8ba-4e6a6ae22c9d", apierr.KindServer))
}

// validSystemWebhookFields checks the length limits of upstream create.ts /
// update.ts (name 1-255, url 1-1024, secret 0-1024)。nil は未送出で検査しない。
// 検査しないと列の長さ (varchar) を超えた値が DB で弾かれて 500 になる。
// 長さはコードポイントで数える (本家の ajv も PostgreSQL の varchar も同じ)。
// NUL と不正な UTF-8 も列に入らないので、colfit.Fits で一緒に見る (#3022)。
func validSystemWebhookFields(name, url, secret *string) bool {
	if name != nil && (*name == "" || !colfit.Fits(*name, 255)) {
		return false
	}
	if url != nil && (*url == "" || !colfit.Fits(*url, 1024)) {
		return false
	}
	if secret != nil && !colfit.Fits(*secret, 1024) {
		return false
	}
	return true
}

// dummyWebhookUser mirrors one of upstream WebhookTestService の dummyUser1-3。
func dummyWebhookUser(n int, followers, following, notes int) *model.User {
	id := fmt.Sprintf("dummy-user-%d", n)
	name := fmt.Sprintf("DummyUser%d", n)
	username := fmt.Sprintf("dummy%d", n)
	return &model.User{
		ID: id, Username: username, UsernameLower: username, Name: &name,
		FollowersCount: followers, FollowingCount: following, NotesCount: notes,
		IsCat: true, IsExplorable: true,
	}
}

// dummyWebhookUserLite packs a dummy user like upstream
// WebhookTestService.toPackedUserLite: アバターは空、onlineStatus は active、
// バッジは無し。実在しない利用者なので、本物の packer の判定 (identicon や
// ロールの引き当て) をそのまま出さない。
func dummyWebhookUserLite(u *model.User) *entity.UserLite {
	if u == nil {
		return nil
	}
	lite := entity.PackUserLite(u)
	lite.AvatarURL = ""
	lite.AvatarBlurhash = nil
	lite.OnlineStatus = "active"
	lite.BadgeRoles = &[]any{}
	return &lite
}

// dummySystemWebhookBody builds the test payload per system event type,
// mirroring upstream WebhookTestService.testSystemWebhook (#3262)。本文の形は
// 本配送と同じ関数で作る (通報は coreabuse.WebhookPayload)。
func dummySystemWebhookBody(eventType string) any {
	user1 := dummyWebhookUser(1, 10, 5, 30)
	user2 := dummyWebhookUser(2, 40, 50, 900)
	user3 := dummyWebhookUser(3, 60, 70, 15900)
	switch eventType {
	case "abuseReport", "abuseReportResolved":
		report := &model.AbuseUserReport{
			ID:             "dummy-abuse-report1",
			TargetUserID:   user1.ID,
			ReporterID:     user2.ID,
			Comment:        "This is a dummy report for testing purposes.",
			ModerationNote: "foo",
		}
		var assignee *model.User
		if eventType == "abuseReportResolved" {
			report.Resolved = true
			report.AssigneeID = &user3.ID
			assignee = user3
		}
		body := coreabuse.WebhookPayload(report, user2, user1, assignee, coreabuse.UserLookups{}, nil)
		body["reporter"] = dummyWebhookUserLite(user2)
		body["targetUser"] = dummyWebhookUserLite(user1)
		if assignee != nil {
			body["assignee"] = dummyWebhookUserLite(assignee)
		}
		// 本配送は通報の id から作成時刻を出す (mk-go の追加の項目)。ダミーの id
		// からは出せないので、送る時刻を入れる。
		body["createdAt"] = entity.ISOMillis(time.Now())
		return body
	case "userCreated":
		return dummyWebhookUserLite(user1)
	case "inactiveModeratorsWarning":
		return map[string]any{"remainingTime": map[string]any{
			"time":    100000,
			"asDays":  1,
			"asHours": 24,
		}}
	default:
		// inactiveModeratorsInvitationOnlyChanged
		return map[string]any{}
	}
}

// SystemWebhookCreate handles POST /api/admin/system-webhook/create.
func (h *Handler) SystemWebhookCreate(c echo.Context) error {
	if h.systemWebhookRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		Name   string `json:"name"`
		URL    string `json:"url"`
		Secret string `json:"secret"`
		// upstream create.ts required:[isActive,name,on,url]。欠落を 400 で弾く
		// ため On/IsActive はポインタで「未送出 (nil)」を判別する (#1542)。
		On       *[]string `json:"on"`
		IsActive *bool     `json:"isActive"`
	}
	if err := c.Bind(&req); err != nil || req.On == nil || req.IsActive == nil {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	if !validSystemWebhookFields(&req.Name, &req.URL, &req.Secret) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("name, url or secret is out of range."))
	}
	// on は systemWebhookEventTypes enum のみ受理する (upstream create.ts)。
	if !validSystemWebhookEvents(*req.On) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("on contains an unknown event type."))
	}
	sw := &model.SystemWebhook{
		ID:        h.idGen.Generate(time.Now()),
		Name:      req.Name,
		URL:       req.URL,
		Secret:    req.Secret,
		On:        *req.On,
		IsActive:  *req.IsActive,
		UpdatedAt: time.Now(),
	}
	if err := h.systemWebhookRepo.Create(sw); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.logModeration(c, moderationlog.LogCreateSystemWebhook, map[string]any{
		"systemWebhookId": sw.ID,
		"webhook":         sw,
	})
	return c.JSON(http.StatusOK, packSystemWebhook(sw))
}

// SystemWebhookDelete handles POST /api/admin/system-webhook/delete.
func (h *Handler) SystemWebhookDelete(c echo.Context) error {
	if h.systemWebhookRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("id is required."))
	}
	snapshot, err := h.systemWebhookRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return errNoSuchSystemWebhook(c)
	}
	if err := h.systemWebhookRepo.Delete(req.ID); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.logModeration(c, moderationlog.LogDeleteSystemWebhook, map[string]any{
		"systemWebhookId": req.ID,
		"webhook":         snapshot,
	})
	return c.NoContent(http.StatusNoContent)
}

// SystemWebhookList handles POST /api/admin/system-webhook/list.
func (h *Handler) SystemWebhookList(c echo.Context) error {
	if h.systemWebhookRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// 本家 list.ts の絞り込み。on は「指定した種類をすべて含む」(`:on <@ on`)。
	var req struct {
		IsActive *bool    `json:"isActive"`
		On       []string `json:"on"`
	}
	if err := c.Bind(&req); err != nil || !validSystemWebhookEvents(req.On) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	rows, err := h.systemWebhookRepo.List()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	out := make([]map[string]any, 0, len(rows))
	for _, w := range rows {
		if req.IsActive != nil && w.IsActive != *req.IsActive {
			continue
		}
		if !containsAllEvents(w.On, req.On) {
			continue
		}
		out = append(out, packSystemWebhook(w))
	}
	return c.JSON(http.StatusOK, out)
}

// SystemWebhookShow handles POST /api/admin/system-webhook/show.
func (h *Handler) SystemWebhookShow(c echo.Context) error {
	if h.systemWebhookRepo == nil {
		return c.JSON(http.StatusNotFound, apierr.NotFound())
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("id is required."))
	}
	sw, err := h.systemWebhookRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return errNoSuchSystemWebhook(c)
	}
	return c.JSON(http.StatusOK, packSystemWebhook(sw))
}

// SystemWebhookTest handles POST /api/admin/system-webhook/test.
//
// upstream test.ts: webhook 不在なら NO_SUCH_WEBHOOK、override.url/secret を受け、
// 配送 processor 経由で type 別 dummy payload を送る。mk-go も DispatchSystemTest
// で real delivery (queue) を経由させ、header/envelope を本配送と一致させる (#1542)。
func (h *Handler) SystemWebhookTest(c echo.Context) error {
	var req struct {
		WebhookID string `json:"webhookId"`
		Type      string `json:"type"`
		Override  *struct {
			URL    *string `json:"url"`
			Secret *string `json:"secret"`
		} `json:"override"`
	}
	if err := c.Bind(&req); err != nil || req.WebhookID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("webhookId is required."))
	}
	// type は本家では必須で、決まった種類だけを受ける (#3262)。検査しないと、空や
	// 未知の種類名のまま本文の無いテストを送ってしまう。
	if !systemWebhookEventTypes[req.Type] {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("type must be a system webhook event type."))
	}
	if h.systemWebhookRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	sw, err := h.systemWebhookRepo.FindByID(req.WebhookID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_WEBHOOK", "No such webhook.", "0c52149c-e913-18f8-5dc7-74870bfe0cf9"))
	}
	if h.systemWebhookDispatcher == nil {
		// dispatcher 未配線時は no-op (DB lookup の NO_SUCH_WEBHOOK 検証は通す)。
		return c.NoContent(http.StatusNoContent)
	}
	// 本家は保存済みの webhook に override を重ねる ({...webhook, ...override})。
	// 片方だけ指定したときは、もう片方は保存済みの値を使う (#3262)。url の空文字は
	// 送り先にならないので、指定が無いのと同じに扱う。
	var overrideURL, overrideSecret string
	if req.Override != nil && (req.Override.URL != nil || req.Override.Secret != nil) {
		overrideURL, overrideSecret = sw.URL, sw.Secret
		if req.Override.URL != nil && *req.Override.URL != "" {
			overrideURL = *req.Override.URL
		}
		if req.Override.Secret != nil {
			overrideSecret = *req.Override.Secret
		}
	}
	h.systemWebhookDispatcher.DispatchSystemTest(sw.ID, req.Type, dummySystemWebhookBody(req.Type), overrideURL, overrideSecret)
	return c.NoContent(http.StatusNoContent)
}

// SystemWebhookUpdate handles POST /api/admin/system-webhook/update.
//
// 配送 processor が並行して latestSentAt/latestStatus を書き換えるため、
// FindByID→Save で全列上書きすると配送ステータスを古い値で踏み潰す。partial
// update (UpdateAdminFields) を使い admin 編集可能列のみ触る。
func (h *Handler) SystemWebhookUpdate(c echo.Context) error {
	if h.systemWebhookRepo == nil {
		return c.NoContent(http.StatusNoContent)
	}
	var req struct {
		ID       string    `json:"id"`
		Name     *string   `json:"name"`
		URL      *string   `json:"url"`
		Secret   *string   `json:"secret"`
		On       *[]string `json:"on"`
		IsActive *bool     `json:"isActive"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("Invalid parameters."))
	}
	if !validSystemWebhookFields(req.Name, req.URL, req.Secret) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("name, url or secret is out of range."))
	}
	// on は systemWebhookEventTypes enum のみ受理 (upstream update.ts、#1542)。本家は
	// パラメータをすべて検査してから引くので、存在の確認より前に見る (#3262)。
	if req.On != nil && !validSystemWebhookEvents(*req.On) {
		return c.JSON(http.StatusBadRequest, apierr.InvalidParam("on contains an unknown event type."))
	}
	// upstream update.ts は required:['id','isActive','name','on','url'] だが、
	// mk-go は意図的に partial update (id 以外は省略可) を許す superset 挙動を維持
	// する (#1772 で確認)。frontend MkSystemWebhookEditor は常に全フィールドを送る
	// ため drop-in 互換に影響せず、partial 対応は mk-go の追加の柔軟性。
	// 存在確認 (GORM Updates(map) は 0 行影響でも nil を返すため)
	before, err := h.systemWebhookRepo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return errNoSuchSystemWebhook(c)
	}
	fields := map[string]any{"updatedAt": time.Now()}
	if req.Name != nil {
		fields["name"] = *req.Name
	}
	if req.URL != nil {
		fields["url"] = *req.URL
	}
	if req.Secret != nil {
		fields["secret"] = *req.Secret
	}
	if req.On != nil {
		// model.StringArray でラップしないと GORM Updates(map) が空 string[] を
		// NULL 化して NOT NULL 制約違反になる (#932、#931 / #896 と同 class)。
		fields["on"] = model.StringArray(*req.On)
	}
	if req.IsActive != nil {
		fields["isActive"] = *req.IsActive
	}
	if err := h.systemWebhookRepo.UpdateAdminFields(req.ID, fields); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	sw, err := h.systemWebhookRepo.FindByID(req.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	h.logModeration(c, moderationlog.LogUpdateSystemWebhook, map[string]any{
		"systemWebhookId": req.ID,
		"before":          before,
		"after":           sw,
	})
	return c.JSON(http.StatusOK, packSystemWebhook(sw))
}
