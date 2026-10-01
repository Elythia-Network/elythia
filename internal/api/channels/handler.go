// Package channels provides /api/channels/* endpoints.
package channels

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/notehide"
	"github.com/shiroha-a/mk/internal/api/optional"
	"github.com/shiroha-a/mk/internal/api/pagination"
	corechannel "github.com/shiroha-a/mk/internal/core/channel"
	"github.com/shiroha-a/mk/internal/core/notesfilter"
	"github.com/shiroha-a/mk/internal/core/ugcvisibility"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Handler handles channel-related API endpoints.
type Handler struct {
	svc           *corechannel.Service
	idGen         id.Generator
	favoriteRepo  ChannelFavoriteRepository
	mutingRepo    ChannelMutingRepository
	followingRepo ChannelFollowingChecker
	instanceRepo  repository.InstanceRepository
	emojiRepo     repository.EmojiRepository
	bufReader     entity.BufferedReactionsReader
	fieldRes      *entity.NoteFieldResolver
	// userRepo は channels/timeline の hardMutedWords filter (#787)。
	userRepo repository.UserRepository
	// bannerResolver は channel の bannerId を drive file に解決して bannerUrl
	// を埋める (#1280)。未配線なら bannerUrl は null で出す。
	bannerResolver ChannelBannerResolver
	// pinnedNoteRepo は channels/show (detailed) の pinnedNotes 展開に使う
	// (#1540)。未配線なら pinnedNotes は空配列で出す。
	pinnedNoteRepo ChannelPinnedNoteRepo
	// userMutingRepo / blockingRepo / noteRepo は channels/timeline の
	// mute / block filter に使う。upstream は channel timeline にも
	// generateMutedUserQueryForNotes / generateBlockedUserQueryForNotes 相当を
	// 通しており、閲覧中チャンネル内でもミュート相手やブロック相手のノートは
	// 出ない。未配線なら filter は no-op。
	userMutingRepo repository.MutingRepository
	blockingRepo   repository.BlockingRepository
	noteRepo       notesfilter.RenoteLookup
	// relationReload は channel mute 変更を streaming connection へ通知する (#2400)。
	relationReload RelationReloadPublisher
	// userFollowingRepo は pinnedNotes の可視性判定に使う。**`followingRepo` とは
	// 別物** — あちらはチャンネルのフォロー、こちらは利用者間のフォローで、
	// followers 限定ノートを見せてよい相手かの判定に要る。未配線なら
	// `CanSeeNote` が fail-closed に倒れる (followers / specified は投稿者本人
	// 以外に見せない)。
	userFollowingRepo repository.FollowingRepository
	// metaRepo は channels/timeline の blocked-host filter で meta.blockedHosts
	// を引く (upstream generateBlockedHostQueryForNote)。
	metaRepo repository.MetaRepository
	// ugcVisibilityFn は meta.ugcVisibilityForVisitor の live lookup。
	// channels/timeline の匿名 visitor への gate に使う。
	ugcVisibilityFn func() string
}

// SetUGCVisibilityLookup wires a live lookup of meta.ugcVisibilityForVisitor.
//
// **毎回読む。** 起動時に焼き込むと、運営者が管理画面で締めても
// プロセスを再起動するまで反映されない。
func (h *Handler) SetUGCVisibilityLookup(fn func() string) {
	h.ugcVisibilityFn = fn
}

// ugcVisibilityNow resolves the current policy ("" when unwired, which the
// helpers treat as "all").
func (h *Handler) ugcVisibilityNow() string {
	if h.ugcVisibilityFn == nil {
		return ""
	}
	return h.ugcVisibilityFn()
}

// HasUGCVisibility reports whether the visitor content visibility lookup was
// wired. 未配線だと channels/timeline の匿名 visitor への gate が素通しになる。
// 起動時検査に使う。
func (h *Handler) HasUGCVisibility() bool { return h.ugcVisibilityFn != nil }

// HasMetaRepo reports whether the blocked-host filter can read meta.
//
// 未配線だと `blockedHosts` の除外が黙って no-op になる (ブロックしたはずの
// インスタンスのノートが一覧に出続ける)。起動時の critical wiring 検査で落とす。
func (h *Handler) HasMetaRepo() bool {
	return h != nil && h.metaRepo != nil
}

// SetMetaRepo wires a MetaRepository used for the blocked-host filter on
// channels/timeline (upstream generateBlockedHostQueryForNote)。
func (h *Handler) SetMetaRepo(r repository.MetaRepository) {
	h.metaRepo = r
}

