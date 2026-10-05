package gallery

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/core/moderationlog"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

// galleryPostsRankingWindow mirrors upstream GALLERY_POSTS_RANKING_WINDOW
// (= 3日)。post 作成からこの期間内の like/unlike だけが featured ランキングへ
// 反映される (#1548)。
const galleryPostsRankingWindow = 3 * 24 * time.Hour

// GalleryRanking is the subset of core/featured.Service used by gallery like /
// unlike / featured (#1548). nil 配線時は ranking 更新を skip し、featured は
// 空配列に degrade する。
type GalleryRanking interface {
	UpdateGalleryPostsRanking(ctx context.Context, postID string, score float64) error
	GetGalleryPostsRanking(ctx context.Context, threshold int) ([]string, error)
}

// RoleChecker reports whether a user is a moderator. Satisfied by
// *core/role.Service. nil 配線時は全員 non-moderator 扱い。
type RoleChecker interface {
	IsModerator(userID string) bool
}

// ModLogger records a moderation action. Satisfied by *core/moderationlog.Service.
// nil 配線時は記録を skip する。
type ModLogger interface {
	Log(ctx context.Context, moderatorID string, t moderationlog.LogType, info map[string]any)
}

// Handler handles gallery-related API endpoints.
type Handler struct {
	db      *gorm.DB
	idGen   id.Generator
	ranking GalleryRanking
	roles   RoleChecker
	modLog  ModLogger
}

// NewHandler creates a new gallery Handler.
func NewHandler(db *gorm.DB, idGen id.Generator) *Handler {
	return &Handler{db: db, idGen: idGen}
}

// SetRanking wires the featured gallery-posts ranking used by like / unlike /
// featured (#1548). nil disables ranking updates and degrades featured to [].
func (h *Handler) SetRanking(r GalleryRanking) { h.ranking = r }

// SetRoleChecker wires the moderator check used by posts/delete (#1548).
func (h *Handler) SetRoleChecker(r RoleChecker) { h.roles = r }

// SetModLog wires the moderation log writer used when a moderator deletes
// another user's gallery post (#1548).
func (h *Handler) SetModLog(m ModLogger) { h.modLog = m }

// Featured handles POST /api/gallery/featured.
//
// upstream featured.ts は featuredService.getGalleryPostsRanking(100) (Redis
// 時間窓ランキング) を読み、id DESC sort → untilId filter → limit slice →
// id IN で取得して packMany する (#1548)。mk-go は upstream の 30 分メモリ
// キャッシュは持たず毎回 Redis を読む (= 常に最新、API shape は同一)。ranking
// 未配線時は空配列に degrade する。
func (h *Handler) Featured(c echo.Context) error {
	var req struct {
		Limit   *int   `json:"limit"`
		UntilID string `json:"untilId"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// 他のページングと同じく `id.NormalizeCursor` を通す (#3025)。ここは
	// in-memory の比較にしか使わないので 500 にはならないが、**upstream の
	// paramDef も `misskey:id` (`^[a-zA-Z0-9]+$`) で弾く**ので 400 に揃える。
	_, untilID, cursorOK := id.NormalizeCursor("", req.UntilID, nil, nil)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	if h.ranking == nil {
		return c.JSON(http.StatusOK, []map[string]any{})
	}
	ids, err := h.ranking.GetGalleryPostsRanking(c.Request().Context(), 100)
	if err != nil || len(ids) == 0 {
		return c.JSON(http.StatusOK, []map[string]any{})
	}
	// upstream は postIds を id DESC に sort してから untilId/limit を適用する。
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	if untilID != "" {
		filtered := ids[:0:0]
		for _, pid := range ids {
			if pid < untilID {
				filtered = append(filtered, pid)
			}
		}
		ids = filtered
	}
	if len(ids) > limit {
		ids = ids[:limit]
	}
	if len(ids) == 0 {
		return c.JSON(http.StatusOK, []map[string]any{})
	}
	var posts []*model.GalleryPost
	if err := h.db.Preload("User").Where("id IN ?", ids).Find(&posts).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packMany(posts, middleware.GetUser(c)))
}

// Popular handles POST /api/gallery/popular.
//
// upstream popular.ts は likedCount>0 の post を likedCount DESC で 10 件返す
// (#1548)。featured (Redis ランキング) とは別ロジック。
func (h *Handler) Popular(c echo.Context) error {
	var posts []*model.GalleryPost
	if err := h.db.Preload("User").Where("\"likedCount\" > 0").Order("\"likedCount\" DESC").Limit(10).Find(&posts).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packMany(posts, middleware.GetUser(c)))
}

// Posts handles POST /api/gallery/posts.
//
// frontend Paginator (cursor mode) は untilId / sinceId を投げてくる。
// id 範囲フィルタ + paginationOrder 同等の ASC/DESC を直接組み立てる。
func (h *Handler) Posts(c echo.Context) error {
	var req struct {
		Limit     *int   `json:"limit"`
		Offset    int    `json:"offset"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	q := h.db.Preload("User")
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" && untilID == "" {
		q = q.Order("id ASC")
	} else {
		q = q.Order("id DESC")
	}
	q = q.Limit(limit)
	if sinceID == "" && untilID == "" && req.Offset > 0 {
		q = q.Offset(req.Offset)
	}
	var posts []*model.GalleryPost
	if err := q.Find(&posts).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packMany(posts, middleware.GetUser(c)))
}

