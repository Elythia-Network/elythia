// Package antennas provides /api/antennas/* endpoints.
package antennas

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/notehide"
	"github.com/elythia-network/elythia/internal/api/pagination"
	coreantenna "github.com/elythia-network/elythia/internal/core/antenna"
	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
)

// Handler handles antenna-related API endpoints.
type Handler struct {
	svc          *coreantenna.Service
	noteRepo     repository.NoteRepository
	idGen        id.Generator
	instanceRepo repository.InstanceRepository
	emojiRepo    repository.EmojiRepository
	bufReader    entity.BufferedReactionsReader
	fieldRes     *entity.NoteFieldResolver
	// userRepo は antennas/notes の hardMutedWords filter (#787) のために
	// viewer profile を引く。未配線時は filter skip。
	userRepo repository.UserRepository
	// queryService は antennas/notes の visibility filter (#1464) で
	// FilterVisible を呼ぶための note.QueryService。本来 push 段
	// (core/antenna matchNote) で visibility gate しているが、過去に stream へ
	// 滞留した entry や設定ミスに対する defense-in-depth として handler でも
	// 1 段 filter する。未配線時は filter skip (旧挙動)。
	queryService *corenote.QueryService
	// mutingRepo / blockingRepo / channelMutingRepo は antennas/notes で
	// viewer が mute/block した user の note と mute した channel の note を除外する
	// (#1544)。upstream notes.ts の generateBaseNoteFilteringQuery + channelMuting
	// に対応。未配線時は該当 dimension の filter skip。
	mutingRepo        repository.MutingRepository
	blockingRepo      repository.BlockingRepository
	channelMutingRepo repository.ChannelMutingRepository
	// metaRepo は antennas/notes の blocked-host filter (#2106 N5、upstream
	// generateBlockedHostQueryForNote) のために meta.blockedHosts を引く。clips/notes と
	// 同じ設計。未配線時は blocked-host filter skip。
	metaRepo repository.MetaRepository
}

// SetMetaRepo wires a MetaRepository used by the antennas/notes blocked-host filter.
func (h *Handler) SetMetaRepo(r repository.MetaRepository) {
	h.metaRepo = r
}

// blockedHosts returns meta.blockedHosts, or nil when the meta repo is unwired.
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

// SetMuteBlockRepos wires the muting / blocking / channel-muting repositories so
// antennas/notes excludes notes from users the viewer muted or who blocked the
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

// SetUserRepo wires a UserRepository so antennas/notes filters out notes that
// match the viewer's hardMutedWords (#787).
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetQueryService wires a note.QueryService used by Notes as defense-in-depth
// for visibility filtering (#1464). 通常 push 段 (matchNote) で gate されるが、
// stream に残留した entry を捌くために handler 側でも 1 段 filter する。
func (h *Handler) SetQueryService(qs *corenote.QueryService) {
	h.queryService = qs
}

// SetNoteFieldResolver attaches the shared resolver that fills Files /
// MyReaction / Channel on packed notes including their Renote / Reply embed
// (#426)。nil-safe (Apply no-op)。
func (h *Handler) SetNoteFieldResolver(r *entity.NoteFieldResolver) {
	h.fieldRes = r
}

// NewHandler constructs an antennas Handler. noteRepo は antennas/notes で
// note id → entity 変換に使う。
func NewHandler(svc *coreantenna.Service, noteRepo repository.NoteRepository, idGen id.Generator) *Handler {
	return &Handler{svc: svc, noteRepo: noteRepo, idGen: idGen}
}

// SetInstanceRepo attaches an InstanceRepository so antennas/notes populates
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

// CreateRequest is the request body for antennas/create.
//
// upstream paramDef の required
// (`['name','src','keywords','excludeKeywords','users','caseSensitive','withReplies','withFile']`)
// を検証するため、required な bool はポインタで受けて「不在」と `false` を
// 区別する (#2284、admin/roles/create と同じ idiom)。配列は絶対に nil に
// ならない `[]` と不在を nil で区別できるのでそのまま。
type CreateRequest struct {
	Name                           string              `json:"name"`
	Src                            model.AntennaSource `json:"src"`
	UserListID                     *string             `json:"userListId"`
	Users                          []string            `json:"users"`
	Keywords                       [][]string          `json:"keywords"`
	ExcludeKeywords                [][]string          `json:"excludeKeywords"`
	CaseSensitive                  *bool               `json:"caseSensitive"`
	ExcludeBots                    bool                `json:"excludeBots"`
	WithReplies                    *bool               `json:"withReplies"`
	WithFile                       *bool               `json:"withFile"`
	LocalOnly                      bool                `json:"localOnly"`
	ExcludeNotesInSensitiveChannel bool                `json:"excludeNotesInSensitiveChannel"`
}

