package announcements

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// MainStreamPublisher emits events to a user's `main` WebSocket channel.
// Used here to publish `announcementCreated` (per-user announcements) and
// `readAllAnnouncements`. Accepted as an interface to avoid a hard
// dependency on internal/stream (循環依存回避)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// BroadcastPublisher emits an instance-wide event to every connected client
// (internal/stream.BroadcastPublisher)。global announcement の
// announcementCreated 配信に使う (#2056)。
type BroadcastPublisher interface {
	PublishBroadcast(eventType string, body any)
}

// Handler handles announcement-related API endpoints.
type Handler struct {
	repo                repository.AnnouncementRepository
	idGen               id.Generator
	mainStreamPublisher MainStreamPublisher
	broadcastPublisher  BroadcastPublisher
	modLogService       *moderationlog.Service
	userRepo            repository.UserRepository
}

// NewHandler creates a new announcements Handler.
func NewHandler(repo repository.AnnouncementRepository, idGen id.Generator) *Handler {
	return &Handler{repo: repo, idGen: idGen}
}

// SetMainStreamPublisher attaches a publisher used to emit `main` channel
// events (`announcementCreated` / `readAllAnnouncements`). Optional — nil
// disables emit.
func (h *Handler) SetMainStreamPublisher(p MainStreamPublisher) {
	h.mainStreamPublisher = p
}

// SetBroadcastPublisher attaches the broadcast publisher used to emit
// `announcementCreated` for global announcements (#2056). nil disables emit.
func (h *Handler) SetBroadcastPublisher(p BroadcastPublisher) {
	h.broadcastPublisher = p
}

// SetModLogService attaches the moderation log writer used by AdminCreate/
// AdminUpdate/AdminDelete. Wired alongside SetUserRepo so user-targeted
// announcements can resolve {userId, userUsername, userHost} for the log
// info payload. nil leaves logs disabled (graceful for tests).
func (h *Handler) SetModLogService(s *moderationlog.Service) {
	h.modLogService = s
}

// SetUserRepo attaches the user repository for resolving target user
// metadata when emitting per-user announcement moderation logs.
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// logAnnouncementAction writes a moderation_log row for a global or
// user-targeted announcement action. Returns silently if the service or
// actor is missing — audit logging never blocks the underlying action.
//
// globalType / userType の 2 つを取り、announcement の userId 有無で
// どちらの type を使うか分岐する (Misskey TS の AnnouncementService と同じ
// semantics)。info には announcement / before / after を caller から渡す。
//
// Ownership: helper は user-targeted announcement の場合に info map へ
// {userId, userUsername, userHost} を **追記** する。caller は呼び出し後
// に info を再利用しない前提で渡すこと。同 map を別目的で保持する場合は
// あらかじめコピーしてから渡す。
func (h *Handler) logAnnouncementAction(c echo.Context, globalType, userType moderationlog.LogType, a *model.Announcement, info map[string]any) {
	if h.modLogService == nil || a == nil {
		return
	}
	ctx := c.Request().Context()
	actor := middleware.GetUser(c)
	if actor == nil {
		slog.WarnContext(ctx, "moderation log: skipping — actor missing in moderator-only handler",
			"announcementId", a.ID)
		return
	}
	t := globalType
	if a.UserID != nil {
		t = userType
		if h.userRepo != nil {
			if target, err := h.userRepo.FindByID(*a.UserID); err == nil && target != nil {
				info["userId"] = target.ID
				info["userUsername"] = target.Username
				info["userHost"] = target.Host
			}
		}
	}
	h.modLogService.Log(ctx, actor.ID, t, info)
}