// SetUserFollowingRepo wires the user-following lookup used by the pinnedNotes
// visibility gate.
//
// **チャンネルのフォロー (`SetFollowingRepo`) とは別。** 名前が似ているので
// 取り違えると「チャンネルをフォローしていれば他人の followers 限定ノートが
// 見える」という別の穴になる。
func (h *Handler) SetUserFollowingRepo(r repository.FollowingRepository) {
	h.userFollowingRepo = r
}

// SetMuteBlockRepos wires the repositories used by the channels/timeline
// mute / block filter.
func (h *Handler) SetMuteBlockRepos(m repository.MutingRepository, b repository.BlockingRepository, n notesfilter.RenoteLookup) {
	h.userMutingRepo, h.blockingRepo, h.noteRepo = m, b, n
}

// ChannelPinnedNoteRepo fetches the notes for the detailed channels/show
// pinnedNotes field (#1540). The returned notes follow the order of `ids`.
type ChannelPinnedNoteRepo interface {
	FindManyByIDsWithUser(ids []string) ([]*model.Note, error)
}

// SetPinnedNoteRepo wires the note repository used to expand pinnedNoteIds into
// pinnedNotes (Note[]) on the detailed channels/show response (#1540).
func (h *Handler) SetPinnedNoteRepo(r ChannelPinnedNoteRepo) {
	h.pinnedNoteRepo = r
}

// ChannelBannerResolver covers the drive-file lookups the channels handler
// needs to resolve `bannerUrl` from a channel's bannerId (#1280). FindByID
// drives the single-channel paths; FindByIDs batches the list endpoints to
// avoid an N+1 banner lookup.
type ChannelBannerResolver interface {
	FindByID(id string) (*model.DriveFile, error)
	FindByIDs(ids []string) ([]*model.DriveFile, error)
}

// SetDriveFileRepo wires a ChannelBannerResolver so packed channels expose
// `bannerUrl` (#1280). nil leaves bannerUrl as null.
func (h *Handler) SetDriveFileRepo(r ChannelBannerResolver) {
	h.bannerResolver = r
}

// SetUserRepo wires a UserRepository so channels/timeline filters out notes
// that match the viewer's hardMutedWords (#787).
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// ChannelFollowingChecker covers the subset of ChannelFollowingRepository
// the channels handler needs to embed isFollowing on viewer-aware payloads
// (#522). Single-row Exists drives the Show / Update / Create paths;
// ExistsMany handles list endpoints in one query.
type ChannelFollowingChecker interface {
	Exists(followerID, channelID string) (bool, error)
	ExistsMany(followerID string, channelIDs []string) (map[string]bool, error)
}

// SetNoteFieldResolver wires the shared resolver that fills Files /
// MyReaction / Channel on packed notes including embedded Renote / Reply
// (#426)。
func (h *Handler) SetNoteFieldResolver(r *entity.NoteFieldResolver) {
	h.fieldRes = r
}

// SetInstanceRepo attaches an InstanceRepository so channel timeline responses
// populate UserLite.Instance for remote users (#277).
func (h *Handler) SetInstanceRepo(r repository.InstanceRepository) {
	h.instanceRepo = r
}

func (h *Handler) instanceLookup() entity.InstanceLookup {
	if h.instanceRepo == nil {
		return nil
	}
	return h.instanceRepo
}

// SetEmojiRepo attaches an EmojiRepository so custom emoji shortcodes in
// note text and user displayNames get resolved to URLs.
func (h *Handler) SetEmojiRepo(r repository.EmojiRepository) {
	h.emojiRepo = r
}

// SetReactionReader wires a BufferedReactionsReader so PackNote / PackNotes
// can merge in-flight buffered reaction deltas (#647)。
func (h *Handler) SetReactionReader(r entity.BufferedReactionsReader) {
	h.bufReader = r
}

func (h *Handler) reactionReader() entity.BufferedReactionsReader {
	return h.bufReader
}

func (h *Handler) emojiLookup() entity.EmojiLookup {
	if h.emojiRepo == nil {
		return nil
	}
	return h.emojiRepo
}

// SetFollowingRepo attaches a ChannelFollowingChecker so Show / list
// endpoints can embed `isFollowing` for the authenticated viewer (#522).
// nil leaves the field unset (compat with anonymous flows).
func (h *Handler) SetFollowingRepo(r ChannelFollowingChecker) {
	h.followingRepo = r
}

