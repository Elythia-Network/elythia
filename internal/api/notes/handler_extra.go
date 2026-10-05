package notes

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	coreachievement "github.com/elythia-network/elythia/internal/core/achievement"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/core/ugcvisibility"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// SetFavoriteRepo attaches a NoteFavoriteRepository for favorites endpoints.
func (h *Handler) SetFavoriteRepo(r repository.NoteFavoriteRepository) {
	h.favoriteRepo = r
}

// FavoritesCreate handles POST /api/notes/favorites/create.
func (h *Handler) FavoritesCreate(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		NoteID string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// 旧実装は noteRepo.FindByID で存在確認のみ行い visibility check が抜けて
	// いた (#1443)。note ID を既に知っている viewer が followers / specified
	// visibility の note を favorite 化でき、その後 /api/i/favorites 経由で
	// content / author を pull 可能 (favorite 一覧側は upstream 互換のため
	// 素の PackNotes で返す設計 — handler.go:633-636)。author が visibility
	// を絞った後も古い favorite 行が残るため #799「ID 既知公開」doctrine は
	// 適用不可。queryService.RequireVisible で見えない note は存在隠蔽する。
	// queryService 未配線時は fail-closed (ShowPartialBulk と同じ pattern)。
	if h.queryService == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "6dd26674-e060-4816-909a-45ba3f4da458"))
	}
	target, err := h.queryService.RequireVisible(user, req.NoteID)
	if h.materializeIfMissing(req.NoteID, err) {
		target, err = h.queryService.RequireVisible(user, req.NoteID)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "6dd26674-e060-4816-909a-45ba3f4da458"))
	}
	if h.favoriteRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	exists, _ := h.favoriteRepo.Exists(user.ID, req.NoteID)
	if exists {
		return c.JSON(http.StatusConflict, apierr.Error("ALREADY_FAVORITED", "Already favorited.", "a402c12b-34dd-41d2-97d8-4d2ffd96a1a6"))
	}
	now := time.Now()
	fav := &model.NoteFavorite{
		ID:        h.idGen.Generate(now),
		UserID:    user.ID,
		NoteID:    req.NoteID,
		CreatedAt: now,
	}
	if err := h.favoriteRepo.Create(fav); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream favorites/create.ts: local note を他人が favorite したとき著者に
	// myNoteFavorited1 を付与する (#1762)。best-effort で favorite 自体 (204) は
	// 巻き戻さない。granter 未配線時は skip。
	if h.achievementGranter != nil && target.UserHost == nil && target.UserID != user.ID {
		if _, err := h.achievementGranter.Grant(c.Request().Context(), target.UserID, coreachievement.MyNoteFavorited1); err != nil {
			slog.Warn("favorites/create: achievement grant failed", "author", target.UserID, "err", err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// FavoritesDelete handles POST /api/notes/favorites/delete.
func (h *Handler) FavoritesDelete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		NoteID string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if h.favoriteRepo == nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream favorites/delete.ts は getNote で存在確認 (無ければ NO_SUCH_NOTE)
	// → favorite 行が無ければ NOT_FAVORITED を返す (#1538)。旧実装は Delete を
	// 直接呼び未 favorite でも 204 を返していた。delete は自分の favorite 行のみ
	// 操作するので visibility gate は不要 (可視性を絞られた後でも un-favorite
	// できるべき)、存在確認のみ行う。
	if _, err := h.noteRepo.FindByID(req.NoteID); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "80848a2c-398f-4343-baa9-df1d57696c56"))
	}
	if exists, _ := h.favoriteRepo.Exists(user.ID, req.NoteID); !exists {
		return c.JSON(http.StatusBadRequest, apierr.Error("NOT_FAVORITED", "You have not favorited that note.", "b625fc69-635e-45e9-86f4-dbefbef35af5"))
	}
	if err := h.favoriteRepo.Delete(user.ID, req.NoteID); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.NoContent(http.StatusNoContent)
}

