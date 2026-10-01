// Package clips provides /api/clips/* endpoints.
package clips

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/notehide"
	"github.com/shiroha-a/mk/internal/api/pagination"
	coreclip "github.com/shiroha-a/mk/internal/core/clip"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/core/notesfilter"
	"github.com/shiroha-a/mk/internal/core/ugcvisibility"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Handler handles clip-related API endpoints.
type Handler struct {
	svc          *coreclip.Service
	idGen        id.Generator
	favoriteRepo ClipFavoriteRepository
	instanceRepo repository.InstanceRepository
	emojiRepo    repository.EmojiRepository
	bufReader    entity.BufferedReactionsReader
	fieldRes     *entity.NoteFieldResolver
	// userRepo は clips/notes 経由で他人の clip を閲覧する際の hardMutedWords
	// filter (#787) のために viewer profile を引く。未配線時は filter skip。
	userRepo repository.UserRepository
	// queryService は clips/add-note の visibility gate (#1456) で
	// RequireVisible を呼ぶための note.QueryService。未配線時は fail-closed
	// で 404 NO_SUCH_NOTE を返し、visibility 未検証の note を絶対に clip に
	// 永続化しない (#1455 favorites/create と同形)。
	queryService *corenote.QueryService
	// mutingRepo / blockingRepo は clips/notes の muted-user / blocked-user
	// filter (#1562、upstream generateMutedUserQueryForNotes 等) に使う。
	// 未配線時は該当次元の filter を skip する。
	mutingRepo   repository.MutingRepository
	blockingRepo repository.BlockingRepository
	// metaRepo は clips/notes の blocked-host filter (#1562、upstream
	// generateBlockedHostQueryForNote) で meta.blockedHosts を引く。
	metaRepo repository.MetaRepository
	// noteRepo は clips/notes の renote 入れ子 mute/block 検査 (#1630) で
	// renote 先行を batch fetch する。未配線時は入れ子検査 skip。
	noteRepo repository.NoteRepository
	// materializer はリレー由来で DB に無いノートを昇格させる (#2332)。
	materializer NoteMaterializer
	// ugcVisibilityFn は meta.ugcVisibilityForVisitor の live lookup。
	// clips/notes の匿名 visitor への gate に使う。
	ugcVisibilityFn func() string
}

// SetUGCVisibilityLookup wires a live lookup of meta.ugcVisibilityForVisitor.
//
// **毎回読む。** 起動時に焼き込むと、運営者が管理画面で締めても
// プロセスを再起動するまで反映されない。
func (h *Handler) SetUGCVisibilityLookup(fn func() string) {
	h.ugcVisibilityFn = fn
}

// HasUGCVisibility reports whether the visitor content visibility lookup was
// wired. 未配線だと clips/notes の匿名 visitor への gate が素通しになる。
// 起動時検査に使う。
func (h *Handler) HasUGCVisibility() bool { return h.ugcVisibilityFn != nil }

// visitorHidesAllNotes reports whether ugcVisibilityForVisitor = none hides
// every note from viewer. Mirrors upstream generateVisibilityQuery's
// `me == null && ugcVisibilityForVisitor === 'none'` → `1=0`.
func (h *Handler) visitorHidesAllNotes(viewer *model.User) bool {
	return viewer == nil && h.ugcVisibilityFn != nil && ugcvisibility.HidesAll(h.ugcVisibilityFn())
}

// SetNoteRepo wires a NoteRepository used by the clips/notes renote-nested
// mute/block check (#1630).
func (h *Handler) SetNoteRepo(r repository.NoteRepository) {
	h.noteRepo = r
}

// SetMuteBlockRepos wires the muting / blocking repositories used by the
// clips/notes muted-user / blocked-user filters (#1562).
func (h *Handler) SetMuteBlockRepos(m repository.MutingRepository, b repository.BlockingRepository) {
	h.mutingRepo = m
	h.blockingRepo = b
}

// SetMetaRepo wires a MetaRepository used by the clips/notes blocked-host
// filter (#1562).
func (h *Handler) SetMetaRepo(r repository.MetaRepository) {
	h.metaRepo = r
}

