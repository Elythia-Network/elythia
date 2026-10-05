// Package flash provides /api/flash/* endpoints.
package flash

import (
	"context"
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	coreflash "github.com/elythia-network/elythia/internal/core/flash"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// RoleChecker reports whether a user is a moderator. Satisfied by
// *core/role.Service. nil 配線時は全員 non-moderator 扱い (#1548)。
type RoleChecker interface {
	IsModerator(userID string) bool
}

// ModLogger records a moderation action. Satisfied by *core/moderationlog.Service.
// nil 配線時は記録を skip する (#1548)。
type ModLogger interface {
	Log(ctx context.Context, moderatorID string, t moderationlog.LogType, info map[string]any)
}

// Handler handles flash-related API endpoints.
type Handler struct {
	svc      *coreflash.Service
	userRepo repository.UserRepository
	idGen    id.Generator
	roles    RoleChecker
	modLog   ModLogger
}

// NewHandler creates a new flash Handler.
// userRepo / idGen are required by list endpoints to embed the
// `user` (UserLite) and `createdAt` fields that the upstream Misskey
// frontend expects when rendering Play (Flash) cards.
func NewHandler(svc *coreflash.Service, userRepo repository.UserRepository, idGen id.Generator) *Handler {
	return &Handler{svc: svc, userRepo: userRepo, idGen: idGen}
}

// SetRoleChecker wires the moderator check used by flash/delete (#1548).
func (h *Handler) SetRoleChecker(r RoleChecker) { h.roles = r }

// SetModLog wires the moderation log writer used when a moderator deletes
// another user's flash (#1548).
func (h *Handler) SetModLog(m ModLogger) { h.modLog = m }

// CreateRequest is the request body for flash/create. summary / permissions は
// upstream paramDef で required なので pointer で受けて「キー欠如」を検出する
// (#1548)。空文字 / 空配列は present 扱い (= upstream の required は presence 検査)。
type CreateRequest struct {
	Title       string    `json:"title"`
	Summary     *string   `json:"summary"`
	Script      string    `json:"script"`
	Permissions *[]string `json:"permissions"`
	Visibility  string    `json:"visibility"`
}

// Create handles POST /api/flash/create.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req CreateRequest
	// upstream paramDef required: ['title','summary','script','permissions']。
	// summary/permissions の欠如は 400 にする (title/script は従来どおり空も弾く)。
	if err := c.Bind(&req); err != nil || req.Title == "" || req.Script == "" || req.Summary == nil || req.Permissions == nil {
		return apierr.JSONInvalidParam(c)
	}
	// upstream flash/create.ts は visibility:{enum:['public','private']}。空は public
	// default (service 側) なので許容、それ以外の非 enum 値は 400 で弾く (#2027)。
	if req.Visibility != "" && req.Visibility != "public" && req.Visibility != "private" {
		return apierr.JSONInvalidParam(c)
	}
	f, err := h.svc.Create(coreflash.CreateInput{
		OwnerID:     user.ID,
		Title:       req.Title,
		Summary:     *req.Summary,
		Script:      req.Script,
		Permissions: *req.Permissions,
		Visibility:  req.Visibility,
	})
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.flashWithKnownUser(f, user))
}

// ShowRequest is the request body for flash/show.
type ShowRequest struct {
	FlashID string `json:"flashId"`
}

// Show handles POST /api/flash/show.
//
// frontend MkPlay.vue は user / createdAt / isLiked を読んでカード描画
// および「いいね」ボタン状態を決めるので、handler 側で embed する。
func (h *Handler) Show(c echo.Context) error {
	user := middleware.GetUser(c)
	var req ShowRequest
	if err := c.Bind(&req); err != nil || req.FlashID == "" {
		return apierr.JSONInvalidParam(c)
	}
	requesterID := ""
	if user != nil {
		requesterID = user.ID
	}
	f, err := h.svc.Show(requesterID, req.FlashID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FLASH", "No such flash.", "f0d34a1a-d29a-401d-90ba-1982122b5630"))
	}
	resp := h.flashesToListWithUser([]*model.Flash{f})[0]
	if user != nil {
		liked, err := h.svc.IsLikedBy(user.ID, f.ID)
		if err == nil {
			resp["isLiked"] = liked
		}
	}
	return c.JSON(http.StatusOK, resp)
}