// ChannelFavoriteRepository is the interface for channel favorite operations.
type ChannelFavoriteRepository interface {
	Create(fav *model.ChannelFavorite) error
	Delete(userID, channelID string) error
	ListByUser(userID string) ([]*model.ChannelFavorite, error)
	Exists(userID, channelID string) (bool, error)
	// ExistsMany powers list-endpoint isFavorited embedding without N+1
	// (#522). Returns a channelID → bool map.
	ExistsMany(userID string, channelIDs []string) (map[string]bool, error)
}

// ChannelMutingRepository is the interface for channel muting operations.
type ChannelMutingRepository interface {
	Create(mut *model.ChannelMuting) error
	Delete(userID, channelID string) error
	ListByUser(userID string) ([]*model.ChannelMuting, error)
	Exists(userID, channelID string) (bool, error)
}

// NewHandler creates a new channels Handler.
func NewHandler(svc *corechannel.Service, idGen id.Generator) *Handler {
	return &Handler{svc: svc, idGen: idGen}
}

// CreateRequest is the request body for channels/create.
type CreateRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	// #2106 L13: Color は *string で absent (nil → core が default 適用) と explicit ""
	// (minLength:1 違反で reject) を区別する。
	Color                 *string `json:"color"`
	IsSensitive           bool    `json:"isSensitive"`
	BannerID              *string `json:"bannerId"`
	AllowRenoteToExternal *bool   `json:"allowRenoteToExternal"`
}

// Create handles POST /api/channels/create.
//
// canCreateChannel role policy gate は #1020 で router 側の
// middleware.RequireRolePolicy に昇格済み。handler 内では policy を見ない。
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req CreateRequest
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return apierr.JSONInvalidParam(c)
	}
	// #2106 L12/L13: upstream paramDef の長さ制約を検証する (超過時 DB の varchar 制約で
	// 500 になるのを防ぎ 400 INVALID_PARAM を返す)。name 1-128 / description <=2048 /
	// color 指定時 1-16 (空文字 color は minLength:1 違反で reject)。
	if utf8.RuneCountInString(req.Name) > 128 ||
		(req.Description != nil && utf8.RuneCountInString(*req.Description) > 2048) {
		return apierr.JSONInvalidParam(c)
	}
	if req.Color != nil {
		if n := utf8.RuneCountInString(*req.Color); n < 1 || n > 16 {
			return apierr.JSONInvalidParam(c)
		}
	}
	// bannerId="" は「banner 無し」に正規化する (TS は misskey:id format で空文字を
	// 400 にするため空文字が DB に入ることはない。mk-go では空文字 column を作らない)。
	if req.BannerID != nil && *req.BannerID == "" {
		req.BannerID = nil
	}
	// bannerId 指定時は呼出ユーザー所有の drive file であることを検証する
	// (upstream channels/create: findOneBy({id, userId: me.id}) → NO_SUCH_FILE)。
	if req.BannerID != nil {
		if !h.bannerOwnedBy(*req.BannerID, user.ID) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "cd1e9f3e-5a12-4ab4-96f6-5d0a2cc32050"))
		}
	}
	colorVal := ""
	if req.Color != nil {
		colorVal = *req.Color
	}
	ch, err := h.svc.Create(corechannel.CreateInput{
		OwnerID:               user.ID,
		Name:                  req.Name,
		Description:           req.Description,
		Color:                 colorVal,
		IsSensitive:           req.IsSensitive,
		BannerID:              req.BannerID,
		AllowRenoteToExternal: req.AllowRenoteToExternal,
	})
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// Create 直後は viewer が当該 channel を follow していないが、API
	// 互換のため空 viewer 情報も含めた map を返す。viewer 経路で is*
	// フィールドを返さないと frontend が undefined を扱えないことがある。
	return c.JSON(http.StatusOK, h.channelToMapForViewer(ch, user))
}

// ShowRequest is the request body for channels/show.
type ShowRequest struct {
	ChannelID string `json:"channelId"`
}

// Show handles POST /api/channels/show.
func (h *Handler) Show(c echo.Context) error {
	var req ShowRequest
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	ch, err := h.svc.Show(req.ChannelID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "6f6c314b-7486-4897-8966-c04a66a02923"))
	}
	viewer := middleware.GetUser(c)
	out := h.channelToMapForViewer(ch, viewer)
	// upstream show.ts は pack(channel, me, detailed=true) を呼び、detailed 時のみ
	// pinnedNotes (展開済 Note[]、pinnedNoteIds 順) を返す (#1540)。
	out["pinnedNotes"] = h.packPinnedNotes(c.Request().Context(), ch, viewer)
	return c.JSON(http.StatusOK, out)
}