// Featured handles POST /api/notes/featured.
//
// 注目ノート — renoteCount + repliesCount が高いノートを返す簡易版。
// channelId 指定時は該当チャンネル内のノートだけを返す (channel.vue の
// ハイライトタブから叩かれる経路)。過去はこの絞り込みが無く、無関係な
// グローバルノートが混ざっていた (#489)。untilId は cursor pagination 用。
func (h *Handler) Featured(c echo.Context) error {
	var req struct {
		Limit     *int   `json:"limit"`
		Offset    int    `json:"offset"`
		ChannelID string `json:"channelId"`
		UntilID   string `json:"untilId"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// untilDate を aidx prefix に正規化 (#1166)。
	_, untilID, cursorOK := id.NormalizeCursor("", req.UntilID, nil, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// upstream featured.ts は未ログインの閲覧者に generateUgcVisibilityQueryForVisitor
	// を掛ける (`none` は空、`local` はリモートの投稿者のノートを除く)。
	viewer := middleware.GetUser(c)
	localOnly := false
	if viewer == nil {
		policy := h.ugcVisibilityNow()
		if ugcvisibility.HidesAll(policy) {
			return c.JSON(http.StatusOK, []any{})
		}
		localOnly = policy == ugcvisibility.Local
	}
	notes, err := h.featuredNotes(c.Request().Context(), viewer, req.ChannelID, untilID, limit, req.Offset, localOnly)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, viewer))
}

// featured ランキング取得の threshold / cache TTL (upstream featured.ts)。
const (
	featuredGlobalThreshold    = 100
	featuredInChannelThreshold = 50
	featuredGlobalCacheTTL     = 30 * time.Minute
)

// featuredNotes returns featured notes from the engagement ranking (#1687) when
// the ranking reader is wired, falling back to the SQL count-DESC ranking when
// it is unavailable or empty (fresh instance / Redis flush). The ranking path
// follows upstream featured.ts (2026.10.0): sort id DESC, drop ids >= untilId,
// fetch every remaining note, filter, and only then cut to limit.
//
// localOnly drops notes whose own author is remote (anonymous visitors under
// meta.ugcVisibilityForVisitor=local). viewer drives the mute / block filters.
func (h *Handler) featuredNotes(ctx context.Context, viewer *model.User, channelID, untilID string, limit, offset int, localOnly bool) ([]*model.Note, error) {
	notes, err := h.fetchFeaturedNotes(ctx, channelID, untilID, limit, offset)
	if err != nil {
		return nil, err
	}
	if localOnly {
		out := make([]*model.Note, 0, len(notes))
		for _, n := range notes {
			if n.UserHost == nil {
				out = append(out, n)
			}
		}
		notes = out
	}
	// upstream featured.ts は me の mute / 被block を isUserRelated で除外する
	// (#1682)。users/featured-notes (#1547) と同じく被block / mute / instance-mute を
	// 取得後に除外し、blocked-host / suspended も applyMuteBlock が落とす (#1783)。
	notes, err = h.applyMuteBlock(viewer, notes)
	if err != nil {
		return nil, err
	}
	// 本家 2026.10.0 は limit で切るのを絞り込みの後へ移した。先に切ると、上位が
	// ミュート・ブロック相手やリモートの投稿者で埋まったときにページが欠ける。
	if len(notes) > limit {
		notes = notes[:limit]
	}
	return notes, nil
}

// fetchFeaturedNotes fetches the featured candidates before featuredNotes
// filters them. The ranking path returns every ranked note below untilID; the
// SQL fallback returns at most limit rows from offset.
func (h *Handler) fetchFeaturedNotes(ctx context.Context, channelID, untilID string, limit, offset int) ([]*model.Note, error) {
	// SQL fallback (ranking が無い / 空のとき) は upstream に無い経路なので、
	// これまでどおり limit / offset で引いてから絞る (件数が欠けることがある。
	// docs/divergence.md)。
	if h.featuredRanking == nil {
		return h.noteRepo.ListFeatured(channelID, untilID, limit, offset)
	}
	var ids []string
	var err error
	if channelID != "" {
		ids, err = h.featuredRanking.GetInChannelNotesRanking(ctx, channelID, featuredInChannelThreshold)
	} else {
		ids, err = h.cachedGlobalRanking(ctx)
	}
	if err != nil || len(ids) == 0 {
		// Redis ranking が空 / 取得失敗 (fresh instance 等) は SQL fallback。
		return h.noteRepo.ListFeatured(channelID, untilID, limit, offset)
	}
	// ranking は現在と直前の窓から最大 (threshold+1)*2 件 (global 202 / channel 102)
	// しか返さないので、全件を引いても取得量は有界。
	ids = sortAndFilterFeaturedIDs(ids, untilID)
	if len(ids) == 0 {
		return []*model.Note{}, nil
	}
	return h.noteRepo.FindManyByIDsWithUser(ids)
}

// cachedGlobalRanking returns the global ranking with a 30-min in-memory cache
// (upstream featured.ts の globalNotesRankingCache)。
func (h *Handler) cachedGlobalRanking(ctx context.Context) ([]string, error) {
	h.featuredGlobalCache.mu.Lock()
	defer h.featuredGlobalCache.mu.Unlock()
	if !h.featuredGlobalCache.fetchedAt.IsZero() && time.Since(h.featuredGlobalCache.fetchedAt) < featuredGlobalCacheTTL {
		return h.featuredGlobalCache.ids, nil
	}
	ids, err := h.featuredRanking.GetGlobalNotesRanking(ctx, featuredGlobalThreshold)
	if err != nil {
		return nil, err
	}
	h.featuredGlobalCache.ids = ids
	h.featuredGlobalCache.fetchedAt = time.Now()
	return ids, nil
}

// sortAndFilterFeaturedIDs sorts note IDs DESC and drops IDs >= untilID
// (upstream featured.ts の noteIds.sort / filter)。limit で切るのは絞り込みの
// 後なのでここでは切らない。入力 slice を破壊しないようコピーしてから操作する
// (cache 共有のため)。
func sortAndFilterFeaturedIDs(ids []string, untilID string) []string {
	out := make([]string, len(ids))
	copy(out, ids)
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	if untilID != "" {
		filtered := out[:0]
		for _, noteID := range out {
			if noteID < untilID {
				filtered = append(filtered, noteID)
			}
		}
		out = filtered
	}
	return out
}

// Unrenote handles POST /api/notes/unrenote.
func (h *Handler) Unrenote(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		NoteID string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if _, err := h.noteRepo.FindByID(req.NoteID); err != nil {
		if !repository.IsNotFound(err) {
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "efd4a259-2442-496b-8dd7-b255aa1a160f"))
	}
	// renoteId が指定ノートの自分のノートを全て削除
	renotes, err := h.noteRepo.ListRenotesByUser(user.ID, req.NoteID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	for _, renote := range renotes {
		if err := h.deleteService.Delete(user, renote.ID); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
	}
	return c.NoContent(http.StatusNoContent)
}

// Mentions handles POST /api/notes/mentions.
func (h *Handler) Mentions(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		Limit      *int   `json:"limit"`
		SinceID    string `json:"sinceId"`
		UntilID    string `json:"untilId"`
		SinceDate  *int64 `json:"sinceDate"`
		UntilDate  *int64 `json:"untilDate"`
		Visibility string `json:"visibility"`
		Following  bool   `json:"following"`
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
	// visibility kind の絞り込みは ListMentions の SQL push-down に委譲する
	// (#1451)。upstream TS notes/mentions と同じく、visibility 指定時のみ
	// note.visibility = <値> で exact-match し、未指定は全種別を返す。旧実装は
	// post-fetch で specified / 非specified に振り分けていたため、ページ内が片方の
	// 種別で埋まると limit 未満になる under-fill が起きていた。
	// following=true のとき followee + 自分の note のみに絞る (upstream
	// mentions.ts following param、#1554)。
	notes, err := h.noteRepo.ListMentions(user.ID, req.Visibility, req.Following, limit, sinceID, untilID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// upstream mentions は generateBaseNoteFilteringQuery + generateMutedNoteThreadQuery
	// で被block / mute / instance-mute / thread-mute を除外する (#1554)。
	notes, err = h.applyMuteBlock(user, notes)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	notes, err = h.applyThreadMute(user, notes)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, user))
}

// UserListTimeline handles POST /api/notes/user-list-timeline.
func (h *Handler) UserListTimeline(c echo.Context) error {
	me := middleware.GetUser(c)
	var req struct {
		ListID                string `json:"listId"`
		Limit                 *int   `json:"limit"`
		SinceID               string `json:"sinceId"`
		UntilID               string `json:"untilId"`
		SinceDate             *int64 `json:"sinceDate"`
		UntilDate             *int64 `json:"untilDate"`
		WithFiles             bool   `json:"withFiles"`
		WithRenotes           *bool  `json:"withRenotes"`
		IncludeMyRenotes      *bool  `json:"includeMyRenotes"`
		IncludeRenotedMyNotes *bool  `json:"includeRenotedMyNotes"`
		IncludeLocalRenotes   *bool  `json:"includeLocalRenotes"`
	}
	if err := c.Bind(&req); err != nil || req.ListID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "listId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// リスト所有権チェック (TS互換: 自分のリストのみ閲覧可)
	if h.userListRepo != nil {
		list, err := h.userListRepo.FindByID(req.ListID)
		if err != nil && !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		if err != nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "8fb1fbd5-e476-4c37-9fb0-43d55b63a2ff"))
		}
		if list.UserID != me.ID {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_LIST", "No such list.", "8fb1fbd5-e476-4c37-9fb0-43d55b63a2ff"))
		}
	}
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// list owner gate だけでは note の visibility を守れない。list メンバーは
	// 自由に編集できるため、未フォローのアカウントを list に詰めれば followers
	// visibility note を読めてしまう (#1442)。#1452 で visibility を
	// ListByUserList の SQL push-down に移し、LIMIT 前に絞ることで under-fill と
	// followers 判定 N+1 を解消した。viewer の見える note だけが返るため handler
	// 側の post-fetch FilterVisible / fail-closed ガードは不要。
	// RequireAuth() 配下なので me は非nil (所有権チェックでも me.ID を直接参照)。
	//
	// renote/file 系 param は他 timeline と同じ filter で SQL push-down する
	// (#1498)。WithReplies は per-member (membership.withReplies, #1496) で別途
	// 処理されるため filter には積まない。
	// upstream user-list-timeline.ts:188-201 は generateBaseNoteFilteringQuery +
	// generateMutedUserRenotesQueryForNotes + mutedChannelIds を適用するため、
	// 他 timeline と同じ base-filter (user-mute / renote-mute / 被block /
	// instance-mute / channel-mute) を積む (#1681)。
	filter := model.TimelineDBFilter{
		ViewerID:                me.ID,
		WithFiles:               req.WithFiles,
		WithRenotes:             req.WithRenotes,
		IncludeMyRenotes:        req.IncludeMyRenotes,
		IncludeRenotedMyNotes:   req.IncludeRenotedMyNotes,
		IncludeLocalRenotes:     req.IncludeLocalRenotes,
		UseMutingSubquery:       true,
		UseRenoteMutingSubquery: true,
		MutedChannelIDs:         h.loadMutedChannelIDs(me),
		BlockerIDs:              h.loadBlockerIDs(me),
		MutedInstances:          h.loadMutedInstances(me),
	}
	notes, err := h.noteRepo.ListByUserList(req.ListID, limit, sinceID, untilID, filter)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// **ブロック済みインスタンスのノートを落とす** (upstream
	// generateBlockedHostQueryForNote)。list のメンバーは自由に編集できるので、
	// ブロック後もリストに残っている相手のノートがここから出続けていた。
	// SQL push-down 側には入っていないので post-fetch で落とす。
	blocked, err := notesfilter.LoadBlockedHosts(h.metaRepo)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes = notesfilter.ApplyBlockedHosts(notes, blocked)
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, me))
}

// SearchByTag handles POST /api/notes/search-by-tag.
// TS互換: tag (string) または query (string[][] — 外側 OR・内側 AND の複合タグ
// 検索) を受け付ける (#1683、upstream search-by-tag.ts)。tag が指定されれば
// 単一タグ、無ければ query を OR-of-AND で検索する。
func (h *Handler) SearchByTag(c echo.Context) error {
	var req struct {
		Tag       string     `json:"tag"`
		Query     [][]string `json:"query"`
		Limit     *int       `json:"limit"`
		SinceID   string     `json:"sinceId"`
		UntilID   string     `json:"untilId"`
		SinceDate *int64     `json:"sinceDate"`
		UntilDate *int64     `json:"untilDate"`
		Reply     *bool      `json:"reply"`
		Renote    *bool      `json:"renote"`
		Poll      *bool      `json:"poll"`
		WithFiles bool       `json:"withFiles"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "tag is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// tag / query を tagGroups ([][]string、外側 OR・内側 AND) に正規化する。
	// upstream: 'tag' があれば単一タグ、無ければ query をそのまま使う。
	var tagGroups [][]string
	if req.Tag != "" {
		tagGroups = [][]string{{req.Tag}}
	} else {
		for _, inner := range req.Query {
			g := make([]string, 0, len(inner))
			for _, t := range inner {
				if t != "" {
					g = append(g, t)
				}
			}
			if len(g) > 0 {
				tagGroups = append(tagGroups, g)
			}
		}
	}
	if len(tagGroups) == 0 {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "tag is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
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
	// tagsカラムにtagを含むノートを検索。visibility は repository 側で
	// push-down する (#1439)。discovery 系の tag 検索は notes/show の
	// 「ID 既知公開」doctrine 対象外なので、匿名/非follower には followers/
	// specified note を返さない。
	viewer := middleware.GetUser(c)
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	// reply/renote/poll/withFiles で絞る (upstream search-by-tag.ts、#1554)。
	filter := model.NoteSearchTagFilter{Reply: req.Reply, Renote: req.Renote, Poll: req.Poll, WithFiles: req.WithFiles}
	// upstream search-by-tag.ts は未ログインの閲覧者に generateVisibilityQuery
	// (`none` で `1=0`) と generateUgcVisibilityQueryForVisitor (`local` で
	// userHost IS NULL) を掛ける。`local` は LIMIT の前に SQL で絞る。
	if viewer == nil {
		policy := h.ugcVisibilityNow()
		if ugcvisibility.HidesAll(policy) {
			return c.JSON(http.StatusOK, []entity.NoteEntity{})
		}
		filter.LocalUsersOnly = policy == ugcvisibility.Local
	}
	notes, err := h.noteRepo.SearchByTag(tagGroups, viewerID, limit, sinceID, untilID, filter)
	if err != nil {
		// tag 検索失敗は従来どおり空配列で返す (TS 互換) が、visibility
		// push-down 追加で SQL エラーも黙殺されうるため診断用に 1 行残す。
		// ユーザー挙動 (200 + 空配列) は不変で operator-actionable でもないため
		// Warn ではなく Debug に留める (#1446 review)。
		slog.Debug("notes/search-by-tag: SearchByTag failed", "tag", req.Tag, "err", err)
		return c.JSON(http.StatusOK, []entity.NoteEntity{})
	}
	// upstream search-by-tag は generateBaseNoteFilteringQuery で被block / mute /
	// instance-mute を除外する (#1554)。block/mute は post-fetch で適用する。
	notes, err = h.applyMuteBlock(viewer, notes)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, viewer))
}

