// Package pages provides /api/pages/* endpoints.
package pages

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/pagination"
	corepage "github.com/elythia-network/elythia/internal/core/page"
	coreuser "github.com/elythia-network/elythia/internal/core/user"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// MainStreamPublisher emits events to a user's `main` WebSocket channel.
// Used here to publish `pageEvent` when /api/page-push is called so the
// page owner's UI can react to custom events emitted from the page script.
// Accepted as an interface to avoid importing internal/stream here
// (循環依存回避)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// UserBundleSource fetches a user + profile bundle. ShowByID is used to pack
// the caller for the `pageEvent` body emitted from /api/page-push and the
// page owner on /api/pages/show (#1134). ShowByUsername is used to resolve
// the upstream-compatible {username, name} shape on /api/pages/show (#955).
// FindManyByIDs is used by /api/pages/featured to batch-resolve multiple
// owners in one round-trip (#1134). Satisfied by *core/user.Service.
type UserBundleSource interface {
	ShowByID(id string) (*coreuser.UserWithProfile, error)
	ShowByUsername(username string, host *string) (*coreuser.UserWithProfile, error)
	FindManyByIDs(ids []string) ([]*model.User, error)
}

// Handler handles page-related API endpoints.
type Handler struct {
	// userPacker は pageEvent の利用者を本家 pack(me.id, {id: page.userId}) と同じく
	// ページの持ち主から見た形に組む (#3330)。
	userPacker          UserPacker
	svc                 *corepage.Service
	idGen               id.Generator
	mainStreamPublisher MainStreamPublisher
	userSource          UserBundleSource
	// driveFileRepo は page content の image block / eyeCatchingImageId から
	// drive file を解決して attachedFiles / eyeCatchingImage を埋めるのに使う
	// (#1662)。未配線なら両 field は PackPage の default (空配列 / null)。
	driveFileRepo repository.DriveFileRepository
}

// SetDriveFileRepo wires the drive file repo used to resolve a page's
// attachedFiles (image-block files) and eyeCatchingImage (#1662)。
func (h *Handler) SetDriveFileRepo(r repository.DriveFileRepository) {
	h.driveFileRepo = r
}

// NewHandler creates a new pages Handler. idGen is required so that
// responses include createdAt derived from the aidx-encoded page ID.
func NewHandler(svc *corepage.Service, idGen id.Generator) *Handler {
	return &Handler{svc: svc, idGen: idGen}
}

// SetMainStreamPublisher attaches a publisher used to emit `pageEvent` on
// /api/page-push. Optional — nil disables emit.
func (h *Handler) SetMainStreamPublisher(p MainStreamPublisher) {
	h.mainStreamPublisher = p
}

// SetUserSource attaches a user bundle fetcher used to pack the caller in
// `pageEvent` bodies. Optional — without it page-push returns 204 without
// emitting (caller info missing).
func (h *Handler) SetUserSource(s UserBundleSource) {
	h.userSource = s
}

// jsonAbsent reports whether a json.RawMessage field was omitted or sent as the
// literal `null` (= ajv の required 違反相当、#2027)。
func jsonAbsent(r json.RawMessage) bool {
	return len(r) == 0 || string(r) == "null"
}

// CreateRequest is the request body for pages/create. Content / Variables
// are stored verbatim into jsonb columns; we don't interpret them.
type CreateRequest struct {
	Title               string               `json:"title"`
	Name                string               `json:"name"`
	Summary             *string              `json:"summary"`
	Content             json.RawMessage      `json:"content"`
	Variables           json.RawMessage      `json:"variables"`
	Script              string               `json:"script"`
	EyeCatchingImageID  *string              `json:"eyeCatchingImageId"`
	Font                string               `json:"font"`
	AlignCenter         bool                 `json:"alignCenter"`
	HideTitleWhenPinned bool                 `json:"hideTitleWhenPinned"`
	Visibility          model.PageVisibility `json:"visibility"`
}