// List handles POST /api/announcements.
func (h *Handler) List(c echo.Context) error {
	var req struct {
		Limit     *int   `json:"limit"`
		Offset    int    `json:"offset"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		IsActive  *bool  `json:"isActive"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	activeOnly := true
	if req.IsActive != nil {
		activeOnly = *req.IsActive
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// 認証ユーザーがいればper-user announcementの対象を自分に限定した
	// ListForUserを使い、他ユーザー宛のannouncementを除外する。未認証は
	// ListGlobal(userId IS NULLのみ)を使い、targetedなannouncementを
	// 漏らさない。
	user := middleware.GetUser(c)
	var items []*model.Announcement
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	var err error
	if user != nil {
		items, err = h.repo.ListForUser(user.ID, activeOnly, limit, req.Offset, sinceID, untilID)
	} else {
		items, err = h.repo.ListGlobal(activeOnly, limit, req.Offset, sinceID, untilID)
	}
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// #2106 L52: forYou は viewer 自身宛て announcement で true。匿名は viewerID="" (forYou=false)。
	viewerID := ""
	if user != nil {
		viewerID = user.ID
	}
	result := make([]map[string]any, 0, len(items))
	for _, a := range items {
		item := packAnnouncement(a, h.idGen, viewerID)
		if user != nil {
			read, _ := h.repo.IsRead(user.ID, a.ID)
			item["isRead"] = read
		} else {
			// 匿名 (requireCredential:false) では upstream pack() が isRead を
			// undefined にし key ごと省略する。mk-go の packAnnouncement は常に
			// false を入れるため、ここで削除して shape を揃える (#1955)。
			delete(item, "isRead")
		}
		result = append(result, item)
	}
	return c.JSON(http.StatusOK, result)
}

// ReadAnnouncement handles POST /api/i/read-announcement.
func (h *Handler) ReadAnnouncement(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		AnnouncementID string `json:"announcementId"`
	}
	if err := c.Bind(&req); err != nil || req.AnnouncementID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "announcementId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	a, err := h.repo.FindByID(req.AnnouncementID)
	if err != nil {
		// upstream read-announcement.ts は errors:{} (NO_SUCH_ANNOUNCEMENT 無し)。
		// AnnouncementService.read は insert を try/catch で囲み、不明な id は FK
		// 違反が握り潰されて void (204) になる。mk-go も silent 204 で揃える
		// (#1767。以前は 404 NO_SUCH_ANNOUNCEMENT)。
		return c.NoContent(http.StatusNoContent)
	}
	// per-user announcement のうち他ユーザー宛ては、upstream も ownership check が
	// 無く read を記録して 204 を返すだけ (isActive は変えない)。mk-go は記録せず
	// silent 204 で揃える (foreign read は surface しないため無害、#1767)。
	if a.UserID != nil && *a.UserID != user.ID {
		return c.NoContent(http.StatusNoContent)
	}
	already, _ := h.repo.IsRead(user.ID, req.AnnouncementID)
	if already {
		return c.NoContent(http.StatusNoContent)
	}
	read := &model.AnnouncementRead{
		ID:             h.idGen.Generate(time.Now()),
		UserID:         user.ID,
		AnnouncementID: req.AnnouncementID,
	}
	if err := h.repo.MarkRead(read); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream AnnouncementService.read:214-217: 自分宛て per-user announcement は
	// 初回既読時に isActive=false にして自動 deactivate する (#1767)。best-effort。
	if a.UserID != nil && *a.UserID == user.ID {
		_ = h.repo.UpdateFields(req.AnnouncementID, map[string]any{"isActive": false})
	}
	// TS本家AnnouncementService.read(line 220): 残unread数が0になった時点で
	// mainにreadAllAnnouncementsをpublishする(body: null)。
	if h.mainStreamPublisher != nil {
		if unread, err := h.repo.UnreadForUser(user.ID); err == nil && len(unread) == 0 {
			h.mainStreamPublisher.PublishMainEvent(user.ID, "readAllAnnouncements", nil)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// --- Admin endpoints ---

// AdminCreate handles POST /api/admin/announcements/create.
//
// upstream paramDef は icon を `enum: ['info', 'warning', 'error', 'success']`、
// display を `enum: ['normal', 'banner', 'dialog']` で制約する (#1108 sweep)。
// 旧 mk-go は enum validation 無く silent fallback で任意値を accept していた
// (admin の typo が DB に書かれる drift)。本 fix で upstream 同等の enum reject。
func (h *Handler) AdminCreate(c echo.Context) error {
	var req struct {
		Title string `json:"title"`
		Text  string `json:"text"`
		// upstream paramDef は `imageUrl: {type:'string', nullable:true}` を
		// required に含める。`null` は valid だが省略は 400 なので、両者を
		// 区別できる json.RawMessage で受ける (*string だと不在と null が
		// どちらも nil になり判別できない、#2284)。
		ImageURL               json.RawMessage `json:"imageUrl"`
		Icon                   string          `json:"icon"`
		Display                string          `json:"display"`
		ForExistingUsers       *bool           `json:"forExistingUsers"`
		Silence                *bool           `json:"silence"`
		NeedConfirmationToRead *bool           `json:"needConfirmationToRead"`
		UserID                 *string         `json:"userId"`
	}
	if err := c.Bind(&req); err != nil || req.Title == "" || req.Text == "" || req.ImageURL == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "title, text and imageUrl are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// RawMessage を実値へ落とす。`null` は nil の *string になる。
	var imageURL *string
	if err := json.Unmarshal(req.ImageURL, &imageURL); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "imageUrl must be a string or null.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if req.Icon == "" {
		req.Icon = "info"
	}
	if !isValidAnnouncementIcon(req.Icon) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "icon must be 'info', 'warning', 'error', or 'success'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if req.Display == "" {
		req.Display = "normal"
	}
	if !isValidAnnouncementDisplay(req.Display) {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "display must be 'normal', 'banner', or 'dialog'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	a := &model.Announcement{
		ID:       h.idGen.Generate(time.Now()),
		Title:    req.Title,
		Text:     req.Text,
		ImageURL: imageURL,
		Icon:     req.Icon,
		Display:  req.Display,
		IsActive: true,
		UserID:   req.UserID,
	}
	if req.ForExistingUsers != nil {
		a.ForExistingUsers = *req.ForExistingUsers
	}
	if req.Silence != nil {
		a.Silence = *req.Silence
	}
	if req.NeedConfirmationToRead != nil {
		a.NeedConfirmationToRead = *req.NeedConfirmationToRead
	}
	if err := h.repo.Create(a); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream AnnouncementService.create: per-user (userId 指定) は main へ、
	// global (userId 無し) は broadcast stream へ announcementCreated を流す (#2056)。
	// #2106 L24/L52: create event は upstream 同様 me-less で pack する (viewerID="" → forYou=false)。
	body := map[string]any{"announcement": entity.PackAnnouncement(a, h.idGen, false, "")}
	if a.UserID != nil {
		if h.mainStreamPublisher != nil {
			h.mainStreamPublisher.PublishMainEvent(*a.UserID, "announcementCreated", body)
		}
	} else if h.broadcastPublisher != nil {
		h.broadcastPublisher.PublishBroadcast("announcementCreated", body)
	}
	h.logAnnouncementAction(c, moderationlog.LogCreateGlobalAnnouncement, moderationlog.LogCreateUserAnnouncement, a, map[string]any{
		"announcementId": a.ID,
		"announcement":   a,
	})
	// upstream admin/announcements/create.ts は `packed` (= AnnouncementEntityService
	// .pack の full object) をそのまま返す。`meta.res` の 6 フィールドは型契約/doc 用で、
	// ランタイムでは strip されない (Ajv は paramDef のみ検証)。よって wire 実体は
	// forYou/isRead/icon 等を含む full shape。唯一 imageUrl だけ upstream は raw を返す
	// (admin/list #1967 と同じく proxy しない)。public 用 PackAnnouncement は #1529 で
	// imageUrl を proxy 化するため、その field だけ raw に上書きする (#1977)。
	// #2106 L24: upstream create は me-less pack なので forYou=false (viewerID="")。
	resp := entity.PackAnnouncement(a, h.idGen, false, "")
	resp["imageUrl"] = a.ImageURL // upstream wire は raw imageUrl
	return c.JSON(http.StatusOK, resp)
}

// isValidAnnouncementIcon reports whether v matches upstream's
// `icon: enum: ['info', 'warning', 'error', 'success']`.
func isValidAnnouncementIcon(v string) bool {
	switch v {
	case "info", "warning", "error", "success":
		return true
	}
	return false
}

// isValidAnnouncementDisplay reports whether v matches upstream's
// `display: enum: ['normal', 'banner', 'dialog']`.
func isValidAnnouncementDisplay(v string) bool {
	switch v {
	case "normal", "banner", "dialog":
		return true
	}
	return false
}

// AdminUpdate handles POST /api/admin/announcements/update.
//
// upstream paramDef は id 以外 全 field optional partial update を受け取り、
// imageUrl (nullable) / icon (enum) / display (enum) / forExistingUsers /
// silence / needConfirmationToRead を含む 10 field を accept する (#1108
// sweep)。旧 mk-go は 4 field (id/title/text/isActive) しか受け取らず、
// admin UI で「icon を warning に変える」「display を dialog に変える」
// 等の更新が一切 DB に書かれない drop-in regression があった (= PR #1102
// RolesUpdate と同種の field 欠落 drift)。本 fix で upstream 全 field に
// 拡張 + icon / display は enum 検証で silent 書き換えを防ぐ。
func (h *Handler) AdminUpdate(c echo.Context) error {
	var req struct {
		ID                     string  `json:"id"`
		Title                  *string `json:"title"`
		Text                   *string `json:"text"`
		ImageURL               *string `json:"imageUrl"`
		Icon                   *string `json:"icon"`
		Display                *string `json:"display"`
		ForExistingUsers       *bool   `json:"forExistingUsers"`
		Silence                *bool   `json:"silence"`
		NeedConfirmationToRead *bool   `json:"needConfirmationToRead"`
		IsActive               *bool   `json:"isActive"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "id is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// before snapshot for moderation log + global/user 分岐判定
	before, err := h.repo.FindByID(req.ID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return c.JSON(http.StatusInternalServerError, apierr.InternalError())
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANNOUNCEMENT", "No such announcement.", "d3aae5a7-6372-4cb4-b61c-f511ffc2d7cc"))
	}
	fields := map[string]any{}
	if req.Title != nil {
		fields["title"] = *req.Title
	}
	if req.Text != nil {
		fields["text"] = *req.Text
	}
	if req.ImageURL != nil {
		// upstream nullable: 空文字 → null クリア (= PR #1102 color/iconUrl
		// と同じ Go *string ↔ JSON null 制約 workaround)。
		if *req.ImageURL == "" {
			fields["imageUrl"] = nil
		} else {
			fields["imageUrl"] = *req.ImageURL
		}
	}
	if req.Icon != nil {
		if !isValidAnnouncementIcon(*req.Icon) {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "icon must be 'info', 'warning', 'error', or 'success'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		fields["icon"] = *req.Icon
	}
	if req.Display != nil {
		if !isValidAnnouncementDisplay(*req.Display) {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "display must be 'normal', 'banner', or 'dialog'.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		fields["display"] = *req.Display
	}
	if req.ForExistingUsers != nil {
		fields["forExistingUsers"] = *req.ForExistingUsers
	}
	if req.Silence != nil {
		fields["silence"] = *req.Silence
	}
	if req.NeedConfirmationToRead != nil {
		fields["needConfirmationToRead"] = *req.NeedConfirmationToRead
	}
	if req.IsActive != nil {
		fields["isActive"] = *req.IsActive
	}
	if len(fields) == 0 {
		return c.NoContent(http.StatusNoContent)
	}
	if err := h.repo.UpdateFields(req.ID, fields); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANNOUNCEMENT", "No such announcement.", "d3aae5a7-6372-4cb4-b61c-f511ffc2d7cc"))
	}
	after, err := h.repo.FindByID(req.ID)
	if err != nil {
		return c.NoContent(http.StatusNoContent)
	}
	h.logAnnouncementAction(c, moderationlog.LogUpdateGlobalAnnouncement, moderationlog.LogUpdateUserAnnouncement, before, map[string]any{
		"announcementId": req.ID,
		"before":         before,
		"after":          after,
	})
	return c.NoContent(http.StatusNoContent)
}

// AdminDelete handles POST /api/admin/announcements/delete.
func (h *Handler) AdminDelete(c echo.Context) error {
	var req struct {
		ID string `json:"id"`
	}
	if err := c.Bind(&req); err != nil || req.ID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "id is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// snapshot before delete (log info に含める + global/user 分岐判定)
	snapshot, _ := h.repo.FindByID(req.ID)
	if err := h.repo.Delete(req.ID); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANNOUNCEMENT", "No such announcement.", "ecad8040-a276-4e85-bda9-015a708d291e"))
	}
	if snapshot != nil {
		h.logAnnouncementAction(c, moderationlog.LogDeleteGlobalAnnouncement, moderationlog.LogDeleteUserAnnouncement, snapshot, map[string]any{
			"announcementId": req.ID,
			"announcement":   snapshot,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// AdminList handles POST /api/admin/announcements/list.
//
// Mirrors upstream Misskey TS semantics:
//   - When userId is set, returns only announcements with that userId
//     (the "announcements" tab on the admin user moderation page).
//   - When userId is empty, returns only global announcements
//     (userId IS NULL).
//   - status: "active" / "archived" / "all" (default "active").
//   - Each row carries a reads count (announcement_read row count).
func (h *Handler) AdminList(c echo.Context) error {
	var req struct {
		Limit     *int   `json:"limit"`
		Offset    int    `json:"offset"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
		UserID    string `json:"userId"`
		Status    string `json:"status"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	items, err := h.repo.ListForAdmin(req.UserID, req.Status, limit, req.Offset, sinceID, untilID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	ids := make([]string, 0, len(items))
	for _, a := range items {
		ids = append(ids, a.ID)
	}
	reads, err := h.repo.CountReadsByAnnouncementIDs(ids)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream admin/announcements/list.ts は生 row を返す: public list 用の
	// forYou / isRead は持たず、imageUrl は media-proxy せず raw を返す (#1967)。
	// public 用 packAnnouncement を流用すると非 upstream な forYou/isRead が混入し、
	// forYou の意味 (userId === me.id) とも乖離するため、ここでは明示的に組み立てる。
	const tsFormat = "2006-01-02T15:04:05.000Z"
	out := make([]map[string]any, 0, len(items))
	for _, a := range items {
		createdAt := ""
		if h.idGen != nil {
			if t, err := h.idGen.ParseTime(a.ID); err == nil {
				createdAt = t.UTC().Format(tsFormat)
			}
		}
		var updatedAt *string
		if a.UpdatedAt != nil {
			s := a.UpdatedAt.UTC().Format(tsFormat)
			updatedAt = &s
		}
		out = append(out, map[string]any{
			"id":                     a.ID,
			"createdAt":              createdAt,
			"updatedAt":              updatedAt,
			"title":                  a.Title,
			"text":                   a.Text,
			"imageUrl":               a.ImageURL, // raw (*string, nullable) — proxy しない
			"icon":                   a.Icon,
			"display":                a.Display,
			"isActive":               a.IsActive,
			"forExistingUsers":       a.ForExistingUsers,
			"silence":                a.Silence,
			"needConfirmationToRead": a.NeedConfirmationToRead,
			"userId":                 a.UserID,
			"reads":                  reads[a.ID],
		})
	}
	return c.JSON(http.StatusOK, out)
}

// packAnnouncement wraps entity.PackAnnouncement for the user-facing List/Show.
// isRead defaults to false at this layer; the caller overrides after the viewer
// check. admin 管理用 field (forExistingUsers / isActive) は user-facing には
// 出さない: upstream の user-facing pack (AnnouncementEntityService.pack) はこれらを
// 返さず、admin は AdminList が別途返す (#2101)。
func packAnnouncement(a *model.Announcement, idGen id.Generator, viewerID string) map[string]any {
	return entity.PackAnnouncement(a, idGen, false, viewerID)
}