// blockedHosts returns meta.blockedHosts, or nil when the meta repo is not
// wired. Fetch error は fail-closed にするため caller へ返す (#1544 と同方針)。
func (h *Handler) blockedHosts() ([]string, error) {
	if h.metaRepo == nil {
		return nil, nil
	}
	meta, err := h.metaRepo.Fetch()
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, nil
	}
	return meta.BlockedHosts, nil
}

// SetUserRepo wires a UserRepository so clips/notes filters out notes that
// match the viewer's hardMutedWords (#787).
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetNoteFieldResolver wires the shared resolver that fills Files /
// MyReaction / Channel on packed notes (#426)。
func (h *Handler) SetNoteFieldResolver(r *entity.NoteFieldResolver) {
	h.fieldRes = r
}

// SetInstanceRepo attaches an InstanceRepository so clips/notes populates
// UserLite.Instance for remote users (#277).
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

// SetQueryService wires a note.QueryService used by AddNote to enforce
// note visibility (#1456). When unwired, AddNote fails closed with 404
// NO_SUCH_NOTE so that a clip never ends up referencing a note whose
// visibility the viewer is not entitled to.
func (h *Handler) SetQueryService(qs *corenote.QueryService) {
	h.queryService = qs
}

// NewHandler creates a new clips Handler.
func NewHandler(svc *coreclip.Service, idGen id.Generator) *Handler {
	return &Handler{svc: svc, idGen: idGen}
}

// CreateRequest is the request body for clips/create.
type CreateRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	IsPublic    bool    `json:"isPublic"`
}

// upstream paramDef の長さ制約 (clips/create.ts / update.ts)。ajv は UTF-16
// code unit 長だが、mk-go では他 endpoint と同じく rune 数で近似する (#1624 と
// 同方針。サロゲートペアを含む文字列は upstream より長く受理される)。
const (
	clipNameMaxLength        = 100
	clipDescriptionMaxLength = 2048
)

// validateClipParams checks name (1..100) / description (≤2048) length and
// normalizes an empty description to null (upstream `ps.description || null`)。
// name が nil の場合 (update の未指定) は検査しない。
func validateClipParams(name *string, description **string) bool {
	if name != nil {
		if n := len([]rune(*name)); n < 1 || n > clipNameMaxLength {
			return false
		}
	}
	if description != nil && *description != nil {
		if *(*description) == "" {
			// 空文字は null に正規化して保存する (#1562)
			*description = nil
		} else if len([]rune(**description)) > clipDescriptionMaxLength {
			return false
		}
	}
	return true
}

// Create handles POST /api/clips/create.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req CreateRequest
	if err := c.Bind(&req); err != nil || req.Name == "" {
		return apierr.JSONInvalidParam(c)
	}
	if !validateClipParams(&req.Name, &req.Description) {
		return apierr.JSONInvalidParam(c)
	}
	cl, err := h.svc.Create(coreclip.CreateInput{
		OwnerID:     user.ID,
		Name:        req.Name,
		Description: req.Description,
		IsPublic:    req.IsPublic,
	})
	if err != nil {
		if errors.Is(err, coreclip.ErrTooManyClips) {
			return apierr.JSONTooManyClips(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.clipToMap(cl, user))
}

// ShowRequest is the request body for clips/show.
type ShowRequest struct {
	ClipID string `json:"clipId"`
}

// Show handles POST /api/clips/show.
func (h *Handler) Show(c echo.Context) error {
	user := middleware.GetUser(c)
	var req ShowRequest
	if err := c.Bind(&req); err != nil || req.ClipID == "" {
		return apierr.JSONInvalidParam(c)
	}
	requesterID := ""
	if user != nil {
		requesterID = user.ID
	}
	// 非公開 clip を他人/匿名が見た場合も service が ErrClipNotFound を返す
	// ので、missing と同一の NO_SUCH_CLIP 404 になり存在が秘匿される (#1562、
	// upstream show.ts と同じ enumeration oracle 封じ)。
	cl, err := h.svc.Show(requesterID, req.ClipID)
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "c3c5fe33-d62c-44d2-9ea5-d997703f5c20"))
	}
	return c.JSON(http.StatusOK, h.clipToMap(cl, user))
}

// UpdateRequest is the request body for clips/update.
type UpdateRequest struct {
	ClipID string `json:"clipId"`
	// upstream paramDef の name は `{type:'string', minLength:1}` で nullable
	// ではない。`name: null` は型違反なので 400 にする必要があり、省略 (不変)
	// と区別するため RawMessage で受ける。
	Name        json.RawMessage `json:"name"`
	Description *string         `json:"description"`
	IsPublic    *bool           `json:"isPublic"`
}