// Create handles POST /api/pages/create.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req CreateRequest
	if err := c.Bind(&req); err != nil || req.Title == "" || req.Name == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream pages/create.ts は required:['title','name','content','variables','script']。
	// content/variables の欠落 (omitted/null) を ajv 同様 400 で弾く (#2027。script は
	// Go の string で omitted と "" を区別できないため presence 強制は見送る)。
	if jsonAbsent(req.Content) || jsonAbsent(req.Variables) {
		return apierr.JSONInvalidParam(c)
	}
	// **jsonb 列へそのまま入る (#3037)。** PostgreSQL の jsonb は NUL エスケープを
	// 受け付けず SQLSTATE 22P05 でクエリごと落とすので、引く前に弾かないと
	// **任意の認証ユーザーが 500 を起こせる**。
	if !colfit.JSONStorable(req.Content) || !colfit.JSONStorable(req.Variables) {
		return apierr.JSONInvalidParam(c)
	}
	// **enum 列へそのまま入る。** `page_visibility_enum` に無い値を渡すと
	// INSERT が落ちて 500 になる (認証済みなら誰でも起こせた)。`visibility` は
	// mk-go 独自のフィールド (drop-in #367) なので upstream 側に検証が無い。
	if !validPageVisibility(req.Visibility) {
		return apierr.JSONInvalidParam(c)
	}
	if !h.eyeCatchingImageOK(req.EyeCatchingImageID, user.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "b7b97489-0f66-4b12-a5ff-b21bd63f6e1c"))
	}
	p, err := h.svc.Create(corepage.CreateInput{
		OwnerID:             user.ID,
		Title:               req.Title,
		Name:                req.Name,
		Summary:             req.Summary,
		AlignCenter:         req.AlignCenter,
		HideTitleWhenPinned: req.HideTitleWhenPinned,
		Font:                req.Font,
		EyeCatchingImageID:  req.EyeCatchingImageID,
		Content:             req.Content,
		Variables:           req.Variables,
		Script:              req.Script,
		Visibility:          req.Visibility,
	})
	if err != nil {
		if errors.Is(err, corepage.ErrPageNameConflict) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NAME_ALREADY_EXISTS", "The page name is already in use.", "4650348e-301c-499a-83c9-6aa988c66bc1"))
		}
		if errors.Is(err, corepage.ErrPageNameInvalid) {
			return apierr.JSONInvalidParam(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.pageToMap(p))
}

// eyeCatchingImageOK reports whether imageID (when non-nil / non-empty) refers
// to a drive file owned by userID. Returns true when no image is set or when
// the drive repo is unwired (test path). false means NO_SUCH_FILE (#1548).
func (h *Handler) eyeCatchingImageOK(imageID *string, userID string) bool {
	if imageID == nil || *imageID == "" {
		return true
	}
	if h.driveFileRepo == nil {
		return true
	}
	f, err := h.driveFileRepo.FindByID(*imageID)
	if err != nil || f == nil || f.UserID == nil || *f.UserID != userID {
		return false
	}
	return true
}

