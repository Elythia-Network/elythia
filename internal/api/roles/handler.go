package roles

import (
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// RoleNotesQuery provides a way to fetch notes by role.
type RoleNotesQuery interface {
	ListByRole(roleID string, limit int, sinceID, untilID string) ([]*model.Note, error)
}

// Handler handles public role API endpoints.
type Handler struct {
	roleService  *role.Service
	notesQuery   RoleNotesQuery
	noteRepo     repository.NoteRepository
	idGen        id.Generator
	instanceRepo repository.InstanceRepository
	emojiRepo    repository.EmojiRepository
	bufReader    entity.BufferedReactionsReader
	fieldRes     *entity.NoteFieldResolver
	// userRepo は roles/notes の hardMutedWords filter (#787)。
	userRepo repository.UserRepository
	// mutingRepo / blockingRepo / channelMutingRepo は roles/notes で viewer が
	// mute/block した user の note と mute した channel の note を除外する (#1544)。
	// upstream notes.ts の generateBaseNoteFilteringQuery + channelMuting に対応。
	// 未配線時は該当 dimension の filter skip。
	mutingRepo        repository.MutingRepository
	blockingRepo      repository.BlockingRepository
	channelMutingRepo repository.ChannelMutingRepository
	// packer は roles/users の embed user を本家 packMany(users, me) と同じ形
	// (関係・カウントのゲート・モデレーター向けの項目・ピン留め・移行先・instance・
	// 絵文字) に組む (#1973、#3330)。未配線なら素の UserDetailed を返す。
	packer userpack.ListPacker
	// metaRepo は roles/notes の blocked-host filter で meta.blockedHosts を
	// 引く (upstream generateBlockedHostQueryForNote)。
	metaRepo repository.MetaRepository
}

// HasMetaRepo reports whether the blocked-host filter can read meta.
//
// 未配線だと `blockedHosts` の除外が黙って no-op になる (ブロックしたはずの
// インスタンスのノートが一覧に出続ける)。起動時の critical wiring 検査で落とす。
func (h *Handler) HasMetaRepo() bool {
	return h != nil && h.metaRepo != nil
}

// SetMetaRepo wires a MetaRepository used for the blocked-host filter on
// roles/notes (upstream generateBlockedHostQueryForNote)。
func (h *Handler) SetMetaRepo(r repository.MetaRepository) {
	h.metaRepo = r
}

// SetListPacker wires the packer of the users embedded in roles/users
// (#1973, #3330).
func (h *Handler) SetListPacker(p userpack.ListPacker) {
	h.packer = p
}

// HasListPacker reports whether the list packer was wired.
//
// 未配線だと roles/users の利用者に関係・ピン留め・移行先・instance・絵文字が
// 載らない。起動時検査に使う。
func (h *Handler) HasListPacker() bool { return h.packer != nil }

// SetUserRepo wires a UserRepository so roles/notes filters out notes that
// match the viewer's hardMutedWords (#787).
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetMuteBlockRepos wires the muting / blocking / channel-muting repositories so
// roles/notes excludes notes from users the viewer muted or who blocked the
// viewer, plus notes in channels the viewer muted (#1544). Unwired repos simply
// disable that filter dimension.
func (h *Handler) SetMuteBlockRepos(
	muting repository.MutingRepository,
	blocking repository.BlockingRepository,
	channelMuting repository.ChannelMutingRepository,
) {
	h.mutingRepo = muting
	h.blockingRepo = blocking
	h.channelMutingRepo = channelMuting
}

// SetNoteFieldResolver wires the shared resolver that fills Files /
// MyReaction / Channel on packed notes (#426)。
func (h *Handler) SetNoteFieldResolver(r *entity.NoteFieldResolver) {
	h.fieldRes = r
}

// NewHandler creates a new roles Handler. idGen is required for note packing
// under /api/roles/notes.
func NewHandler(roleService *role.Service, idGen id.Generator) *Handler {
	return &Handler{roleService: roleService, idGen: idGen}
}

// SetNotesQuery attaches a RoleNotesQuery for the roles/notes endpoint.
func (h *Handler) SetNotesQuery(q RoleNotesQuery) {
	h.notesQuery = q
}

// SetNoteRepo wires a NoteRepository used by the roles/notes renote-nested
// mute/block check (#1630).
func (h *Handler) SetNoteRepo(r repository.NoteRepository) {
	h.noteRepo = r
}

// SetInstanceRepo attaches an InstanceRepository so roles/notes populates
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

// List handles POST /api/roles/list.
func (h *Handler) List(c echo.Context) error {
	roles, err := h.roleService.List()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	var result []any
	for _, r := range roles {
		// upstream roles/list は isPublic AND isExplorable の 2 条件で絞る。
		if r.IsPublic && r.IsExplorable {
			result = append(result, h.packRole(r))
		}
	}
	if result == nil {
		result = []any{}
	}
	return c.JSON(http.StatusOK, result)
}

// Show handles POST /api/roles/show.
func (h *Handler) Show(c echo.Context) error {
	var req struct {
		RoleID string `json:"roleId"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	r, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "de5502bf-009a-4639-86c1-fec349e46dcb"))
		}
		return apierr.JSONInternalError(c)
	}
	if !r.IsPublic {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "de5502bf-009a-4639-86c1-fec349e46dcb"))
	}
	return c.JSON(http.StatusOK, h.packRole(r))
}

// Users handles POST /api/roles/users.
//
// upstream users.ts: isPublic かつ isExplorable な role の roleAssignments を
// 期限有効分だけ取得し id cursor でページングして [{id, user:UserDetailed}] を
// 返す。!isPublic / !isExplorable は NO_SUCH_ROLE 扱い (#1544)。
func (h *Handler) Users(c echo.Context) error {
	var req struct {
		RoleID    string `json:"roleId"`
		Limit     int    `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	r, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "30aaaee3-4792-48dc-ab0d-cf501a575ac5"))
		}
		return apierr.JSONInternalError(c)
	}
	if !r.IsPublic || !r.IsExplorable {
		// upstream は findOneBy({id, isPublic:true, isExplorable:true}) が null
		// なら NO_SUCH_ROLE。!isPublic / !isExplorable も同じ扱い。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "30aaaee3-4792-48dc-ab0d-cf501a575ac5"))
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// sinceDate / untilDate を aidx prefix に正規化 (Notes と同 pattern、#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	assigns, err := h.roleService.ListByRole(req.RoleID, untilID, sinceID, limit)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "30aaaee3-4792-48dc-ab0d-cf501a575ac5"))
		}
		return apierr.JSONInternalError(c)
	}

	// reporter/assignee と同様に user_profile を 1 batch で解決して UserDetailed を
	// pack する (N+1 回避)。userRepo 未配線時は profile なしで pack する。
	viewer := middleware.GetUser(c)
	profByID := h.assignmentProfiles(assigns)
	kept := make([]*model.RoleAssignment, 0, len(assigns))
	users := make([]*model.User, 0, len(assigns))
	for _, a := range assigns {
		if a.User == nil {
			// preload 失敗 (user 削除等) は upstream では userId fallback だが、
			// UserDetailed を組めないため skip する (defensive)。
			continue
		}
		kept = append(kept, a)
		users = append(users, a.User)
	}
	ctx := c.Request().Context()
	var packed []entity.UserDetailed
	if h.packer != nil {
		// 本家は packMany(users, me, {schema: 'UserDetailed'})。関係 (#1973)・カウントの
		// ゲート (#1558)・モデレーター向けの項目・ピン留め・移行先・instance・絵文字を
		// まとめて埋める (#3330)。
		packed = h.packer.DetailedMany(ctx, viewer, users, profByID)
	} else {
		// 配線が外れた構成では素の UserDetailed (packer の既定で非公開のカウントは伏せる)。
		packed = make([]entity.UserDetailed, len(users))
		for i, u := range users {
			packed[i] = entity.PackUserDetailed(u, profByID[u.ID], h.idGen)
		}
	}
	out := make([]any, 0, len(kept))
	for i, a := range kept {
		out = append(out, map[string]any{
			"id": a.ID,
			// 本家の pack は isMe なら MeDetailed を返す。
			"user": meself.Pack(ctx, packed[i], a.User, profByID[a.UserID], viewer),
		})
	}
	return c.JSON(http.StatusOK, out)
}