// Update handles POST /api/clips/update.
func (h *Handler) Update(c echo.Context) error {
	user := middleware.GetUser(c)
	var req UpdateRequest
	if err := c.Bind(&req); err != nil || req.ClipID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream update.ts は `ps.description || null` を常に ClipService に渡す
	// ため、description 未指定 (undefined) でも NULL にクリアされる。mk-go も
	// 常に Description を更新対象にする (#1562)。
	// name: キー省略なら不変、null は型違反で 400、文字列なら更新。
	var name *string
	if len(req.Name) > 0 {
		if string(req.Name) == "null" {
			return apierr.JSONInvalidParam(c)
		}
		var v string
		if err := json.Unmarshal(req.Name, &v); err != nil {
			return apierr.JSONInvalidParam(c)
		}
		name = &v
	}
	desc := req.Description
	in := coreclip.UpdateInput{
		Name:        name,
		Description: &desc,
		IsPublic:    req.IsPublic,
	}
	// name 長 / description 長の検証と空 description の null 正規化 (#1562)
	if !validateClipParams(name, in.Description) {
		return apierr.JSONInvalidParam(c)
	}
	cl, err := h.svc.Update(user.ID, req.ClipID, in)
	if err != nil {
		switch {
		case errors.Is(err, coreclip.ErrClipNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "b4d92d70-b216-46fa-9a3f-a8c811699257"))
		case errors.Is(err, coreclip.ErrClipNameRequired):
			return apierr.JSONInvalidParam(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.clipToMap(cl, user))
}

// DeleteRequest is the request body for clips/delete.
type DeleteRequest struct {
	ClipID string `json:"clipId"`
}

// Delete handles POST /api/clips/delete.
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req DeleteRequest
	if err := c.Bind(&req); err != nil || req.ClipID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Delete(user.ID, req.ClipID); err != nil {
		switch {
		case errors.Is(err, coreclip.ErrClipNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "70ca08ba-6865-4630-b6fb-8494759aa754"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// ListRequest is the request body for clips/list.
type ListRequest struct {
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// List handles POST /api/clips/list.
//
// frontend Paginator は cursor mode (untilId / sinceId) で叩いてくる。
// upstream Misskey TS と同じく cursor 指定時は offset を無視する。
func (h *Handler) List(c echo.Context) error {
	user := middleware.GetUser(c)
	var req ListRequest
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
	out := make([]map[string]any, 0, len(rows))
	for _, cl := range rows {
		out = append(out, h.clipToMap(cl, user))
	}
	return c.JSON(http.StatusOK, out)
}

// AddNoteRequest is the request body for clips/add-note.
type AddNoteRequest struct {
	ClipID string `json:"clipId"`
	NoteID string `json:"noteId"`
}

// AddNote handles POST /api/clips/add-note.
func (h *Handler) AddNote(c echo.Context) error {
	user := middleware.GetUser(c)
	var req AddNoteRequest
	if err := c.Bind(&req); err != nil || req.ClipID == "" || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream ClipService.addNote は clip の所有者と件数上限だけを見て、note の
	// 可視性は検証しない。クリップは「あとで見る」リストで、可視性は読み取り時に
	// 評価される (mk-go も ListByClipVisible で push-down 済み #1418)。追加時に
	// 弾くと「今は見えないノートをクリップしておいて、フォロー後に読む」という
	// upstream で成立する使い方が壊れる。
	//
	// #1456 は「追加の可否で note の存在を探れる」ことを塞ぐ硬化だったが、
	// upstream 自身が「存在する ID → 204 / 存在しない ID → NO_SUCH_NOTE」で
	// 同じ oracle を持つので、ここだけ塞いでも実効が薄い。
	// リモート note が DB に無ければ取り込んでおく。取り込めなければ下の
	// AddNote が FK 相当のエラーで NO_SUCH_NOTE を返す。
	if h.queryService != nil {
		if _, verr := h.queryService.RequireVisible(user, req.NoteID); verr != nil {
			h.materializeIfMissing(req.NoteID, verr)
		}
	}
	if err := h.svc.AddNote(user.ID, req.ClipID, req.NoteID); err != nil {
		switch {
		case errors.Is(err, coreclip.ErrClipNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "d6e76cc0-a1b5-4c7c-a287-73fa9c716dcf"))
		case errors.Is(err, coreclip.ErrNoteNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "fc8c0b49-c7a3-4664-a0a6-b418d386bb8b"))
		case errors.Is(err, coreclip.ErrAlreadyClipped):
			return c.JSON(http.StatusBadRequest, apierr.Error("ALREADY_CLIPPED", "The note has already been clipped.", "734806c4-542c-463a-9311-15c512803965"))
		case errors.Is(err, coreclip.ErrTooManyClipNotes):
			return apierr.JSONTooManyClipNotes(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// RemoveNoteRequest is the request body for clips/remove-note.
type RemoveNoteRequest struct {
	ClipID string `json:"clipId"`
	NoteID string `json:"noteId"`
}

// RemoveNote handles POST /api/clips/remove-note.
func (h *Handler) RemoveNote(c echo.Context) error {
	user := middleware.GetUser(c)
	var req RemoveNoteRequest
	if err := c.Bind(&req); err != nil || req.ClipID == "" || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.RemoveNote(user.ID, req.ClipID, req.NoteID); err != nil {
		switch {
		case errors.Is(err, coreclip.ErrClipNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "b80525c6-97f7-49d7-a42d-ebccd49cfd52"))
		case errors.Is(err, coreclip.ErrNoteNotFound):
			// upstream 2026.10.0 の remove-note は、note が無い場合も clip に
			// 入っていない場合も NO_SUCH_NOTE を返す (4682d44cae)。
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "aff017de-190e-434b-893e-33a9ff5049d8"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// NotesRequest is the request body for clips/notes.
type NotesRequest struct {
	ClipID    string  `json:"clipId"`
	UntilID   string  `json:"untilId"`
	SinceID   string  `json:"sinceId"`
	SinceDate *int64  `json:"sinceDate"`
	UntilDate *int64  `json:"untilDate"`
	Limit     *int    `json:"limit"`
	Search    *string `json:"search"`
}

// clipNotesSearchMaxLength は upstream clips/notes paramDef の search
// maxLength。minLength は 1 (空文字は 400)。
const clipNotesSearchMaxLength = 100

// Notes handles POST /api/clips/notes.
func (h *Handler) Notes(c echo.Context) error {
	user := middleware.GetUser(c)
	var req NotesRequest
	if err := c.Bind(&req); err != nil || req.ClipID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// search は nullable (null = 未指定)。指定時は 1..100 を強制 (upstream
	// paramDef minLength 1 / maxLength 100、#1562)。
	search := ""
	if req.Search != nil {
		if n := len([]rune(*req.Search)); n < 1 || n > clipNotesSearchMaxLength {
			return apierr.JSONInvalidParam(c)
		}
		search = *req.Search
	}
	requesterID := ""
	if user != nil {
		requesterID = user.ID
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
	notes, err := h.svc.Notes(requesterID, req.ClipID, untilID, sinceID, limit, search)
	if err != nil {
		// 非公開 clip の他人/匿名閲覧は missing と同じ ErrClipNotFound →
		// NO_SUCH_CLIP 404 (存在秘匿、#1562)。
		if errors.Is(err, coreclip.ErrClipNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_CLIP", "No such clip.", "1d7645e6-2b6d-4635-b0fe-fe22b0e72e00"))
		}
		return apierr.JSONInternalError(c)
	}
	// upstream clips/notes.ts の generateVisibilityQuery は匿名 visitor かつ
	// ugcVisibilityForVisitor='none' のとき 1=0 になる。NO_SUCH_CLIP は query
	// より前に投げるので、clip の検査を通した後で空を返す。
	if h.visitorHidesAllNotes(user) {
		return c.JSON(http.StatusOK, []any{})
	}
	// visibility は clip service の ListByClipVisible で push down 済み (#1418
	// review)。muted-user / blocked-user / blocked-host / hardMutedWords を
	// post-fetch で適用する (#1562。antennas / roles の #1544 と同形のため
	// LIMIT 後適用 = ページ過少充填はあり得る)。channel mute は upstream
	// clips/notes が適用しないため渡さない。upstream が併せ持つ
	// muted-instances と renote 入れ子変種 (renote の reply/renote author) は
	// #1544 と同じく未対応の follow-up。
	mbSets, err := notesfilter.LoadMuteBlockSets(user, h.mutingRepo, h.blockingRepo, nil, h.userRepo)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes, err = notesfilter.ApplyMuteBlockChannel(notes, mbSets, h.noteRepo)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	blockedHosts, err := h.blockedHosts()
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes = notesfilter.ApplyBlockedHosts(notes, blockedHosts)
	notes = notesfilter.ApplyHardMute(h.userRepo, user, notes)
	entities := entity.PackNotes(c.Request().Context(), notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldRes.Apply(entities, user)
	notehide.HideEmbeds(user, entities)
	out := make([]any, 0, len(entities))
	for _, pn := range entities {
		out = append(out, pn)
	}
	return c.JSON(http.StatusOK, out)
}

// clipToMap packs a clip into the misskey_dart-compatible shape via
// entity.PackClip. owner (clip の所有ユーザー) を userRepo から解決して user
// field を埋める。misskey_dart の Clip.fromJson は createdAt / user /
// favoritedCount を非null必須とするため、旧 ad-hoc map では落ちていた (#1245)。
// viewer は favoritedCount / isFavorited / notesCount の出し分けに使う (#1562)。
func (h *Handler) clipToMap(cl *model.Clip, viewer *model.User) map[string]any {
	var owner *model.User
	if h.userRepo != nil {
		owner, _ = h.userRepo.FindByID(cl.UserID)
	}
	return entity.PackClip(cl, h.idGen, owner, h.clipExtras(cl, viewer))
}

// clipExtras resolves the viewer-dependent clip fields (#1562)。upstream
// ClipEntityService.pack 同様 favoritedCount は実カウント、isFavorited は
// 認証 viewer のみ、notesCount は owner 閲覧時のみ出す。favoriteRepo 未配線
// や lookup 失敗時は count 0 / isFavorited false に degrade する。
func (h *Handler) clipExtras(cl *model.Clip, viewer *model.User) entity.ClipExtras {
	extras := entity.ClipExtras{}
	// notesCount は owner 閲覧時のみ (upstream は他人には undefined を返し
	// note 件数を露出しない、#1562)。件数は clip_note の実カウント (#2243)。
	if viewer != nil && viewer.ID == cl.UserID && h.svc != nil {
		if n, err := h.svc.CountNotes(cl.ID); err == nil {
			extras.NotesCount = &n
		}
	}
	if viewer != nil {
		fav := false
		extras.IsFavorited = &fav
	}
	if h.favoriteRepo == nil {
		return extras
	}
	if n, err := h.favoriteRepo.CountByClip(cl.ID); err == nil {
		extras.FavoritedCount = n
	}
	if viewer != nil {
		if ok, err := h.favoriteRepo.Exists(viewer.ID, cl.ID); err == nil {
			extras.IsFavorited = &ok
		}
	}
	return extras
}

// NoteMaterializer promotes a relay-delivered note out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
//
// clip 追加は core/clip 側でも materialize するが、この handler の
// RequireVisible 事前チェックが先に 404 を返してしまうため、ここでも通す。
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

// PackForEmbed packs a clip for the anonymous embed page (#2389).
//
// viewer は nil 固定。isFavorited のような閲覧者依存フィールドは埋め込みでは
// 解決しない。
func (h *Handler) PackForEmbed(cl *model.Clip) map[string]any {
	return h.clipToMap(cl, nil)
}

// HasMetaRepo reports whether the meta repository was wired.
//
// 未配線だと blocked-host の note が post-fetch 経路で漏れる。起動時検査に
// 使う (#2708)。
func (h *Handler) HasMetaRepo() bool { return h.metaRepo != nil }

// HasMuteBlockRepos reports whether both mute/block repositories were wired.
//
// channelMuting は `LoadMuteBlockSets` へ意図的に nil を渡している
// (clips に channel 次元が無い) ので対象外。残る 2 つは nil が**空集合として
// 素通し**されるため、欠けるとその次元の filter が黙って消える。起動時検査に
// 使う (#2709 review)。
func (h *Handler) HasMuteBlockRepos() bool {
	return h.mutingRepo != nil && h.blockingRepo != nil
}