// SetClipRepos wires the clip repos used by notes/clips (#1554)。未配線時は
// 旧 stub 同様に空配列を返す。
func (h *Handler) SetClipRepos(clip repository.ClipRepository, clipNote repository.ClipNoteRepository, clipFavorite repository.ClipFavoriteRepository) {
	h.clipRepo = clip
	h.clipNoteRepo = clipNote
	h.clipFavoriteRepo = clipFavorite
}

// Clips handles POST /api/notes/clips.
//
// upstream notes/clips.ts: getNote で note 存在確認 (無ければ NO_SUCH_NOTE) →
// clipNotesRepository.findBy({noteId}) で note を含む clip を引き →
// clipsRepository.findBy({id IN, isPublic:true}) → clipEntityService.packMany。
// owner / 他人を問わず「その note を含む public clip」を返す (#1554)。
func (h *Handler) Clips(c echo.Context) error {
	var req struct {
		NoteID string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// clip repo 未配線時は旧 stub 互換で空配列。
	if h.clipRepo == nil || h.clipNoteRepo == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// note 存在確認 (upstream getterService.getNote)。可視性は問わず存在のみ。
	if _, err := h.noteRepo.FindByID(req.NoteID); err != nil {
		if !repository.IsNotFound(err) {
			// **DB 障害を not-found に丸めない** (#2792)。
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "47db1a1c-b0af-458d-8fb4-986e4efafe1e"))
	}
	clipIDs, err := h.clipNoteRepo.ListClipIDsByNote(req.NoteID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	clips, err := h.clipRepo.ListPublicByIDs(clipIDs)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	viewer := middleware.GetUser(c)
	out := make([]map[string]any, 0, len(clips))
	for _, cl := range clips {
		// owner は clip ごとに異なりうる (note は複数 user の clip に入りうる)。
		var owner *model.User
		if h.userRepo != nil {
			owner, _ = h.userRepo.FindByID(cl.UserID)
		}
		// favoritedCount は常時、isFavorited は認証 viewer のみ、notesCount は
		// owner 閲覧時のみ (#1562、upstream ClipEntityService.pack)。
		extras := entity.ClipExtras{}
		// notesCount は owner 閲覧時のみ。clip.notesCount 列は upstream に無く
		// TS 製 DB では生えないため clip_note を実カウントする (#2243)。
		if viewer != nil && viewer.ID == cl.UserID && h.clipNoteRepo != nil {
			if n, err := h.clipNoteRepo.CountByClip(cl.ID); err == nil {
				count := int(n)
				extras.NotesCount = &count
			}
		}
		if h.clipFavoriteRepo != nil {
			if n, err := h.clipFavoriteRepo.CountByClip(cl.ID); err == nil {
				extras.FavoritedCount = n
			}
			if viewer != nil {
				if ok, err := h.clipFavoriteRepo.Exists(viewer.ID, cl.ID); err == nil {
					extras.IsFavorited = &ok
				}
			}
		}
		out = append(out, entity.PackClip(cl, h.idGen, owner, extras))
	}
	return c.JSON(http.StatusOK, out)
}

// Translate handles POST /api/notes/translate.
func (h *Handler) Translate(c echo.Context) error {
	var req struct {
		NoteID     string `json:"noteId"`
		TargetLang string `json:"targetLang"`
	}
	if err := c.Bind(&req); err != nil || req.NoteID == "" || req.TargetLang == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteId and targetLang are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	// upstream translate.ts:72-75: canUseTranslator policy が false なら note fetch より
	// 前に 503 UNAVAILABLE を返す。汎用 RequireRolePolicy middleware の 403
	// ROLE_PERMISSION_DENIED ではなく translate 固有の error なので handler 側で評価する
	// (#2010)。policyProvider 未配線 (test) では gate を skip。
	if u := middleware.GetUser(c); u != nil && h.policyProvider != nil {
		if v, ok := h.policyProvider.GetUserPolicies(u.ID)["canUseTranslator"].(bool); !ok || !v {
			// upstream の unavailable error は httpStatusCode 未指定 + kind 既定 'client'
			// なので ApiCallService が 400 にマップする (503 ではない、#2010)。
			return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Translate of notes unavailable.", "50a70314-2d8a-431b-b433-efa5cc56444c"))
		}
	}

	// upstream translate.ts の順序に合わせる (#1948-17): note fetch → visibility →
	// text==null で 204 → unavailable → translate。translator-nil(unavailable) を
	// note/text check より前に置くと、null-text note が upstream の 204 ではなく
	// UNAVAILABLE を返してしまうため後ろに移動する。
	n, err := h.noteRepo.FindByID(req.NoteID)
	if err != nil && !repository.IsNotFound(err) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "bea9b03f-36e0-49c5-a4db-627a029f8971"))
	}

	// 旧実装は存在確認のみで visibility check が無く、note ID 既知の viewer が
	// followers / specified note を翻訳 = 本文の content leak + DeepL quota 消費
	// ができた (#1445)。upstream TS `notes/translate` は非可視 note を専用の
	// CANNOT_TRANSLATE_INVISIBLE_NOTE で弾く (NO_SUCH_NOTE で隠さない) ため
	// それに合わせる。queryService 未配線時は fail-closed で同エラー。
	// この gate は translator.Translate より前なので DeepL を消費しない。
	viewer := middleware.GetUser(c)
	if h.queryService == nil || !h.queryService.CanSee(viewer, n) {
		return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_TRANSLATE_INVISIBLE_NOTE", "Cannot translate invisible note.", "ea29f2ca-c368-43b3-aaf1-5ac3e74bbe5d"))
	}

	// upstream 2026.10.0 translate.ts と同じく、visibility の後に
	// `(await noteEntityService.pack(note, me)).isHidden` 相当でも弾く。CanSee は
	// 著者の makeNotesHiddenBefore / makeNotesFollowersOnlyBefore /
	// requireSigninToViewContents を見ないので、別に判定する。translator より前に
	// 置き、DeepL を呼ばない。
	hidden, err := h.translateHiddenByAuthorPrefs(viewer, n)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	if hidden {
		return c.JSON(http.StatusBadRequest, apierr.Error("CANNOT_TRANSLATE_INVISIBLE_NOTE", "Cannot translate invisible note.", "ea29f2ca-c368-43b3-aaf1-5ac3e74bbe5d"))
	}

	// upstream translate.ts: CW があれば `<cw>\n-----\n<text>` を翻訳対象にする
	// (upstream ab26d2b7b2)。text が無くても CW だけあれば翻訳する。
	text := ""
	if n.Text != nil {
		text = *n.Text
	}
	if n.CW != nil {
		text = *n.CW + "\n-----\n" + text
	}

	// upstream は 2026.9.0 で判定を `note.text == null` から `text.trim() === ''`
	// に変えた。空白のみ / 空文字は DeepL へ投げずに 204 (res optional → No Content)。
	// **空白の集合は完全一致ではない** — JS の trim は U+FEFF を落とすが Go の
	// unicode.IsSpace は落とさず、逆に Go は U+0085 を落とすが JS は落とさない。
	// それだけで構成された本文でのみ分岐する。
	// mk-go の旧 CANNOT_TRANSLATE は upstream に無い独自 error なので廃止済み (#1948-17)。
	if strings.TrimSpace(text) == "" {
		return c.NoContent(http.StatusNoContent)
	}

	if h.translator == nil {
		// upstream deeplAuthKey==null も同 unavailable error で 400 (#2010)。
		return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Translate of notes unavailable.", "50a70314-2d8a-431b-b433-efa5cc56444c"))
	}

	result, err := h.translator.Translate(c.Request().Context(), text, req.TargetLang)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Translation failed.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	return c.JSON(http.StatusOK, map[string]any{
		"sourceLang": result.SourceLang,
		"text":       result.Text,
	})
}