// PostsCreate handles POST /api/gallery/posts/create.
func (h *Handler) PostsCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Title       string   `json:"title"`
		Description *string  `json:"description"`
		FileIDs     []string `json:"fileIds"`
		IsSensitive bool     `json:"isSensitive"`
	}
	if err := c.Bind(&req); err != nil || req.Title == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "title is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream paramDef: fileIds required, minItems:1。所有する drive file のみ
	// を残し、1 件も残らなければ reject (upstream は filter→0 で throw)。
	if len(req.FileIDs) == 0 || len(req.FileIDs) > galleryMaxFiles {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "fileIds must contain 1-32 items.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	owned, err := h.ownedFileIDs(req.FileIDs, user.ID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// #2106 L26: upstream gallery/posts/create は files 全件 not-owned のとき bare Error を投げ
	// INTERNAL_ERROR(500) になるが、mk-go は semantics 上適切な 400 INVALID_PARAM を意図的に
	// 維持する (worse な 500 拡散を避ける、#2106 方針)。
	if len(owned) == 0 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "No valid files.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	now := time.Now()
	post := &model.GalleryPost{
		ID:          h.idGen.Generate(now),
		UpdatedAt:   now,
		Title:       req.Title,
		Description: req.Description,
		UserID:      user.ID,
		FileIDs:     model.StringArray(owned),
		IsSensitive: req.IsSensitive,
		Tags:        []string{},
	}
	if err := h.db.Create(post).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	post.User = user
	return c.JSON(http.StatusOK, h.packOne(post, user))
}

// galleryMaxFiles mirrors upstream paramDef fileIds.maxItems.
const galleryMaxFiles = 32

// ownedFileIDs returns the subset of fileIDs that belong to userID, preserving
// input order and deduplicating (upstream fileIds.uniqueItems). A DB error is
// propagated (not swallowed) so the handler returns 500 rather than masking it
// as a "no valid files" 400.
func (h *Handler) ownedFileIDs(fileIDs []string, userID string) ([]string, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}
	var rows []*model.DriveFile
	if err := h.db.Select("id").Where("id IN ? AND \"userId\" = ?", fileIDs, userID).Find(&rows).Error; err != nil {
		return nil, err
	}
	ownedSet := make(map[string]bool, len(rows))
	for _, f := range rows {
		ownedSet[f.ID] = true
	}
	seen := make(map[string]bool, len(fileIDs))
	out := make([]string, 0, len(fileIDs))
	for _, fid := range fileIDs {
		if ownedSet[fid] && !seen[fid] {
			seen[fid] = true
			out = append(out, fid)
		}
	}
	return out, nil
}

// PostsShow handles POST /api/gallery/posts/show.
func (h *Handler) PostsShow(c echo.Context) error {
	var req struct {
		PostID string `json:"postId"`
	}
	if err := c.Bind(&req); err != nil || req.PostID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "postId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	var post model.GalleryPost
	if err := h.db.Preload("User").Where("id = ?", req.PostID).First(&post).Error; err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_POST", "No such post.", "1137bf14-c5b0-4604-85bb-5b5371b1cd45"))
	}
	return c.JSON(http.StatusOK, h.packOne(&post, middleware.GetUser(c)))
}