// assignmentProfiles batch-fetches the user_profile rows of the assignees so
// roles/users can pack UserDetailed without N+1 (#1544)。userRepo 未配線時は nil。
func (h *Handler) assignmentProfiles(assigns []*model.RoleAssignment) map[string]*model.UserProfile {
	if h.userRepo == nil {
		return nil
	}
	ids := make([]string, 0, len(assigns))
	seen := map[string]struct{}{}
	for _, a := range assigns {
		if a.User == nil {
			continue
		}
		if _, ok := seen[a.UserID]; ok {
			continue
		}
		seen[a.UserID] = struct{}{}
		ids = append(ids, a.UserID)
	}
	if len(ids) == 0 {
		return nil
	}
	profiles, err := h.userRepo.FindProfilesByUserIDs(ids)
	if err != nil {
		return nil
	}
	byID := make(map[string]*model.UserProfile, len(profiles))
	for _, p := range profiles {
		byID[p.UserID] = p
	}
	return byID
}

// Notes handles POST /api/roles/notes.
func (h *Handler) Notes(c echo.Context) error {
	var req struct {
		RoleID    string `json:"roleId"`
		Limit     int    `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil || req.RoleID == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "roleId is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}

	r, err := h.roleService.Show(req.RoleID)
	if err != nil {
		if errors.Is(err, role.ErrRoleNotFound) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "eb70323a-df61-4dd4-ad90-89c83c7cf26e"))
		}
		return apierr.JSONInternalError(c)
	}
	if !r.IsPublic {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ROLE", "No such role.", "eb70323a-df61-4dd4-ad90-89c83c7cf26e"))
	}
	// upstream notes.ts: role が public でも !isExplorable なら空配列を返す
	// (role の note を explore させない policy)。このガードが無いと非 explorable
	// role の note 一覧が漏れる (#1544)。
	if !r.IsExplorable {
		return c.JSON(http.StatusOK, []any{})
	}

	if h.notesQuery == nil {
		return c.JSON(http.StatusOK, []any{})
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	// sinceDate / untilDate を aidx prefix に正規化 (#1173)。
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	notes, err := h.notesQuery.ListByRole(req.RoleID, limit, sinceID, untilID)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	viewer := middleware.GetUser(c)
	// mute/block/channel-mute/instance-mute filter (#1544 / #1630): upstream
	// notes.ts の generateBaseNoteFilteringQuery + channelMuting に相当。set の
	// ロードに失敗したら fail-closed で 500 を返す (security 項目なので
	// silently leak しない)。
	mbSets, err := notesfilter.LoadMuteBlockSets(viewer, h.mutingRepo, h.blockingRepo, h.channelMutingRepo, h.userRepo)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	notes, err = notesfilter.ApplyMuteBlockChannel(notes, mbSets, h.noteRepo)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	notes = notesfilter.ApplyHardMute(h.userRepo, viewer, notes)
	// **ブロック済みインスタンスのノートを落とす** (upstream
	// generateBlockedHostQueryForNote)。`ListByRole` には入っていないので
	// post-fetch で落とす。
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

// packRole builds the misskey_dart RolesListResponse 互換 shape。createdAt /
// updatedAt (非null String) / canEditMembersByModerator (非null bool) /
// usersCount (非null num) を含めないと misskey_dart の RolesListResponse.fromJson
// が cast で落ちる (#1249)。usersCount は mk-go が role member 数を集計していない
// ため 0 固定 (best-effort、非crashing)。
// packRole renders a public role in the upstream-compatible shape. upstream の
// roles/show・list は admin と同じ RoleEntityService.pack を使うため共通 packer
// (entity.PackRole) に統一する。旧実装は target / condFormula / policies /
// preserveAssignmentOnMoveAccount を欠き usersCount を 0 固定していた。
// usersCount は active assignment 数 (role は少数なので per-role count)。
func (h *Handler) packRole(r *model.Role) map[string]any {
	return entity.PackRole(r, h.roleService.CountAssignedUsers(r.ID), h.idGen, role.DefaultPolicies())
}