// ShowRequest is the request body for pages/show. upstream Misskey TS の
// paramDef は anyOf で 3 形を accept する: {pageId} / {userId, name} /
// {username, name} (#955)。frontend (page.vue) は 3 つ目の {username, name}
// を投げてくるので、本 struct も同 shape を受ける。
type ShowRequest struct {
	PageID   string `json:"pageId"`
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// Show handles POST /api/pages/show.
func (h *Handler) Show(c echo.Context) error {
	user := middleware.GetUser(c)
	var req ShowRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	requesterID := ""
	if user != nil {
		requesterID = user.ID
	}
	var (
		p   *model.Page
		err error
	)
	switch {
	case req.PageID != "":
		p, err = h.svc.Show(requesterID, req.PageID)
	case req.UserID != "" && req.Name != "":
		p, err = h.svc.ShowByName(requesterID, req.UserID, req.Name)
	case req.Username != "" && req.Name != "":
		// {username, name} 経路 (#955): username で local user を引いて
		// その user.id + name で page を引く。upstream は host=NULL の
		// local user 縛り (= remote の page は連合経路で配信されない)。
		if h.userSource == nil {
			return apierr.JSONInternalError(c)
		}
		bundle, ulerr := h.userSource.ShowByUsername(req.Username, nil)
		if ulerr != nil || bundle == nil || bundle.User == nil {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "222120c0-3ead-4528-811b-b96f233388d7"))
		}
		p, err = h.svc.ShowByName(requesterID, bundle.User.ID, req.Name)
	default:
		return apierr.JSONInvalidParam(c)
	}
	if err != nil {
		// **DB 障害は 500** (#2792)。service は not-found と access denied だけを
		// 専用の error にするので、それ以外はここで 500 に落とす。
		if !errors.Is(err, corepage.ErrPageNotFound) && !errors.Is(err, corepage.ErrAccessDenied) {
			return apierr.JSONInternalError(c)
		}
		// 非 public page を非許可 viewer が引いた場合 (ErrAccessDenied) も存在ごと
		// 隠して NO_SUCH_PAGE (404) を返す。upstream TS pages/show は可視性ゲートを
		// 持たず noSuchPage のみ返すため shape が一致し、private page の存在を 403 で
		// 露呈しない (#1432)。update/delete は TS に accessDenied があるため 403 のまま。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "222120c0-3ead-4528-811b-b96f233388d7"))
	}
	// owner / isLiked を attach (#1134)。owner lookup miss は frontend page.vue
	// の `v-if="page.user"` で吸収されるため fail-soft で nil 維持。
	// isLiked は logged-in viewer のときだけ pointer で渡し、guest 経路では
	// upstream 同様 field を omit する。
	var owner *model.User
	if h.userSource != nil {
		if bundle, oerr := h.userSource.ShowByID(p.UserID); oerr == nil && bundle != nil {
			owner = bundle.User
		}
	}
	var isLikedPtr *bool
	if user != nil {
		liked := h.svc.HasLiked(user.ID, p.ID)
		isLikedPtr = &liked
	}
	return c.JSON(http.StatusOK, h.pageToMapWithOwner(p, owner, isLikedPtr))
}

// UpdateRequest is the request body for pages/update.
type UpdateRequest struct {
	PageID              string                `json:"pageId"`
	Title               *string               `json:"title"`
	Name                *string               `json:"name"`
	Summary             *string               `json:"summary"`
	Content             json.RawMessage       `json:"content"`
	Variables           json.RawMessage       `json:"variables"`
	Script              *string               `json:"script"`
	EyeCatchingImageID  *string               `json:"eyeCatchingImageId"`
	Font                *string               `json:"font"`
	AlignCenter         *bool                 `json:"alignCenter"`
	HideTitleWhenPinned *bool                 `json:"hideTitleWhenPinned"`
	Visibility          *model.PageVisibility `json:"visibility"`
}