// PostsDelete handles POST /api/gallery/posts/delete.
func (h *Handler) PostsDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		PostID string `json:"postId"`
	}
	if err := c.Bind(&req); err != nil || req.PostID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "postId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	var post model.GalleryPost
	if err := h.db.Where("id = ?", req.PostID).First(&post).Error; err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_POST", "No such post.", "ae52f367-4bd7-4ecd-afc6-5672fff427f5"))
	}
	// upstream delete.ts: 所有者でもモデレータでもなければ ACCESS_DENIED。
	// モデレータは他人の post も削除でき、その場合 moderationLog を残す (#1548)。
	isOwner := post.UserID == user.ID
	isModerator := h.roles != nil && h.roles.IsModerator(user.ID)
	if !isOwner && !isModerator {
		return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "c86e09de-1c48-43ac-a435-1c7e42ed4496"))
	}
	if err := h.db.Where("id = ?", post.ID).Delete(&model.GalleryPost{}).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// 他人の post をモデレータ権限で削除した場合のみ監査ログを残す。
	if !isOwner && h.modLog != nil {
		h.modLog.Log(c.Request().Context(), user.ID, moderationlog.LogDeleteGalleryPost, map[string]any{
			"postId":           post.ID,
			"postUserId":       post.UserID,
			"postUserUsername": h.usernameOf(post.UserID),
			"post":             h.buildPostMap(&post, h.resolveFiles(post.FileIDs), nil),
		})
	}
	return c.NoContent(http.StatusNoContent)
}

// usernameOf returns the username for userID, or "" when the lookup fails.
// Used to populate the moderation log info (#1548).
func (h *Handler) usernameOf(userID string) string {
	var u model.User
	if err := h.db.Select("username").Where("id = ?", userID).First(&u).Error; err != nil {
		return ""
	}
	return u.Username
}

// PostsUpdate handles POST /api/gallery/posts/update.
func (h *Handler) PostsUpdate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		PostID      string    `json:"postId"`
		Title       *string   `json:"title"`
		Description *string   `json:"description"`
		FileIDs     *[]string `json:"fileIds"`
		// upstream paramDef は isSensitive: default false。省略時も false で
		// 上書きされる (TypeORM は undefined だけ skip)。bool で受け常に書く。
		IsSensitive bool `json:"isSensitive"`
	}
	if err := c.Bind(&req); err != nil || req.PostID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "postId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// title は minLength:1 (空文字は 400)。
	if req.Title != nil && *req.Title == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "title must not be empty.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream は updatedAt と isSensitive(default false) を必ず更新する。fields が
	// 空にならないので RowsAffected==0 (= 行不在) を NO_SUCH_POST 判定に使える。
	fields := map[string]any{"updatedAt": time.Now(), "isSensitive": req.IsSensitive}
	if req.Title != nil {
		fields["title"] = *req.Title
	}
	if req.Description != nil {
		fields["description"] = *req.Description
	}
	if req.FileIDs != nil {
		if len(*req.FileIDs) == 0 || len(*req.FileIDs) > galleryMaxFiles {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "fileIds must contain 1-32 items.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		// fileIds 指定時は所有 file のみに絞り、1 件も無ければ reject。
		owned, err := h.ownedFileIDs(*req.FileIDs, user.ID)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		if len(owned) == 0 {
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "No valid files.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
		}
		fields["fileIds"] = model.StringArray(owned)
	}
	result := h.db.Model(&model.GalleryPost{}).Where("id = ? AND \"userId\" = ?", req.PostID, user.ID).Updates(fields)
	if result.RowsAffected == 0 {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_POST", "No such post.", "1137bf14-c27b-46e0-86d3-0cbbf8b0aca5"))
	}
	// upstream は更新後の GalleryPost を返す (204 ではない)。
	var post model.GalleryPost
	if err := h.db.Preload("User").Where("id = ?", req.PostID).First(&post).Error; err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_POST", "No such post.", "1137bf14-c27b-46e0-86d3-0cbbf8b0aca5"))
	}
	return c.JSON(http.StatusOK, h.packOne(&post, user))
}