// translateHiddenByAuthorPrefs reports whether the packed note would carry
// isHidden for viewer because of the author's preference gates
// (corenote.HideNoteByPrefsDecision), mirroring upstream translate.ts's
// `pack(note, me).isHidden` check. A non-nil error means a lookup failed and
// the caller must not treat it as "not hidden".
func (h *Handler) translateHiddenByAuthorPrefs(viewer *model.User, n *model.Note) (bool, error) {
	author := n.User
	if author == nil && h.userRepo != nil {
		u, err := h.userRepo.FindByID(n.UserID)
		if err != nil && !repository.IsNotFound(err) {
			return false, err
		}
		if u == nil {
			// 著者が引けない note は設定を確かめられないので翻訳させない (fail-closed)。
			return true, nil
		}
		author = u
	}

	f := corenote.EmbedFacts{
		AuthorID:       n.UserID,
		Visibility:     string(n.Visibility),
		VisibleUserIDs: n.VisibleUserIDs,
		Mentions:       n.Mentions,
	}
	if n.ReplyUserID != nil {
		f.ReplyTargetAuthorID = *n.ReplyUserID
	}
	// 作成時刻が分からないときは epoch 0 扱いにして、期間設定のゲートを
	// 「隠す」側に倒す (notehide.parseCreatedAtMs と同じ方針)。
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(n.ID); err == nil {
			f.CreatedAtMs = t.UnixMilli()
		}
	}
	// userRepo 未配線 (テストの最小構成) では著者設定が分からず、他の経路と同じく
	// AuthorPrefsKnown=false で著者設定のゲートを評価しない。本番は SetUserRepo
	// で常に配線される。
	if author != nil {
		f.AuthorPrefsKnown = true
		f.RequireSigninToViewContents = author.RequireSigninToViewContents
		f.MakeNotesHiddenBefore = author.MakeNotesHiddenBefore
		f.MakeNotesFollowersOnlyBefore = author.MakeNotesFollowersOnlyBefore
	}

	// **follow の lookup 障害を「フォローしていない」に丸めない** (#2792)。
	// 丸めると一時的な障害でフォロワーの翻訳が 400 になる。
	var followErr error
	follows := func(authorID string) bool {
		if viewer == nil || h.userFollowingRepo == nil {
			return false
		}
		ok, err := h.userFollowingRepo.Exists(viewer.ID, authorID)
		if err != nil {
			followErr = err
			return false
		}
		return ok
	}
	hidden := corenote.HideNoteByPrefsDecision(viewer, f, follows, time.Now().UnixMilli())
	if followErr != nil {
		return false, followErr
	}
	return hidden, nil
}