// packPinnedNotes expands ch.PinnedNoteIDs into packed Note entities in
// pinnedNoteIds order for the detailed channels/show response (#1540).
// FindManyByIDsWithUser preserves the requested order, so no re-sort is needed
// (upstream re-sorts because findBy does not). Returns an empty slice when there
// are no pinned notes or the note repo / field resolver is unwired.
func (h *Handler) packPinnedNotes(ctx context.Context, ch *model.Channel, viewer *model.User) []any {
	out := make([]any, 0)
	ids := []string(ch.PinnedNoteIDs)
	if len(ids) == 0 || h.pinnedNoteRepo == nil {
		return out
	}
	notes, err := h.pinnedNoteRepo.FindManyByIDsWithUser(ids)
	if err != nil || len(notes) == 0 {
		return out
	}
	// **可視性ゲート。** `channels/update` は `pinnedNoteIds` を検証しない
	// (所有者もチャンネル所属も見ない) ので、ここに他人のノートの ID を書ける。
	// `channels/show` は未認証で叩けるため、ゲートが無いと **followers 限定
	// ノートが全文で公開される**。
	//
	// **`HideEmbeds` では止まらない。** あちらの top-level 判定
	// (`HideNoteByPrefsDecision`) は著者設定による降格しか見ず、元から
	// followers の note は「intrinsic ゲート任せ」と明記して素通しする
	// (`internal/core/note/hide_embed.go`)。その intrinsic ゲートがこれ。
	//
	// 同じ「ピン留めを展開する」経路である `users/show` と `antennas/notes` は
	// 既に `FilterVisible` を通しており、ここだけが抜けていた。
	notes = notesfilter.FilterVisible(viewer, notes, h.userFollowingRepo)
	if len(notes) == 0 {
		return out
	}
	entities := entity.PackNotes(ctx, notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	if h.fieldRes != nil {
		h.fieldRes.Apply(entities, viewer)
	}
	notehide.HideEmbeds(viewer, entities)
	for _, pn := range entities {
		out = append(out, pn)
	}
	return out
}

// UpdateRequest is the request body for channels/update.
type UpdateRequest struct {
	ChannelID             string                    `json:"channelId"`
	Name                  *string                   `json:"name"`
	Description           optional.Nullable[string] `json:"description"`
	Color                 *string                   `json:"color"`
	IsArchived            *bool                     `json:"isArchived"`
	IsSensitive           *bool                     `json:"isSensitive"`
	BannerID              *string                   `json:"bannerId"`
	PinnedNoteIDs         *[]string                 `json:"pinnedNoteIds"`
	AllowRenoteToExternal *bool                     `json:"allowRenoteToExternal"`
}

// Update handles POST /api/channels/update.
func (h *Handler) Update(c echo.Context) error {
	user := middleware.GetUser(c)
	var req UpdateRequest
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// #2106 L12: upstream update.ts paramDef の長さ制約を検証する (超過時 DB 制約 500 でなく
	// 400)。name 指定時 1-128 / description 非 null 時 <=2048 / color 指定時 1-16。
	if req.Name != nil {
		if n := utf8.RuneCountInString(*req.Name); n < 1 || n > 128 {
			return apierr.JSONInvalidParam(c)
		}
	}
	if req.Description.Present && req.Description.Value != nil &&
		utf8.RuneCountInString(*req.Description.Value) > 2048 {
		return apierr.JSONInvalidParam(c)
	}
	if req.Color != nil {
		if n := utf8.RuneCountInString(*req.Color); n < 1 || n > 16 {
			return apierr.JSONInvalidParam(c)
		}
	}
	in := corechannel.UpdateInput{
		Name:                  req.Name,
		Color:                 req.Color,
		IsArchived:            req.IsArchived,
		IsSensitive:           req.IsSensitive,
		BannerID:              req.BannerID,
		PinnedNoteIDs:         req.PinnedNoteIDs,
		AllowRenoteToExternal: req.AllowRenoteToExternal,
	}
	// upstream channels/update.ts は description を `!== undefined` で spread するため、
	// explicit null も書き込んで NULL clear する。absent は維持 (#1948-15)。
	// in.Description は **string: nil=未変更 / *が nil=NULL clear / 値=設定。
	if req.Description.Present {
		desc := req.Description.Value // *string (nil for explicit null)
		in.Description = &desc
	}
	// bannerId 設定時 (空文字 = 解除は検証不要) は所有権を検証する
	// (upstream channels/update: bannerId != null → findOneBy({id, userId}) → NO_SUCH_FILE)。
	if req.BannerID != nil && *req.BannerID != "" {
		if !h.bannerOwnedBy(*req.BannerID, user.ID) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "e86c14a4-0da2-4032-8df3-e737a04c7f3b"))
		}
	}
	ch, err := h.svc.Update(user.ID, req.ChannelID, in)
	if err != nil {
		switch {
		case errors.Is(err, corechannel.ErrChannelNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "f9c5467f-d492-4c3c-9a8d-a70dacc86512"))
		case errors.Is(err, corechannel.ErrAccessDenied):
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "1fb7cb09-d46a-4fdf-b8df-057788cce513"))
		case errors.Is(err, corechannel.ErrChannelNameRequired):
			return apierr.JSONInvalidParam(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.channelToMapForViewer(ch, user))
}