// PostsLike handles POST /api/gallery/posts/like.
func (h *Handler) PostsLike(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		PostID string `json:"postId"`
	}
	if err := c.Bind(&req); err != nil || req.PostID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "postId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream like.ts: post 不在 → NO_SUCH_POST、自分の post → YOUR_POST、
	// 既 like → ALREADY_LIKED の順で判定する (#1548)。
	var post model.GalleryPost
	if err := h.db.Where("id = ?", req.PostID).First(&post).Error; err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_POST", "No such post.", "56c06af3-1287-442f-9701-c93f7c4a62ff"))
	}
	if post.UserID == user.ID {
		return c.JSON(http.StatusBadRequest, apierr.Error("YOUR_POST", "You cannot like your post.", "f78f1511-5ebc-4478-a888-1198d752da68"))
	}
	// 既 like は ALREADY_LIKED。upstream は insert 前に exists チェックするので、
	// insert 自体の失敗 (= 想定外の DB error) は ALREADY_LIKED でなく 500 に倒す。
	var existing int64
	h.db.Model(&model.GalleryLike{}).Where("\"userId\" = ? AND \"postId\" = ?", user.ID, req.PostID).Count(&existing)
	if existing > 0 {
		// upstream gallery/posts/like.ts の alreadyLiked は httpStatusCode 未指定で、
		// ApiError は kind:'client' 既定の 400 を返す。flash/like も同 ALREADY_LIKED を
		// 400 で返しており、409 だと upstream とも mk-go 内とも不一致になる (#1773)。
		return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_LIKED", "Already liked.", "40e9ed56-a59c-473a-bf3f-f289c54fb5a7"))
	}
	like := &model.GalleryLike{
		ID:     h.idGen.Generate(time.Now()),
		UserID: user.ID,
		PostID: req.PostID,
	}
	if err := h.db.Create(like).Error; err != nil {
		// exists チェックを通った後の insert 失敗は競合 like の unique 違反含め
		// 想定外。ALREADY_LIKED に丸めず INTERNAL_ERROR を返す。
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// ランキング更新は post 作成から RANKING_WINDOW 内のみ (#1548)。
	if h.ranking != nil {
		if t, err := h.idGen.ParseTime(post.ID); err == nil && time.Since(t) < galleryPostsRankingWindow {
			_ = h.ranking.UpdateGalleryPostsRanking(c.Request().Context(), post.ID, 1)
		}
	}
	h.db.Model(&model.GalleryPost{}).Where("id = ?", req.PostID).UpdateColumn("\"likedCount\"", gorm.Expr("\"likedCount\" + 1"))
	return c.NoContent(http.StatusNoContent)
}

// PostsUnlike handles POST /api/gallery/posts/unlike.
func (h *Handler) PostsUnlike(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		PostID string `json:"postId"`
	}
	if err := c.Bind(&req); err != nil || req.PostID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "postId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream unlike.ts: post 不在 → NO_SUCH_POST、未 like → NOT_LIKED (#1548)。
	var post model.GalleryPost
	if err := h.db.Where("id = ?", req.PostID).First(&post).Error; err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_POST", "No such post.", "c32e6dd0-b555-4413-925e-b3757d19ed84"))
	}
	result := h.db.Where("\"userId\" = ? AND \"postId\" = ?", user.ID, req.PostID).Delete(&model.GalleryLike{})
	if result.RowsAffected == 0 {
		return c.JSON(http.StatusBadRequest, apierr.Error("NOT_LIKED", "You have not liked that post.", "e3e8e06e-be37-41f7-a5b4-87a8250288f0"))
	}
	// ランキング更新は post 作成から RANKING_WINDOW 内のみ (#1548)。
	if h.ranking != nil {
		if t, err := h.idGen.ParseTime(post.ID); err == nil && time.Since(t) < galleryPostsRankingWindow {
			_ = h.ranking.UpdateGalleryPostsRanking(c.Request().Context(), post.ID, -1)
		}
	}
	h.db.Model(&model.GalleryPost{}).Where("id = ?", req.PostID).UpdateColumn("\"likedCount\"", gorm.Expr("GREATEST(\"likedCount\" - 1, 0)"))
	return c.NoContent(http.StatusNoContent)
}

// resolveFiles fetches the DriveFiles for fileIDs and packs them in the same
// order as fileIDs (upstream GalleryPost.files is the resolved DriveFile[]).
// Missing files are skipped. Returns a non-nil slice so JSON is [] not null.
func (h *Handler) resolveFiles(fileIDs []string) []entity.DriveFileEntity {
	out := make([]entity.DriveFileEntity, 0, len(fileIDs))
	if len(fileIDs) == 0 {
		return out
	}
	var rows []*model.DriveFile
	if err := h.db.Where("id IN ?", fileIDs).Find(&rows).Error; err != nil {
		return out
	}
	byID := make(map[string]*model.DriveFile, len(rows))
	for _, f := range rows {
		byID[f.ID] = f
	}
	for _, fid := range fileIDs {
		if f := byID[fid]; f != nil {
			out = append(out, entity.PackDriveFile(f, h.idGen))
		}
	}
	return out
}