// missingRequired reports whether any upstream-required field is absent.
func (r *CreateRequest) missingRequired() bool {
	return r.Name == "" || r.Src == "" ||
		r.Keywords == nil || r.ExcludeKeywords == nil || r.Users == nil ||
		r.CaseSensitive == nil || r.WithReplies == nil || r.WithFile == nil
}

// Create handles POST /api/antennas/create.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)
	var req CreateRequest
	if err := c.Bind(&req); err != nil || req.missingRequired() {
		return apierr.JSONInvalidParam(c)
	}
	// upstream create.ts: keywords と excludeKeywords が両方とも空なら EMPTY_KEYWORD。
	if coreantenna.AllKeywordsEmpty(req.Keywords) && coreantenna.AllKeywordsEmpty(req.ExcludeKeywords) {
		return c.JSON(http.StatusBadRequest, apierr.Error("EMPTY_KEYWORD", "Either keywords or excludeKeywords is required.", "53ee222e-1ddd-4f9a-92e5-9fb82ddb463a"))
	}
	a, err := h.svc.Create(coreantenna.CreateInput{
		OwnerID:         user.ID,
		Name:            req.Name,
		Src:             req.Src,
		UserListID:      req.UserListID,
		Users:           req.Users,
		Keywords:        req.Keywords,
		ExcludeKeywords: req.ExcludeKeywords,
		// missingRequired() で非 nil を保証済み。
		CaseSensitive:                  *req.CaseSensitive,
		ExcludeBots:                    req.ExcludeBots,
		WithReplies:                    *req.WithReplies,
		WithFile:                       *req.WithFile,
		LocalOnly:                      req.LocalOnly,
		ExcludeNotesInSensitiveChannel: req.ExcludeNotesInSensitiveChannel,
	})
	if err != nil {
		// Name / EMPTY_KEYWORD は事前に弾いているため、ここに来るのは
		// ErrInvalidSource / NO_SUCH_USER_LIST / antennaLimit / repo エラー。
		if errors.Is(err, coreantenna.ErrInvalidSource) {
			return apierr.JSONInvalidParam(c)
		}
		if errors.Is(err, coreantenna.ErrNoSuchUserList) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER_LIST", "No such user list.", "95063e93-a283-4b8b-9aa5-bcdb8df69a7f"))
		}
		if errors.Is(err, coreantenna.ErrTooManyAntennas) {
			return apierr.JSONTooManyAntennas(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.antennaToMap(a))
}

// ShowRequest is the request body for antennas/show.
type ShowRequest struct {
	AntennaID string `json:"antennaId"`
}