// Update handles POST /api/pages/update.
func (h *Handler) Update(c echo.Context) error {
	user := middleware.GetUser(c)
	var req UpdateRequest
	if err := c.Bind(&req); err != nil || req.PageID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// **jsonb 列へそのまま入る (#3037)。** create 側と同じ理由。
	if !colfit.JSONStorable(req.Content) || !colfit.JSONStorable(req.Variables) {
		return apierr.JSONInvalidParam(c)
	}
	// enum 列の検証も create と揃える (指定されたときだけ)。
	//
	// **update では空文字を弾く。** create は値型なので「省略」と「空文字」を
	// 区別できず `validPageVisibility` が空文字を通すが (service が `public` へ
	// 正規化する)、update は `*model.PageVisibility` なので省略は nil で表せる。
	// 空文字を通すと `fields["visibility"] = ""` がそのまま enum 列へ行き、
	// **認証済みの一般利用者が 500 を起こせる**
	// (`invalid input value for enum page_visibility_enum: ""`)。
	if req.Visibility != nil && (*req.Visibility == "" || !validPageVisibility(*req.Visibility)) {
		return apierr.JSONInvalidParam(c)
	}
	// upstream update.ts: eyeCatchingImageId 指定時は自分の drive file か検証し、
	// 不在なら NO_SUCH_FILE (#1548)。
	if !h.eyeCatchingImageOK(req.EyeCatchingImageID, user.ID) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_FILE", "No such file.", "cfc23c7c-3887-490e-af30-0ed576703c82"))
	}
	in := corepage.UpdateInput{
		Title:               req.Title,
		Name:                req.Name,
		AlignCenter:         req.AlignCenter,
		HideTitleWhenPinned: req.HideTitleWhenPinned,
		Font:                req.Font,
		Script:              req.Script,
		Visibility:          req.Visibility,
	}
	if req.Summary != nil {
		s := req.Summary
		in.Summary = &s
	}
	if req.EyeCatchingImageID != nil {
		s := req.EyeCatchingImageID
		in.EyeCatchingImageID = &s
	}
	if req.Content != nil {
		in.Content = req.Content
	}
	if req.Variables != nil {
		in.Variables = req.Variables
	}
	p, err := h.svc.Update(user.ID, req.PageID, in)
	if err != nil {
		switch {
		case errors.Is(err, corepage.ErrPageNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "21149b9e-3616-4778-9592-c4ce89f5a864"))
		// upstream TS pages/update は accessDenied (UUID 3c15cd52) を持つため、
		// show (存在隠蔽 404) とは異なり 403 を維持する (#1432)。
		case errors.Is(err, corepage.ErrAccessDenied):
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "3c15cd52-3b4b-4274-967d-6456fc4f792b"))
		case errors.Is(err, corepage.ErrPageNameRequired),
			errors.Is(err, corepage.ErrPageTitleRequired),
			errors.Is(err, corepage.ErrPageNameInvalid):
			return apierr.JSONInvalidParam(c)
		case errors.Is(err, corepage.ErrPageNameConflict):
			return c.JSON(http.StatusBadRequest, apierr.Error("NAME_ALREADY_EXISTS", "The page name is already in use.", "2298a392-d4a1-44c5-9ebb-ac1aeaa5a9ab"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.pageToMap(p))
}

// DeleteRequest is the request body for pages/delete.
type DeleteRequest struct {
	PageID string `json:"pageId"`
}