// FollowRequest / UnfollowRequest reuse the channelId field.
type FollowRequest struct {
	ChannelID string `json:"channelId"`
}

// Follow handles POST /api/channels/follow.
func (h *Handler) Follow(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FollowRequest
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Follow(user.ID, req.ChannelID); err != nil {
		switch {
		case errors.Is(err, corechannel.ErrChannelNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "c0031718-d573-4e85-928e-10039f1fbb68"))
		case errors.Is(err, corechannel.ErrAlreadyFollowing):
			return alreadyFollowing(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// Unfollow handles POST /api/channels/unfollow.
func (h *Handler) Unfollow(c echo.Context) error {
	user := middleware.GetUser(c)
	var req FollowRequest
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Unfollow(user.ID, req.ChannelID); err != nil {
		if errors.Is(err, corechannel.ErrNotFollowing) {
			return notFollowing(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// PaginatedListRequest is shared by followed / owned / featured / search /
// timeline endpoints. frontend Paginator は cursor mode で sinceId / untilId
// を投げてくる (#493)。
type PaginatedListRequest struct {
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// Followed handles POST /api/channels/followed.
//
// frontend Paginator は response item の id (= channel.id = followeeId)
// を cursor として送ってくる。repository.ListFollowed 側で followeeId を
// cursor 列として使うことで domain mismatch を避けている。詳細は
// channel_following.go:ListFollowed の comment 参照。
func (h *Handler) Followed(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PaginatedListRequest
	if err := c.Bind(&req); err != nil {
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
	rows, err := h.svc.ListFollowed(user.ID, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.channelsToList(rows, user))
}

// Owned handles POST /api/channels/owned.
func (h *Handler) Owned(c echo.Context) error {
	user := middleware.GetUser(c)
	var req PaginatedListRequest
	if err := c.Bind(&req); err != nil {
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
	rows, err := h.svc.ListOwned(user.ID, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.channelsToList(rows, user))
}

// Featured handles POST /api/channels/featured.
//
// upstream featured.ts は pagination param を持たず、lastNotedAt IS NOT NULL かつ
// isArchived=FALSE の channel を lastNotedAt DESC で固定 10 件返す (#1540)。
// cursor / limit は無視する (service が固定条件で返す) が、malformed JSON は
// upstream のフレームワーク同様 400 で弾くため bind 検証だけ残す。
func (h *Handler) Featured(c echo.Context) error {
	var req PaginatedListRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	rows, err := h.svc.ListFeatured()
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.channelsToList(rows, middleware.GetUser(c)))
}

// SearchRequest carries the query string for channels/search.
type SearchRequest struct {
	Query     string `json:"query"`
	Type      string `json:"type"`
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// Search handles POST /api/channels/search.
func (h *Handler) Search(c echo.Context) error {
	var req SearchRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	// type は enum ['nameAndDescription','nameOnly'] (空は default)。upstream は
	// enum 外を 400 で弾くので silent fallback せず合わせる。
	if req.Type != "" && req.Type != "nameAndDescription" && req.Type != "nameOnly" {
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
	rows, err := h.svc.Search(req.Query, req.Type, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.channelsToList(rows, middleware.GetUser(c)))
}

// bannerOwnedBy reports whether bannerID resolves to a drive file owned by
// userID. Used to validate channels/create・update の bannerId
// (upstream は findOneBy({id, userId: me.id}) で所有を確認する)。bannerResolver
// 未配線時は検証不能なので true を返さず false (fail-closed) にする。
func (h *Handler) bannerOwnedBy(bannerID, userID string) bool {
	if h.bannerResolver == nil {
		return false
	}
	f, err := h.bannerResolver.FindByID(bannerID)
	if err != nil || f == nil || f.UserID == nil || *f.UserID != userID {
		return false
	}
	return true
}

// TimelineRequest is the request body for channels/timeline.
type TimelineRequest struct {
	ChannelID string `json:"channelId"`
	UntilID   string `json:"untilId"`
	SinceID   string `json:"sinceId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
	Limit     *int   `json:"limit"`
}

// Timeline handles POST /api/channels/timeline.
func (h *Handler) Timeline(c echo.Context) error {
	var req TimelineRequest
	if err := c.Bind(&req); err != nil || req.ChannelID == "" {
		return apierr.JSONInvalidParam(c)
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
	viewer := middleware.GetUser(c)
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	// upstream channels/timeline.ts は匿名 visitor に generateUgcVisibilityQueryForVisitor
	// (`none` → 1=0、`local` → note.userHost IS NULL) を掛ける。チャンネルの存在
	// 検査 (NO_SUCH_CHANNEL) は query より前なので、`none` でも先に存在を確かめる。
	policy := ""
	if viewer == nil {
		policy = h.ugcVisibilityNow()
	}
	if ugcvisibility.HidesAll(policy) {
		if _, err := h.svc.Show(req.ChannelID); err != nil {
			if errors.Is(err, corechannel.ErrChannelNotFound) {
				return noSuchChannelTimeline(c)
			}
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusOK, []any{})
	}
	// public channel に投稿された followers / specified note を非フォロワーに
	// 返さないよう、visibility filter は service / repo 層で LIMIT 前に SQL
	// push-down する (#1440)。`local` の絞り込みも同じく LIMIT 前に掛ける。
	notes, err := h.svc.Timeline(req.ChannelID, viewerID, untilID, sinceID, limit, policy == ugcvisibility.Local)
	if err != nil {
		if errors.Is(err, corechannel.ErrChannelNotFound) {
			return noSuchChannelTimeline(c)
		}
		return apierr.JSONInternalError(c)
	}
	// upstream channels/timeline.ts: viewer の muted channel を renote/quote した
	// note を除外する (renoteChannelId NOT IN mutedChannels)。当該 channel 自身は
	// muted set から除く (ignoreAuthorChannelFromMute、#1790)。
	if viewer != nil && h.mutingRepo != nil {
		if muts, err := h.mutingRepo.ListByUser(viewer.ID); err == nil {
			mutedChannelIDs := make([]string, 0, len(muts))
			for _, m := range muts {
				if m.ChannelID != req.ChannelID {
					mutedChannelIDs = append(mutedChannelIDs, m.ChannelID)
				}
			}
			notes = notesfilter.ApplyRenoteChannelMute(notes, mutedChannelIDs)
		}
	}
	// 閲覧中チャンネル内でも、ミュート相手 / 自分をブロックしている相手の
	// ノートは出さない (upstream channels/timeline.ts も
	// generateMutedUserQueryForNotes / generateBlockedUserQueryForNotes を通す)。
	// チャンネルミュート自体は上の ApplyRenoteChannelMute が担うので、ここでは
	// channelMutingRepo を渡さない (閲覧中チャンネルを巻き込まないため)。
	if viewer != nil && h.blockingRepo != nil {
		sets, err := notesfilter.LoadMuteBlockSets(viewer, h.userMutingRepo, h.blockingRepo, nil, h.userRepo)
		if err != nil {
			return apierr.JSONInternalError(c)
		}
		if notes, err = notesfilter.ApplyMuteBlockChannel(notes, sets, h.noteRepo); err != nil {
			return apierr.JSONInternalError(c)
		}
	}
	notes = notesfilter.ApplyHardMute(h.userRepo, viewer, notes)
	// **ブロック済みインスタンスのノートを落とす** (upstream
	// generateBlockedHostQueryForNote)。チャンネルは連合先からも投稿されるので、
	// ブロック後も既存のノートがタイムラインに出続けていた。
	blockedHosts, err := notesfilter.LoadBlockedHosts(h.metaRepo)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes = notesfilter.ApplyBlockedHosts(notes, blockedHosts)
	entities := entity.PackNotes(c.Request().Context(), notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldRes.Apply(entities, viewer)
	notehide.HideEmbeds(viewer, entities)
	out := make([]any, 0, len(entities))
	for _, pn := range entities {
		out = append(out, pn)
	}
	return c.JSON(http.StatusOK, out)
}

// noSuchChannelTimeline writes channels/timeline's NO_SUCH_CHANNEL error.
func noSuchChannelTimeline(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CHANNEL", "No such channel.", "4d0eeeba-a02c-4c3c-9966-ef60d38d2e7f"))
}

// channelToMap packs a channel into the misskey-compatible shape. bannerURL
// is the caller-resolved `bannerUrl` value (nil = no banner / unresolved);
// list callers batch-resolve it once to avoid an N+1 drive lookup (#1280).
func (h *Handler) channelToMap(ch *model.Channel, bannerURL any) map[string]any {
	// golden Channel の pinnedNoteIds は string[] 必須 (non-null)。model.StringArray
	// は nil のとき JSON null に marshal されるため、空でも [] を返すよう coalesce
	// する (#1280。Page content #1237 と同種の null-array drift)。
	pinnedNoteIDs := []string(ch.PinnedNoteIDs)
	if pinnedNoteIDs == nil {
		pinnedNoteIDs = []string{}
	}
	out := map[string]any{
		"id":                    ch.ID,
		"name":                  ch.Name,
		"description":           ch.Description,
		"userId":                ch.UserID,
		"bannerId":              ch.BannerID,
		"bannerUrl":             bannerURL,
		"pinnedNoteIds":         pinnedNoteIDs,
		"color":                 ch.Color,
		"isArchived":            ch.IsArchived,
		"notesCount":            ch.NotesCount,
		"usersCount":            ch.UsersCount,
		"isSensitive":           ch.IsSensitive,
		"allowRenoteToExternal": ch.AllowRenoteToExternal,
		"lastNotedAt":           nil,
	}
	// upstream ChannelEntityService は lastNotedAt を toISOString() で常に 3桁 ms
	// にする。createdAt と同様 .000Z に正規化する (#1770。以前は *time.Time を生で
	// 入れて Go 既定の RFC3339Nano になっていた)。nil は null のまま。
	if ch.LastNotedAt != nil {
		out["lastNotedAt"] = ch.LastNotedAt.UTC().Format("2006-01-02T15:04:05.000Z")
	}
	// Misskey TS は createdAt を channel.id (aidx) から導出する。golden Channel
	// でも createdAt は必須なので他 entity と同じ ISO ms 形式で埋める (#1280)。
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(ch.ID); err == nil {
			out["createdAt"] = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	return out
}

// channelBannerPublicURL mirrors upstream DriveFileEntityService.getPublicUrl
// for the non-proxy path (webpublicUrl ?? url, #1280), then wraps a remote
// origin through the media proxy to prevent viewer IP leakage (#1529).
// entity.ProxyMediaURL は context 未配線時 / local URL では raw を返すので
// 従来挙動を維持する。
func channelBannerPublicURL(f *model.DriveFile) string {
	raw := f.URL
	if f.WebpublicURL != nil && *f.WebpublicURL != "" {
		raw = *f.WebpublicURL
	}
	return entity.ProxyMediaURL(raw)
}

// resolveBannerURL resolves a single channel's bannerId to its public URL.
// Returns nil (= JSON null) when there is no banner or no resolver wired.
func (h *Handler) resolveBannerURL(bannerID *string) any {
	if bannerID == nil || *bannerID == "" || h.bannerResolver == nil {
		return nil
	}
	f, err := h.bannerResolver.FindByID(*bannerID)
	if err != nil || f == nil {
		return nil
	}
	return channelBannerPublicURL(f)
}

// resolveBannerURLs batch-resolves bannerUrl for a slice of channels in a
// single FindByIDs call, returning a channelID -> url map (#1280).
func (h *Handler) resolveBannerURLs(rows []*model.Channel) map[string]any {
	res := make(map[string]any, len(rows))
	if h.bannerResolver == nil {
		return res
	}
	ids := make([]string, 0, len(rows))
	for _, ch := range rows {
		if ch.BannerID != nil && *ch.BannerID != "" {
			ids = append(ids, *ch.BannerID)
		}
	}
	if len(ids) == 0 {
		return res
	}
	files, err := h.bannerResolver.FindByIDs(ids)
	if err != nil {
		return res
	}
	byID := make(map[string]*model.DriveFile, len(files))
	for _, f := range files {
		byID[f.ID] = f
	}
	for _, ch := range rows {
		if ch.BannerID != nil {
			if f := byID[*ch.BannerID]; f != nil {
				res[ch.ID] = channelBannerPublicURL(f)
			}
		}
	}
	return res
}

// channelToMapForViewer wraps channelToMap and embeds viewer-dependent
// fields (isFollowing / isFavorited) by issuing per-row Exists calls.
// Returns the bare channelToMap when viewer is nil (#522).
func (h *Handler) channelToMapForViewer(ch *model.Channel, viewer *model.User) map[string]any {
	// Misskey TS の `ChannelEntityService` は channelFollowings /
	// channelFavorites を join して埋めており、frontend (`channel.vue`) は
	// このフィールドを直接参照してフォローボタン状態を切り替える。欠落
	// すると常に未フォロー UI になる。
	out := h.channelToMap(ch, h.resolveBannerURL(ch.BannerID))
	if viewer == nil {
		return out
	}
	// upstream は me がある時 hasUnreadNote:false を後方互換で必ず付ける
	// (ChannelEntityService、#1540)。mk-go は未読判定を持たないため常に false。
	out["hasUnreadNote"] = false
	if h.followingRepo != nil {
		if ok, err := h.followingRepo.Exists(viewer.ID, ch.ID); err == nil {
			out["isFollowing"] = ok
		}
	}
	if h.favoriteRepo != nil {
		if ok, err := h.favoriteRepo.Exists(viewer.ID, ch.ID); err == nil {
			out["isFavorited"] = ok
		}
	}
	// upstream Channel schema は viewer 視点の isMuting を含む。
	if h.mutingRepo != nil {
		if ok, err := h.mutingRepo.Exists(viewer.ID, ch.ID); err == nil {
			out["isMuting"] = ok
		}
	}
	return out
}

// channelsToList serialises a slice of channels with viewer-dependent
// fields embedded via batch lookups (1 query each for following / favorite,
// not N+1). viewer が nil なら viewer-dependent フィールドは省略する。
func (h *Handler) channelsToList(rows []*model.Channel, viewer *model.User) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	banners := h.resolveBannerURLs(rows)
	if viewer == nil || (h.followingRepo == nil && h.favoriteRepo == nil && h.mutingRepo == nil) {
		for _, ch := range rows {
			out = append(out, h.channelToMap(ch, banners[ch.ID]))
		}
		return out
	}
	ids := make([]string, 0, len(rows))
	for _, ch := range rows {
		ids = append(ids, ch.ID)
	}
	var followed, favorited map[string]bool
	if h.followingRepo != nil {
		followed, _ = h.followingRepo.ExistsMany(viewer.ID, ids)
	}
	if h.favoriteRepo != nil {
		favorited, _ = h.favoriteRepo.ExistsMany(viewer.ID, ids)
	}
	// isMuting も list pack に乗せる (単一 show だけでなく search/featured/owned/
	// favorites でも upstream Channel schema の isMuting が出る)。muting repo に
	// ExistsMany が無いため ListByUser で viewer の muted set を一括取得する。
	var mutedSet map[string]bool
	if h.mutingRepo != nil {
		if mutings, err := h.mutingRepo.ListByUser(viewer.ID); err == nil {
			mutedSet = make(map[string]bool, len(mutings))
			for _, m := range mutings {
				mutedSet[m.ChannelID] = true
			}
		}
	}
	for _, ch := range rows {
		m := h.channelToMap(ch, banners[ch.ID])
		if followed != nil {
			m["isFollowing"] = followed[ch.ID]
		}
		if favorited != nil {
			m["isFavorited"] = favorited[ch.ID]
		}
		if mutedSet != nil {
			m["isMuting"] = mutedSet[ch.ID]
		}
		out = append(out, m)
	}
	return out
}

func alreadyFollowing(c echo.Context) error {
	// mk-go は upstream 2026.7.0 (#17802) に先行して ALREADY_FOLLOWING を実装
	// していたが id が独自だった。upstream が canonical id を定義したので揃える。
	return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_FOLLOWING", "You are already following that channel.", "7db31665-651e-40c1-8e6e-28e9ad829a2d"))
}

func notFollowing(c echo.Context) error {
	return c.JSON(http.StatusBadRequest, apierr.Error("NOT_FOLLOWING", "You are not following that channel.", "1f87a25b-7c72-4ed1-b3c3-bcaf57636a64"))
}
