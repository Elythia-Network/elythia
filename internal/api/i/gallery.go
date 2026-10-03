package i

import (
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// paginationFromRequest reads optional limit / offset / sinceId / untilId
// JSON fields, with Misskey-friendly defaults (limit=10, offset=0,
// limit clamped to 100). Cursor 指定時は frontend Paginator が untilId
// / sinceId を投げてくる経路 (#493)。
//
// ok=false は bind の失敗か列に入らないカーソルで、呼び出し側は 400 を返すこと
// (#3025 / #3330)。
func paginationFromRequest(c echo.Context) (limit, offset int, sinceID, untilID string, ok bool) {
	var req struct {
		Limit     *int   `json:"limit"`
		Offset    *int   `json:"offset"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		// 本家は body の型違いや object でない body を ajv で 400 にする (#3330)。
		return 0, 0, "", "", false
	}
	limit = 10
	if req.Limit != nil {
		limit = *req.Limit
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	if req.Offset != nil && *req.Offset > 0 {
		offset = *req.Offset
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	cursorSince, cursorUntil, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return 0, 0, "", "", false
	}
	sinceID, untilID = cursorSince, cursorUntil
	return limit, offset, sinceID, untilID, true
}

// GalleryLikes handles POST /api/i/gallery/likes.
// 自分が like した gallery_post を返却する。
//
// upstream Misskey TS は `{id, post: GalleryPost}` を返し、frontend は
// `item.post` で MkGalleryPostPreview を描画する。mk-go は元々 `postId`
// しか返しておらず frontend が空表示になっていた (#493 関連)。
// 同じ shape に揃えるため like 行ごとに対応する gallery_post を fetch
// して埋める。
func (h *Handler) GalleryLikes(c echo.Context) error {
	if h.galleryRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	limit, offset, sinceID, untilID, cursorOK := paginationFromRequest(c)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	likes, err := h.galleryRepo.ListLikesByUser(u.ID, sinceID, untilID, limit, offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	postIDs := make([]string, 0, len(likes))
	seen := make(map[string]struct{}, len(likes))
	for _, l := range likes {
		if _, ok := seen[l.PostID]; ok {
			continue
		}
		seen[l.PostID] = struct{}{}
		postIDs = append(postIDs, l.PostID)
	}
	posts, err := h.galleryRepo.FindPostsByIDs(postIDs)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	postByID := make(map[string]*model.GalleryPost, len(posts))
	for _, p := range posts {
		postByID[p.ID] = p
	}
	out := make([]map[string]any, 0, len(likes))
	liked := true // この経路の post は viewer が like 済 (= isLiked true)。
	for _, l := range likes {
		// upstream Misskey TS は post が削除済の like row を Promise.all
		// + pack の null フィルタで弾く。frontend の MkGalleryPostPreview
		// も it.post 必須なので、ここで skip して整合させる。
		p, ok := postByID[l.PostID]
		if !ok {
			continue
		}
		out = append(out, map[string]any{
			"id":   l.ID,
			"post": packGalleryPost(p, h.idGen, h.resolveGalleryFiles(p.FileIDs), &liked),
		})
	}
	return c.JSON(http.StatusOK, out)
}

// resolveGalleryFiles resolves a gallery post's fileIds into packed DriveFile
// entities in fileIds order (upstream driveFileEntityService.packManyByIds)。
// driveFileRepo 未配線 / fileIds 空 / lookup miss は空配列で degrade する。
// gallery post / file は公開前提なので owner-scope filter はしない (upstream 同様)。
func (h *Handler) resolveGalleryFiles(fileIDs []string) []any {
	files := []any{}
	if h.driveFileRepo == nil || len(fileIDs) == 0 {
		return files
	}
	found, err := h.driveFileRepo.FindByIDs([]string(fileIDs))
	if err != nil {
		return files
	}
	byID := make(map[string]*model.DriveFile, len(found))
	for _, f := range found {
		byID[f.ID] = f
	}
	for _, fid := range fileIDs {
		if f, ok := byID[fid]; ok {
			files = append(files, entity.PackDriveFile(f, h.idGen))
		}
	}
	return files
}

// packGalleryPost は upstream の GalleryPost shape (createdAt / user 含む)
// に揃えてレスポンス用の map を返す。files は呼び出し側が resolveGalleryFiles
// で解決した DriveFile entity 群、isLiked は viewer の like 状態 (nil で省略、
// upstream は me==null 時に undefined)。
func packGalleryPost(p *model.GalleryPost, idGen id.Generator, files []any, isLiked *bool) map[string]any {
	const tsFormat = "2006-01-02T15:04:05.000Z"
	createdAt := ""
	if idGen != nil {
		if t, err := idGen.ParseTime(p.ID); err == nil {
			createdAt = t.UTC().Format(tsFormat)
		}
	}
	// fileIds / tags は golden GalleryPost で string[] (non-null)。nil の
	// model.StringArray は JSON null になるため [] へ coalesce する (#1322)。
	fileIDs := []string(p.FileIDs)
	if fileIDs == nil {
		fileIDs = []string{}
	}
	tags := []string(p.Tags)
	if tags == nil {
		tags = []string{}
	}
	if files == nil {
		files = []any{}
	}
	resp := map[string]any{
		"id":          p.ID,
		"createdAt":   createdAt,
		"updatedAt":   p.UpdatedAt.UTC().Format(tsFormat),
		"title":       p.Title,
		"description": p.Description,
		"userId":      p.UserID,
		"fileIds":     fileIDs,
		"files":       files,
		"isSensitive": p.IsSensitive,
		"likedCount":  p.LikedCount,
		"tags":        tags,
	}
	if isLiked != nil {
		resp["isLiked"] = *isLiked
	}
	if p.User != nil {
		resp["user"] = entity.PackUserLite(p.User)
	}
	return resp
}

// GalleryPosts handles POST /api/i/gallery/posts.
// 自分が投稿した gallery_post を返却する。
func (h *Handler) GalleryPosts(c echo.Context) error {
	if h.galleryRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	u := middleware.GetUser(c)
	limit, offset, sinceID, untilID, cursorOK := paginationFromRequest(c)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	posts, err := h.galleryRepo.ListByUser(u.ID, sinceID, untilID, limit, offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(posts))
	for _, p := range posts {
		// golden GalleryPost は user (UserLite) を必須とする。i/gallery/posts は
		// 認証 user 自身の投稿一覧なので、relation 未 preload の場合は作成者
		// (= 認証 user) を attach してから pack する (#1322)。
		if p.User == nil {
			p.User = u
		}
		// isLiked は viewer が自分の post を like しているか (upstream
		// galleryLikesRepository.exists)。list は小さい前提で per-post lookup。
		liked := false
		if ok, err := h.galleryRepo.ExistsLike(u.ID, p.ID); err == nil {
			liked = ok
		}
		out = append(out, packGalleryPost(p, h.idGen, h.resolveGalleryFiles(p.FileIDs), &liked))
	}
	return c.JSON(http.StatusOK, out)
}