// ShowPartialBulk handles POST /api/notes/show-partial-bulk.
func (h *Handler) ShowPartialBulk(c echo.Context) error {
	var req struct {
		NoteIDs []string `json:"noteIds"`
	}
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "noteIds is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if len(req.NoteIDs) == 0 {
		return c.JSON(http.StatusOK, []any{})
	}
	viewer := middleware.GetUser(c)
	// upstream fetchDiffs は ugcVisibilityForVisitor=none の未ログインの閲覧者に
	// ノートを引く前に [] を返す。`local` は upstream も絞らない (TODO のまま)。
	if h.visitorHidesAll(viewer) {
		return c.JSON(http.StatusOK, []any{})
	}
	notes, err := h.noteRepo.FindManyByIDsWithUser(req.NoteIDs)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	// ShowPartialBulk は anonymous でも叩ける endpoint (router で RequireAuth()
	// 無し) のため、followers / specified visibility のノートが任意の閲覧者に
	// 漏洩しないよう FilterVisible に通す (#509、Devin #529 FLAG-1)。queryService
	// 未配線時は fail-closed で空配列。upstream fetchDiffs も 2026.10.0 から
	// isVisibleForMe でノートごとに可視性を確かめるようになり、挙動が揃った
	// (以前はここを意図的な divergence としていた)。
	if h.queryService == nil {
		return c.JSON(http.StatusOK, []any{})
	}
	notes = h.queryService.FilterVisible(viewer, notes)
	// 本家 show-partial-bulk は fetchDiffs で {id, reactions, reactionEmojis} の
	// 軽量 diff のみ返す (reaction polling 用途、本文/可視性詳細は返さない)。
	// packMany が reactions (buffered merge 込み) と reactionEmojis を解決済みなので
	// それを投影して shape を揃える (#1538)。
	packed := h.packMany(c.Request().Context(), notes, viewer)
	diffs := make([]map[string]any, 0, len(packed))
	for i := range packed {
		diffs = append(diffs, map[string]any{
			"id":             packed[i].ID,
			"reactions":      packed[i].Reactions,
			"reactionEmojis": packed[i].ReactionEmojis,
		})
	}
	return c.JSON(http.StatusOK, diffs)
}

// NoteMaterializer promotes a relay-delivered note out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
type NoteMaterializer interface {
	EnsureNote(ctx context.Context, noteID string) (*model.Note, error)
}

// SetNoteMaterializer attaches the ephemeral-note materializer. Optional.
func (h *Handler) SetNoteMaterializer(m NoteMaterializer) {
	h.materializer = m
}

// materializeIfMissing promotes an ephemeral note only when the lookup already
// failed. 通常のノートでは Redis を一切引かない。
func (h *Handler) materializeIfMissing(noteID string, lookupErr error) bool {
	if lookupErr == nil || h.materializer == nil {
		return false
	}
	// **DB 障害では materialize しない** (#2799)。`RequireVisible` は not-found と
	// 非可視を `ErrNoteNotFound` に集約し、接続断だけ raw error を返す。種別を
	// 見ずに走らせると、DB 断のあいだ 1 リクエストごとに outbound の
	// remote-note fetch が 1 発出る。
	if !errors.Is(lookupErr, corenote.ErrNoteNotFound) {
		return false
	}
	_, err := h.materializer.EnsureNote(context.Background(), noteID)
	return err == nil
}