// Delete handles POST /api/pages/delete.
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req DeleteRequest
	if err := c.Bind(&req); err != nil || req.PageID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Delete(user.ID, req.PageID); err != nil {
		switch {
		case errors.Is(err, corepage.ErrPageNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "eb0c6e1d-d519-4764-9486-52a7e1c6392a"))
		// upstream TS pages/delete は accessDenied (UUID 8b741b3e) を持つため、
		// show (存在隠蔽 404) とは異なり 403 を維持する (#1432)。
		case errors.Is(err, corepage.ErrAccessDenied):
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "Access denied.", "8b741b3e-2c22-44b3-a15f-29949aa1601e"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// MyRequest is the request body for i/pages (own pages list).
type MyRequest struct {
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// My handles POST /api/i/pages.
//
// frontend Paginator (cursor mode) は untilId / sinceId を forward する (#493)。
// 全 row が同一 owner なので middleware.GetUser を直接 owner として attach し、
// 追加 round-trip 無しで `page.user` を埋める (#1134)。
func (h *Handler) My(c echo.Context) error {
	user := middleware.GetUser(c)
	var req MyRequest
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
	rows, err := h.svc.ListByUser(user.ID, sinceID, untilID, limit, req.Offset)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.pagesToList(rows, func(string) *model.User { return user }))
}

// featuredPageLimit is the fixed page count pages/featured returns, matching
// upstream's `.limit(10)` (featured.ts)。
const featuredPageLimit = 10

// Featured handles POST /api/pages/featured.
//
// 複数 owner なので FindManyByIDs で 1 round-trip batch 取得し、行毎の owner を
// pagesToList に owner closure で渡す (#1134)。userSource が未配線の test 経路
// では owner 取得不能なので空 list を返す (= silent fail-soft、501 にしない)。
func (h *Handler) Featured(c echo.Context) error {
	// upstream pages/featured は paramDef 空。limit/offset/sinceId/untilId を一切
	// 受け取らず、常に visibility=public かつ likedCount>0 の page を likedCount
	// DESC で固定 10 件返す (#1830)。REST 経由で順序・件数を変えさせない。
	rows, err := h.svc.Featured("", "", featuredPageLimit, 0)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	ownerLookup := h.batchOwnerLookup(rows)
	// upstream pages/featured.ts は packMany(pages, me) を me 付きで呼び、認証
	// viewer には各 page の isLiked を出す (i/pages・users/pages は me 無しで omit)。
	// mk-go も viewer を渡して isLiked を埋める (#1773)。
	return c.JSON(http.StatusOK, h.pagesToListForViewer(rows, ownerLookup, middleware.GetUser(c)))
}

// batchOwnerLookup returns a closure that resolves page.userId → *model.User
// in O(1) per call by pre-loading all unique owner IDs in one query. Caller
// guarantees `rows` may contain duplicates; the lookup map dedupes via the
// userId key. When userSource is unset or the batch fetch errors, the
// closure returns nil for every ID and the caller (pagesToList) drops the
// row — same fail-soft behavior as upstream PageEntityService.packMany
// which skips unpackable rows rather than failing the whole list.
func (h *Handler) batchOwnerLookup(rows []*model.Page) func(string) *model.User {
	if h.userSource == nil || len(rows) == 0 {
		return func(string) *model.User { return nil }
	}
	seen := make(map[string]struct{}, len(rows))
	ids := make([]string, 0, len(rows))
	for _, p := range rows {
		if _, ok := seen[p.UserID]; ok {
			continue
		}
		seen[p.UserID] = struct{}{}
		ids = append(ids, p.UserID)
	}
	users, err := h.userSource.FindManyByIDs(ids)
	if err != nil {
		return func(string) *model.User { return nil }
	}
	byID := make(map[string]*model.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	return func(uid string) *model.User { return byID[uid] }
}

// LikeRequest is the request body for pages/like and pages/unlike.
type LikeRequest struct {
	PageID string `json:"pageId"`
}

// Like handles POST /api/pages/like.
func (h *Handler) Like(c echo.Context) error {
	user := middleware.GetUser(c)
	var req LikeRequest
	if err := c.Bind(&req); err != nil || req.PageID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Like(user.ID, req.PageID); err != nil {
		switch {
		// upstream TS pages/like は accessDenied を宣言しておらず可視性ゲート
		// 自体を持たない (noSuchPage / yourPage / alreadyLiked のみ)。mk-go は
		// Page.visibility (drop-in #367) を持つため core service にゲートを残す
		// が、ErrAccessDenied は show (#1434) と同じく 404 NO_SUCH_PAGE で隠して
		// private page の存在を 403 で露呈しないようにする (#1435)。
		case errors.Is(err, corepage.ErrPageNotFound),
			errors.Is(err, corepage.ErrAccessDenied):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "cc98a8a2-0dc3-4123-b198-62c71df18ed3"))
		case errors.Is(err, corepage.ErrYourPage):
			// 自分の page への like は upstream TS と同じく YOUR_PAGE で弾く (#1438)。
			return c.JSON(http.StatusBadRequest, apierr.Error("YOUR_PAGE", "You cannot like your own page.", "28800466-e6db-40f2-8fae-bf9e82aa92b8"))
		case errors.Is(err, corepage.ErrAlreadyLiked):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_LIKED", "You already liked that page.", "d4c1edbe-7da2-4eae-8714-1acfd2d63941"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// PagePushRequest is the request body for /api/page-push.
type PagePushRequest struct {
	PageID string          `json:"pageId"`
	Event  string          `json:"event"`
	Var    json.RawMessage `json:"var"`
}

// PagePush handles POST /api/page-push. Triggered by page scripts to emit
// a custom event to the page owner's `main` WebSocket channel. Body shape
// mirrors the Misskey original endpoints/page-push.ts
// (pageId / event / var / userId / user).
func (h *Handler) PagePush(c echo.Context) error {
	caller := middleware.GetUser(c)
	var req PagePushRequest
	if err := c.Bind(&req); err != nil || req.PageID == "" || req.Event == "" {
		return apierr.JSONInvalidParam(c)
	}
	// TS本家はfindOneByのみで可視性チェックをしないのでFindByIDを
	// 使って合わせる。見つからなければ404に丸め、存在するIDだけに
	// emitする。
	p, err := h.svc.FindByID(req.PageID)
	// **service の sentinel を見る。** page.Service は not-found を
	// ErrPageNotFound に置き換えるので、repository.IsNotFound では判定できない。
	if err != nil && !errors.Is(err, corepage.ErrPageNotFound) {
		// **DB 障害を not-found に丸めない** (#2792)。
		return apierr.JSONInternalError(c)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "4a13ad31-6729-46b4-b9af-e86b265c2e74"))
	}
	if h.mainStreamPublisher == nil || h.userSource == nil {
		// 配線未完了ならemitせず204を返す(API互換のため)。
		return c.NoContent(http.StatusNoContent)
	}
	bundle, err := h.userSource.ShowByID(caller.ID)
	if err != nil || bundle == nil || bundle.User == nil {
		// bundle.Userは現実装のShowByIDでは常にnon-nilだが、interface
		// 経由で将来実装が変わる可能性に備えて防御的にcheck。
		return c.NoContent(http.StatusNoContent)
	}
	user, ok := h.packPusher(c.Request().Context(), bundle, p.UserID)
	if !ok {
		// 本家は profile を findOneByOrFail で読むので、読めなければ例外で送らない。
		return apierr.JSONInternalError(c)
	}
	body := map[string]any{
		"pageId": p.ID,
		"event":  req.Event,
		"var":    rawJSONBytes(req.Var),
		"userId": caller.ID,
		"user":   user,
	}
	h.mainStreamPublisher.PublishMainEvent(p.UserID, "pageEvent", body)
	return c.NoContent(http.StatusNoContent)
}

// rawJSONBytes returns b as json.RawMessage, or nil when empty. Empty
// bytes are mapped to nil so that json.Marshal serialises the field as
// null — matching the TS original's behaviour of sending `undefined`
// when the optional `var` parameter is omitted.
func rawJSONBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(b)
}