// UpdateRequest is the request body for flash/update.
type UpdateRequest struct {
	FlashID     string    `json:"flashId"`
	Title       *string   `json:"title"`
	Summary     *string   `json:"summary"`
	Script      *string   `json:"script"`
	Permissions *[]string `json:"permissions"`
	Visibility  *string   `json:"visibility"`
}

// Update handles POST /api/flash/update.
func (h *Handler) Update(c echo.Context) error {
	user := middleware.GetUser(c)
	var req UpdateRequest
	if err := c.Bind(&req); err != nil || req.FlashID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream flash/update.ts も visibility:{enum:['public','private']} (#2027)。
	if req.Visibility != nil && *req.Visibility != "public" && *req.Visibility != "private" {
		return apierr.JSONInvalidParam(c)
	}
	f, err := h.svc.Update(user.ID, req.FlashID, coreflash.UpdateInput{
		Title:       req.Title,
		Summary:     req.Summary,
		Script:      req.Script,
		Permissions: req.Permissions,
		Visibility:  req.Visibility,
	})
	if err != nil {
		switch {
		case errors.Is(err, coreflash.ErrFlashNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FLASH", "No such flash.", "611e13d2-309e-419a-a5e4-e0422da39b02"))
		case errors.Is(err, coreflash.ErrAccessDenied):
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "08e60c88-5948-478e-a132-02ec701d67b2"))
		case errors.Is(err, coreflash.ErrFlashTitleRequired):
			return apierr.JSONInvalidParam(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.flashWithKnownUser(f, user))
}

// DeleteRequest is the request body for flash/delete.
type DeleteRequest struct {
	FlashID string `json:"flashId"`
}

// Delete handles POST /api/flash/delete.
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req DeleteRequest
	if err := c.Bind(&req); err != nil || req.FlashID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream delete.ts: 所有者でもモデレータでもなければ ACCESS_DENIED。
	// モデレータは他人の flash も削除でき、その場合 moderationLog を残す (#1548)。
	//
	// **可視性ゲートを通さない lookup を使う。** `Show` は非公開のものを
	// 所有者以外に not-found で返すので、ここを通すと**所有者にもモデレーターにも
	// 非公開の Flash が消せなくなる**。認可はこの直下の owner / moderator 判定が
	// 担っている。
	f, err := h.svc.ShowAny(req.FlashID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FLASH", "No such flash.", "de1623ef-bbb3-4289-a71e-14cfa83d9740"))
	}
	isOwner := f.UserID == user.ID
	isModerator := h.roles != nil && h.roles.IsModerator(user.ID)
	if !isOwner && !isModerator {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1036ad7b-9f92-4fff-89c3-0e50dc941704"))
	}
	if err := h.svc.DeleteByID(req.FlashID); err != nil {
		if errors.Is(err, coreflash.ErrFlashNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FLASH", "No such flash.", "de1623ef-bbb3-4289-a71e-14cfa83d9740"))
		}
		return apierr.JSONInternalError(c)
	}
	if !isOwner && h.modLog != nil {
		h.modLog.Log(c.Request().Context(), user.ID, moderationlog.LogDeleteFlash, map[string]any{
			"flashId":           f.ID,
			"flashUserId":       f.UserID,
			"flashUserUsername": h.usernameOf(f.UserID),
			"flash":             flashToMap(f),
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// usernameOf returns the username for userID, or "" when the lookup fails.
// Used to populate the moderation log info (#1548).
func (h *Handler) usernameOf(userID string) string {
	if h.userRepo == nil {
		return ""
	}
	u, err := h.userRepo.FindByID(userID)
	if err != nil || u == nil {
		return ""
	}
	return u.Username
}

// PaginationRequest is the shared body for list-style endpoints.
type PaginationRequest struct {
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// SearchRequest is the request body for flash/search.
type SearchRequest struct {
	Query     string `json:"query"`
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// My handles POST /api/i/flashs and POST /api/flash/my (own list).
//
// frontend Paginator (cursor mode) は untilId / sinceId を forward する (#493)。
func (h *Handler) My(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PaginationRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.svc.My(user.ID, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// upstream flash/my.ts は packMany(flashs) を me 無しで呼ぶため isLiked を
	// omit する (featured/search/my-likes は me 付きで isLiked を出すのと対照的)。
	// mk-go の /api/i/flashs も同 handler を共有するが、upstream に i/flashs は無く
	// flash/my と同 shape に揃えるため viewer 無しで pack する (#1773)。
	return c.JSON(http.StatusOK, h.flashesToListWithUser(rows))
}

// Featured handles POST /api/flash/featured.
func (h *Handler) Featured(c echo.Context) error {
	var req PaginationRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.svc.Featured(sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.flashesToListForViewer(rows, middleware.GetUser(c)))
}

// Search handles POST /api/flash/search.
func (h *Handler) Search(c echo.Context) error {
	var req SearchRequest
	if err := c.Bind(&req); err != nil || req.Query == "" {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 5, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.svc.Search(req.Query, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.flashesToListForViewer(rows, middleware.GetUser(c)))
}

// LikeRequest is the request body for flash/like and flash/unlike.
type LikeRequest struct {
	FlashID string `json:"flashId"`
}

// Like handles POST /api/flash/like.
func (h *Handler) Like(c echo.Context) error {
	user := middleware.GetUser(c)
	var req LikeRequest
	if err := c.Bind(&req); err != nil || req.FlashID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Like(user.ID, req.FlashID); err != nil {
		switch {
		case errors.Is(err, coreflash.ErrFlashNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FLASH", "No such flash.", "c07c1491-9161-4c5c-9d75-01906f911f73"))
		case errors.Is(err, coreflash.ErrYourFlash):
			return c.JSON(http.StatusBadRequest, apierr.Error("YOUR_FLASH", "You cannot like your flash.", "3fd8a0e7-5955-4ba9-85bb-bf3e0c30e13b"))
		case errors.Is(err, coreflash.ErrAlreadyLiked):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_LIKED", "You already liked that flash.", "010065cf-ad43-40df-8067-abff9f4686e3"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// Unlike handles POST /api/flash/unlike.
func (h *Handler) Unlike(c echo.Context) error {
	user := middleware.GetUser(c)
	var req LikeRequest
	if err := c.Bind(&req); err != nil || req.FlashID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Unlike(user.ID, req.FlashID); err != nil {
		switch {
		case errors.Is(err, coreflash.ErrFlashNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FLASH", "No such flash.", "afe8424a-a69e-432d-a5f2-2f0740c62410"))
		case errors.Is(err, coreflash.ErrNotLiked):
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_LIKED", "You have not liked that flash.", "755f25a7-9871-4f65-9f34-51eaad9ae0ac"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// MyLikes handles POST /api/flash/my-likes.
//
// upstream Misskey TS は `{id, flash: Flash}` を返す (id = flash_like
// row id, frontend は it.flash で MkPlayPreview を描画)。mk-go は元々
// flatten された Flash 配列を返しており frontend が空表示になっていた。
func (h *Handler) MyLikes(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		PaginationRequest
		// upstream my-likes paramDef: search (string, 1-100, nullable)。
		Search string `json:"search"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	pairs, err := h.svc.MyLikes(user.ID, req.Search, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	flashes := make([]*model.Flash, 0, len(pairs))
	for _, p := range pairs {
		flashes = append(flashes, p.Flash)
	}
	packed := h.flashesToListForViewer(flashes, user)
	out := make([]map[string]any, 0, len(pairs))
	for i, p := range pairs {
		out = append(out, map[string]any{
			"id":    p.LikeID,
			"flash": packed[i],
		})
	}
	return c.JSON(http.StatusOK, out)
}

func flashToMap(f *model.Flash) map[string]any {
	const tsFormat = "2006-01-02T15:04:05.000Z"
	// upstream FlashEntityService.pack は id/createdAt/updatedAt/userId/user/title/
	// summary/script/visibility/likedCount/isLiked のみを返し permissions を含めない
	// (flash.ts json-schema にも permissions プロパティは無い)。create/update は
	// permissions を受理・保存するが応答には出さないため、mk-go も応答から除外して
	// shape を一致させる (#1773)。
	return map[string]any{
		"id":         f.ID,
		"updatedAt":  f.UpdatedAt.UTC().Format(tsFormat),
		"title":      f.Title,
		"summary":    f.Summary,
		"userId":     f.UserID,
		"script":     f.Script,
		"likedCount": f.LikedCount,
		"visibility": f.Visibility,
	}
}

// flashWithKnownUser packs a single flash whose author is already in scope
// (typically the authenticated user from middleware on Create / Update).
// Saves one FindManyByIDs round-trip vs flashesToListWithUser.
func (h *Handler) flashWithKnownUser(f *model.Flash, owner *model.User) map[string]any {
	const tsFormat = "2006-01-02T15:04:05.000Z"
	entry := flashToMap(f)
	createdAt := ""
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(f.ID); err == nil {
			createdAt = t.UTC().Format(tsFormat)
		}
	}
	// flashesToListWithUser と同じく nil/error 時も "" でフィールドを必ず
	// 残す。frontend が `if (item.createdAt)` する場合のため (#520 review)。
	entry["createdAt"] = createdAt
	if owner != nil && owner.ID == f.UserID {
		entry["user"] = entity.PackUserLite(owner)
	}
	return entry
}

// flashesToListWithUser packs flashes for an anonymous viewer (no isLiked).
func (h *Handler) flashesToListWithUser(rows []*model.Flash) []map[string]any {
	return h.flashesToListForViewer(rows, nil)
}

// flashesToListForViewer packs flashes including the embedded `user`
// (UserLite), ISO-formatted `createdAt`, and (when viewer != nil) the per-flash
// `isLiked` flag, matching upstream Misskey TS FlashEntityService output. The
// frontend Play page reads `flash.user` for display so a missing user object
// causes empty card render; the like button reads `isLiked` (#1548).
//
// User lookups are deduped by author and fetched in a single FindManyByIDs
// call; liked-flash ids are batch-loaded in one query, both avoiding N+1.
func (h *Handler) flashesToListForViewer(rows []*model.Flash, viewer *model.User) []map[string]any {
	const tsFormat = "2006-01-02T15:04:05.000Z"
	userIDs := make([]string, 0, len(rows))
	flashIDs := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, f := range rows {
		flashIDs = append(flashIDs, f.ID)
		if _, ok := seen[f.UserID]; ok {
			continue
		}
		seen[f.UserID] = struct{}{}
		userIDs = append(userIDs, f.UserID)
	}
	userByID := make(map[string]*model.User, len(userIDs))
	if h.userRepo != nil && len(userIDs) > 0 {
		if users, err := h.userRepo.FindManyByIDs(userIDs); err == nil {
			for _, u := range users {
				userByID[u.ID] = u
			}
		}
	}
	// 認証 viewer のときだけ isLiked を埋める (upstream optional field)。
	var likedSet map[string]bool
	if viewer != nil {
		likedSet, _ = h.svc.LikedFlashIDs(viewer.ID, flashIDs)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, f := range rows {
		entry := flashToMap(f)
		createdAt := ""
		if h.idGen != nil {
			if t, err := h.idGen.ParseTime(f.ID); err == nil {
				createdAt = t.UTC().Format(tsFormat)
			}
		}
		entry["createdAt"] = createdAt
		if u, ok := userByID[f.UserID]; ok {
			entry["user"] = entity.PackUserLite(u)
		}
		if viewer != nil {
			entry["isLiked"] = likedSet[f.ID]
		}
		out = append(out, entry)
	}
	return out
}