// likedPostIDs returns the set of postIDs (from the given list) that viewerID
// has liked, in one query. Empty when viewer is nil.
func (h *Handler) likedPostIDs(viewerID string, postIDs []string) map[string]bool {
	set := map[string]bool{}
	if viewerID == "" || len(postIDs) == 0 {
		return set
	}
	var liked []string
	h.db.Model(&model.GalleryLike{}).
		Where("\"userId\" = ? AND \"postId\" IN ?", viewerID, postIDs).
		Pluck("\"postId\"", &liked)
	for _, id := range liked {
		set[id] = true
	}
	return set
}

func (h *Handler) packMany(posts []*model.GalleryPost, viewer *model.User) []map[string]any {
	// batch file resolution (N+1 回避): 全 post の fileIds をまとめて 1 query。
	idSet := map[string]struct{}{}
	postIDs := make([]string, 0, len(posts))
	for _, p := range posts {
		postIDs = append(postIDs, p.ID)
		for _, fid := range p.FileIDs {
			idSet[fid] = struct{}{}
		}
	}
	allFileIDs := make([]string, 0, len(idSet))
	for fid := range idSet {
		allFileIDs = append(allFileIDs, fid)
	}
	fileByID := map[string]entity.DriveFileEntity{}
	if len(allFileIDs) > 0 {
		var rows []*model.DriveFile
		if err := h.db.Where("id IN ?", allFileIDs).Find(&rows).Error; err == nil {
			for _, f := range rows {
				fileByID[f.ID] = entity.PackDriveFile(f, h.idGen)
			}
		}
	}
	var likedSet map[string]bool
	if viewer != nil {
		likedSet = h.likedPostIDs(viewer.ID, postIDs)
	}

	result := make([]map[string]any, 0, len(posts))
	for _, p := range posts {
		files := make([]entity.DriveFileEntity, 0, len(p.FileIDs))
		for _, fid := range p.FileIDs {
			if f, ok := fileByID[fid]; ok {
				files = append(files, f)
			}
		}
		var isLiked *bool
		if viewer != nil {
			v := likedSet[p.ID]
			isLiked = &v
		}
		result = append(result, h.buildPostMap(p, files, isLiked))
	}
	return result
}

// packOne packs a single post, resolving its files + the viewer's like state.
// viewer may be nil (isLiked is then omitted, matching upstream optional field).
func (h *Handler) packOne(p *model.GalleryPost, viewer *model.User) map[string]any {
	files := h.resolveFiles(p.FileIDs)
	var isLiked *bool
	if viewer != nil {
		liked := h.likedPostIDs(viewer.ID, []string{p.ID})
		v := liked[p.ID]
		isLiked = &v
	}
	return h.buildPostMap(p, files, isLiked)
}

// buildPostMap assembles the GalleryPost response shape from precomputed files
// and isLiked. isLiked nil omits the field (upstream `isLiked` is optional).
func (h *Handler) buildPostMap(p *model.GalleryPost, files []entity.DriveFileEntity, isLiked *bool) map[string]any {
	createdAt := ""
	if t, err := h.idGen.ParseTime(p.ID); err == nil {
		createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	fileIDs := []string(p.FileIDs)
	if fileIDs == nil {
		fileIDs = []string{}
	}
	resp := map[string]any{
		"id":          p.ID,
		"createdAt":   createdAt,
		"updatedAt":   p.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"title":       p.Title,
		"description": p.Description,
		"userId":      p.UserID,
		"fileIds":     fileIDs,
		"files":       files,
		"isSensitive": p.IsSensitive,
		"likedCount":  p.LikedCount,
	}
	// upstream GalleryPostEntityService は tags: post.tags.length > 0 ? post.tags :
	// undefined で、空のとき field 自体を omit する。mk-go も空配列を出さず omit して
	// shape を一致させる (gallery-post.ts schema 上 tags は optional、#1773)。
	if tags := []string(p.Tags); len(tags) > 0 {
		resp["tags"] = tags
	}
	if p.User != nil {
		resp["user"] = entity.PackUserLite(p.User)
	}
	if isLiked != nil {
		resp["isLiked"] = *isLiked
	}
	return resp
}