// Unlike handles POST /api/pages/unlike.
func (h *Handler) Unlike(c echo.Context) error {
	user := middleware.GetUser(c)
	var req LikeRequest
	if err := c.Bind(&req); err != nil || req.PageID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Unlike(user.ID, req.PageID); err != nil {
		switch {
		case errors.Is(err, corepage.ErrPageNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_PAGE", "No such page.", "a0d41e20-1993-40bd-890e-f6e560ae648e"))
		case errors.Is(err, corepage.ErrNotLiked):
			return c.JSON(http.StatusBadRequest, apierr.Error("NOT_LIKED", "You have not liked that page.", "f5e586b0-ce93-4050-b0e3-7f31af5259ee"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// pagesToList packs a slice of pages using h.idGen so every element gets a
// consistent createdAt alongside the other fields. ownerLookup resolves the
// page owner per row; when it returns nil for a row the row is dropped
// (otherwise the frontend MkPagePreview template throws on
// `page.user.username` and the entire list disappears, #1134).
func (h *Handler) pagesToList(rows []*model.Page, ownerLookup func(userID string) *model.User) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		owner := ownerLookup(p.UserID)
		if owner == nil {
			continue
		}
		out = append(out, h.pageToMapWithOwner(p, owner, nil))
	}
	return out
}

// pagesToListForViewer packs a slice of pages like pagesToList but additionally
// fills each row's `isLiked` from the viewer's like state (batch-loaded in one
// query). When viewer is nil the field is omitted, matching upstream
// packMany(pages, me) where `isLiked` is `meId ? exists(...) : undefined`
// (pages/featured passes me; i/pages and users/pages do not). Rows whose owner
// lookup misses are dropped, same as pagesToList (#1773 / #1134)。
func (h *Handler) pagesToListForViewer(rows []*model.Page, ownerLookup func(userID string) *model.User, viewer *model.User) []map[string]any {
	if viewer == nil {
		return h.pagesToList(rows, ownerLookup)
	}
	pageIDs := make([]string, 0, len(rows))
	for _, p := range rows {
		pageIDs = append(pageIDs, p.ID)
	}
	likedSet, _ := h.svc.LikedPageIDs(viewer.ID, pageIDs)
	out := make([]map[string]any, 0, len(rows))
	for _, p := range rows {
		owner := ownerLookup(p.UserID)
		if owner == nil {
			continue
		}
		liked := likedSet[p.ID]
		out = append(out, h.pageToMapWithOwner(p, owner, &liked))
	}
	return out
}

// pageToMap delegates to entity.PackPage so the JSON shape (timestamp
// format, field set) stays in sync between /api/pages/* and other places
// that embed a page (e.g. pinnedPage on /api/i and /api/users/show).
//
// Use pageToMapWithOwner for list endpoints / detail views — they need
// `page.user` populated or the frontend templates fail to render (#1134).
// This bare variant remains for pinned-page embeds where the user context
// is implicit (the embedding endpoint already returns the user separately).
func (h *Handler) pageToMap(p *model.Page) map[string]any {
	return entity.PackPage(p, h.idGen)
}

// pageToMapWithOwner enriches the page entity with `user` (UserLite) and
// optional `eyeCatchingImage` / `isLiked`. Upstream PageEntityService.pack
// always emits `user`; mk-go previously omitted it which silently broke the
// frontend list views (#1134).
func (h *Handler) pageToMapWithOwner(p *model.Page, owner *model.User, isLiked *bool) map[string]any {
	// content の image block / eyeCatchingImageId から drive file を解決する
	// (#1662)。driveFileRepo 未配線なら entity 側が (nil,nil) を返し default 維持。
	// list 経路 (i/pages, featured) では page ごとに drive lookup が走る (N+1)
	// が、page list は通常小さいので許容する。
	attachedFiles, eyeCatchingImage := entity.ResolvePageDriveFiles(p, h.driveFileRepo, h.idGen)
	return entity.PackPageWithContext(p, entity.PackPageContext{
		IDGen:            h.idGen,
		Owner:            owner,
		EyeCatchingImage: eyeCatchingImage,
		AttachedFiles:    attachedFiles,
		IsLiked:          isLiked,
	})
}

// HasDriveFileRepo reports whether the drive file repository was wired.
//
// 未配線だと eyecatching image の所有者チェックが素通しになり、他人の drive
// ファイルを自分の page に貼れる (IDOR)。起動時検査に使う (#2683)。
func (h *Handler) HasDriveFileRepo() bool { return h.driveFileRepo != nil }

// validPageVisibility reports whether v is one of the values the
// `page_visibility_enum` column accepts.
//
// **空文字は許す。** `visibility` は省略可能で、その場合は列の既定
// (`public`) が入る。Go の string では省略と空文字を区別できない。
func validPageVisibility(v model.PageVisibility) bool {
	switch v {
	case "", model.PageVisibilityPublic, model.PageVisibilityFollowers, model.PageVisibilitySpecified:
		return true
	}
	return false
}