// Show handles POST /api/antennas/show.
func (h *Handler) Show(c echo.Context) error {
	user := middleware.GetUser(c)
	var req ShowRequest
	if err := c.Bind(&req); err != nil || req.AntennaID == "" {
		return apierr.JSONInvalidParam(c)
	}
	a, err := h.svc.Show(user.ID, req.AntennaID)
	if err != nil {
		if errors.Is(err, coreantenna.ErrAccessDenied) {
			return apierr.JSONAccessDenied(c)
		}
		// **DB 障害を not-found に丸めない** (#2799)。`Show` は #2799 で raw DB
		// error も返すようになったので、sentinel かどうかで分ける。
		if !errors.Is(err, coreantenna.ErrAntennaNotFound) {
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANTENNA", "No such antenna.", "c06569fb-b025-4f23-b22d-1fcd20d2816b"))
	}
	return c.JSON(http.StatusOK, h.antennaToMap(a))
}

// UpdateRequest is the request body for antennas/update.
type UpdateRequest struct {
	AntennaID                      string               `json:"antennaId"`
	Name                           *string              `json:"name"`
	Src                            *model.AntennaSource `json:"src"`
	UserListID                     *string              `json:"userListId"`
	Users                          *[]string            `json:"users"`
	Keywords                       *[][]string          `json:"keywords"`
	ExcludeKeywords                *[][]string          `json:"excludeKeywords"`
	CaseSensitive                  *bool                `json:"caseSensitive"`
	ExcludeBots                    *bool                `json:"excludeBots"`
	WithReplies                    *bool                `json:"withReplies"`
	WithFile                       *bool                `json:"withFile"`
	LocalOnly                      *bool                `json:"localOnly"`
	IsActive                       *bool                `json:"isActive"`
	ExcludeNotesInSensitiveChannel *bool                `json:"excludeNotesInSensitiveChannel"`
}

// Update handles POST /api/antennas/update.
func (h *Handler) Update(c echo.Context) error {
	user := middleware.GetUser(c)
	var req UpdateRequest
	if err := c.Bind(&req); err != nil || req.AntennaID == "" {
		return apierr.JSONInvalidParam(c)
	}
	// upstream update.ts: keywords/excludeKeywords が両方指定され、かつ両方とも
	// 空なら EMPTY_KEYWORD (片方のみ指定の場合は検査しない)。
	if req.Keywords != nil && req.ExcludeKeywords != nil &&
		coreantenna.AllKeywordsEmpty(*req.Keywords) && coreantenna.AllKeywordsEmpty(*req.ExcludeKeywords) {
		return c.JSON(http.StatusBadRequest, apierr.Error("EMPTY_KEYWORD", "Either keywords or excludeKeywords is required.", "721aaff6-4e1b-4d88-8de6-877fae9f68c4"))
	}
	a, err := h.svc.Update(user.ID, req.AntennaID, coreantenna.UpdateInput{
		Name:                           req.Name,
		Src:                            req.Src,
		UserListID:                     req.UserListID,
		Users:                          req.Users,
		Keywords:                       req.Keywords,
		ExcludeKeywords:                req.ExcludeKeywords,
		CaseSensitive:                  req.CaseSensitive,
		ExcludeBots:                    req.ExcludeBots,
		WithReplies:                    req.WithReplies,
		WithFile:                       req.WithFile,
		LocalOnly:                      req.LocalOnly,
		IsActive:                       req.IsActive,
		ExcludeNotesInSensitiveChannel: req.ExcludeNotesInSensitiveChannel,
	})
	if err != nil {
		switch {
		case errors.Is(err, coreantenna.ErrAntennaNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANTENNA", "No such antenna.", "10c673ac-8852-48eb-aa1f-f5b67f069290"))
		case errors.Is(err, coreantenna.ErrAccessDenied):
			return apierr.JSONAccessDenied(c)
		case errors.Is(err, coreantenna.ErrNoSuchUserList):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_USER_LIST", "No such user list.", "1c6b35c9-943e-48c2-81e4-2844989407f7"))
		case errors.Is(err, coreantenna.ErrAntennaNameRequired),
			errors.Is(err, coreantenna.ErrInvalidSource):
			return apierr.JSONInvalidParam(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.antennaToMap(a))
}

// DeleteRequest is the request body for antennas/delete.
type DeleteRequest struct {
	AntennaID string `json:"antennaId"`
}

// Delete handles POST /api/antennas/delete.
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)
	var req DeleteRequest
	if err := c.Bind(&req); err != nil || req.AntennaID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.Delete(user.ID, req.AntennaID); err != nil {
		switch {
		case errors.Is(err, coreantenna.ErrAntennaNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANTENNA", "No such antenna.", "b34dcf9d-348f-44bb-99d0-6c9314cfe2df"))
		case errors.Is(err, coreantenna.ErrAccessDenied):
			return apierr.JSONAccessDenied(c)
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// RemoveNote handles POST /api/antennas/remove-note (upstream #17463, #2069).
// antenna TL から特定ノートを除去する。upstream は findOneBy({id, userId}) で
// not-found / not-owner を区別せず NO_SUCH_ANTENNA を返すので、mk-go も両方を
// NO_SUCH_ANTENNA に倒す。
func (h *Handler) RemoveNote(c echo.Context) error {
	user := middleware.GetUser(c)
	var req struct {
		AntennaID string `json:"antennaId"`
		NoteID    string `json:"noteId"`
	}
	if err := c.Bind(&req); err != nil || req.AntennaID == "" || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if err := h.svc.RemoveNote(user.ID, req.AntennaID, req.NoteID); err != nil {
		if errors.Is(err, coreantenna.ErrAntennaNotFound) || errors.Is(err, coreantenna.ErrAccessDenied) {
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANTENNA", "No such antenna.", "850926e0-fd3b-49b6-b69a-b28a5dbd82fe"))
		}
		return apierr.JSONInternalError(c)
	}
	return c.NoContent(http.StatusNoContent)
}

// List handles POST /api/antennas/list.
func (h *Handler) List(c echo.Context) error {
	user := middleware.GetUser(c)
	rows, err := h.svc.ListByUser(user.ID)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, h.antennaToMap(a))
	}
	return c.JSON(http.StatusOK, out)
}

// NotesRequest is the request body for antennas/notes.
//
// SinceID / UntilID は upstream Misskey TS と同じ paging key (#693)。
// 設定しないと FE が無限スクロールするたびに同じ最新 N 件を取り続けて
// 「同じノートが何度も表示される」現象になる。
type NotesRequest struct {
	AntennaID string `json:"antennaId"`
	Limit     *int   `json:"limit"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// Notes handles POST /api/antennas/notes.
func (h *Handler) Notes(c echo.Context) error {
	user := middleware.GetUser(c)
	var req NotesRequest
	if err := c.Bind(&req); err != nil || req.AntennaID == "" {
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
	// over-fetch: zset に滞留した followers/specified entry や hardMute hit が
	// handler 側 filter で削られると、返却件数が limit を下回り得る。安全側に
	// limit の 2 倍 (上限 MaxNotesPerAntenna) で zset から拾い、filter 後に
	// limit へトリミングする (#1467 review)。FE は最後の note id を untilId
	// に渡してくるため、トリミングしてもページング境界は保たれる。
	overFetch := limit * 2
	if overFetch > coreantenna.MaxNotesPerAntenna {
		overFetch = coreantenna.MaxNotesPerAntenna
	}
	ids, err := h.svc.Notes(c.Request().Context(), user.ID, req.AntennaID, overFetch, sinceID, untilID)
	if err != nil {
		switch {
		case errors.Is(err, coreantenna.ErrAntennaNotFound):
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_ANTENNA", "No such antenna.", "850926e0-fd3b-49b6-b69a-b28a5dbd82fe"))
		case errors.Is(err, coreantenna.ErrAccessDenied):
			return apierr.JSONAccessDenied(c)
		}
		return apierr.JSONInternalError(c)
	}
	notes, err := h.noteRepo.FindManyByIDsWithUser(ids)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	// 解決できなかった ID を zset から除く (#2719)。**filter を掛ける前の
	// notes で判定する。** 下の visibility / mute / block で落ちた note は
	// 生きているので、filter 後の集合で差分を取ると消してはいけない ID を
	// 消す (#2718 と同じ不変条件)。
	//
	// FindManyByIDsWithUser は素の `WHERE id IN ?` で soft-delete 条件も
	// 持たないので、単一 primary なら「ids に有って戻りに無い = DB 行が無い」
	// が言える。**リードレプリカ構成では言えない** — 複製前の行が引けない
	// ので、service 側が消す前に primary で存在を確かめる。
	// err のときは prune しない (上で早期 return 済み)。
	h.svc.PruneDangling(c.Request().Context(), req.AntennaID, ids, notes)
	// visibility filter (defense-in-depth, #1464): push 段 (core/antenna
	// matchNote) で followers/specified note は antenna owner 視点で gate されて
	// いるが、過去に zset に滞留した entry や設定ミスに対するフォールバック
	// として handler でも 1 段 filter する (`notes/user-list-timeline`
	// (`internal/api/notes/handler_extra.go:UserListTimeline`) と同じパターン。
	// なお `channels/timeline` は service/repo 層で SQL push-down する別パターン
	// で filter している (#1440))。queryService 未配線時は filter skip (旧挙動)。
	if h.queryService != nil {
		notes = h.queryService.FilterVisible(user, notes)
	}
	// mute/block/channel-mute/instance-mute filter (#1544 / #1630): upstream
	// notes.ts の generateBaseNoteFilteringQuery + channelMuting に相当。set の
	// ロードに失敗したら fail-closed で 500 を返す (security 項目なので
	// silently leak しない)。renote 先の入れ子 author 検査は h.noteRepo 経由。
	mbSets, err := notesfilter.LoadMuteBlockSets(user, h.mutingRepo, h.blockingRepo, h.channelMutingRepo, h.userRepo)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes, err = notesfilter.ApplyMuteBlockChannel(notes, mbSets, h.noteRepo)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes = notesfilter.ApplyHardMute(h.userRepo, user, notes)
	// #2106 N5: admin の blockedHosts でブロックした remote host の note と、suspended な
	// author の note を除外する (upstream notes.ts:132-133 の generateBaseNoteFilteringQuery
	// = generateBlockedHostQueryForNote + generateSuspendedUserQueryForNote)。clips/notes が
	// blocked-host を適用済なのに antennas は未適用で非対称だった。truncate 前に適用する。
	blockedHosts, err := h.blockedHosts()
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	notes = notesfilter.ApplyBlockedHosts(notes, blockedHosts)
	notes = notesfilter.ApplySuspended(notes)
	// over-fetch 分を要求 limit に揃える。FindManyByIDsWithUser が ids の順序を
	// 保つので newest-first の先頭 limit 件を返せばよい (#1467 review)。
	if len(notes) > limit {
		notes = notes[:limit]
	}
	entities := entity.PackNotes(c.Request().Context(), notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldRes.Apply(entities, user)
	notehide.HideEmbeds(user, entities)
	out := make([]any, 0, len(entities))
	for _, pn := range entities {
		out = append(out, pn)
	}
	return c.JSON(http.StatusOK, out)
}

func (h *Handler) antennaToMap(a *model.Antenna) map[string]any {
	// upstream Misskey TS の packAntenna は userId field を embed しない
	// (= antenna は user-scoped で frontend が呼出 user の id を別経路で持つ
	// 設計、#904)。drop-in 互換維持のため mk-go も同 shape に揃える。
	//
	// createdAt は antenna ID (aidx) から復元する。以前は lastUsedAt を流用して
	// いたが createdAt は作成時刻であるべき + misskey_dart の Antenna.fromJson が
	// 非null String として cast するため ISO ms 形式で出す (#1244)。
	// hasUnreadNote は misskey_dart が非null bool として cast する (#1244)。
	//
	// 未読そのものは追跡している (#2406。matchNote が `antenna_note_unread` に
	// 行を作り、antenna timeline の閲覧で消す)。false 固定なのは **antenna 個別**
	// の未読を算出していないためで、user 全体の `hasUnreadAntenna` (`/api/i`) は
	// 実際に DB を引いている。upstream も `AntennaEntityService` が
	// `false, // TODO` なのでここは一致している (docs/divergence.md)。
	createdAt := ""
	if h.idGen != nil {
		if t, err := h.idGen.ParseTime(a.ID); err == nil {
			createdAt = t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	// users は model.StringArray (nil なら null になる) を golden の非null array に
	// 合わせて [] へ coalesce する (#1270 L3 検出)。
	users := []string(a.Users)
	if users == nil {
		users = []string{}
	}
	return map[string]any{
		"id":         a.ID,
		"createdAt":  createdAt,
		"name":       a.Name,
		"src":        a.Src,
		"userListId": a.UserListID,
		"users":      users,
		// keywords / excludeKeywords は jsonb (datatypes.JSON)。未設定 antenna は
		// nil で JSON null になるが golden Antenna は string[][] 必須なので [] へ
		// coalesce する (#1318。users #1270 と同種の null-array drift)。
		"keywords":        jsonArrayOrEmpty(a.Keywords),
		"excludeKeywords": jsonArrayOrEmpty(a.ExcludeKeywords),
		"caseSensitive":   a.CaseSensitive,
		"excludeBots":     a.ExcludeBots,
		"withReplies":     a.WithReplies,
		"withFile":        a.WithFile,
		"localOnly":       a.LocalOnly,
		"isActive":        a.IsActive,
		"hasUnreadNote":   false,
		// excludeNotesInSensitiveChannel は model にあるので反映。notify は
		// golden で必須 (boolean) だが false 固定で shape だけ満たす
		// (#1270 L3 検出)。**未実装ではなく、持たないのが upstream と同じ**:
		// upstream は `RemoveAntennaNotify` migration (1716450883149) で列ごと
		// DROP しており、JSON schema に残っているのは既存クライアントの型を
		// 壊さないためだけ。実装すると逆に乖離する (#2406)。
		"excludeNotesInSensitiveChannel": a.ExcludeNotesInSensitiveChannel,
		"notify":                         false,
	}
}

// jsonArrayOrEmpty coalesces a nil/empty jsonb column to a non-null empty
// array so the JSON encoder emits `[]` instead of `null`。非空なら格納済みの
// 生 JSON をそのまま返す (#1318)。
func jsonArrayOrEmpty(b []byte) any {
	if len(b) == 0 {
		return []any{}
	}
	return json.RawMessage(b)
}

// HasMetaRepo reports whether the meta repository was wired.
//
// 未配線だと blocked-host の note が post-fetch 経路で漏れる。起動時検査に
// 使う (#2708)。
func (h *Handler) HasMetaRepo() bool { return h.metaRepo != nil }

// HasMuteBlockRepos reports whether all three mute/block repositories were wired.
//
// **3 つを 1 つの述語で見る。** 呼び出しは `LoadMuteBlockSets` への直渡しで、
// 各 repo は nil を**空集合として素通し**するため、1 つ欠けるだけでその次元の
// filter が黙って消える。notes 側と違い呼び出し元の gate も無い。起動時検査に
// 使う (#2709 review)。
func (h *Handler) HasMuteBlockRepos() bool {
	return h.mutingRepo != nil && h.blockingRepo != nil && h.channelMutingRepo != nil
}
