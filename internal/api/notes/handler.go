package notes

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/notehide"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/core/notesfilter"
	"github.com/shiroha-a/mk/internal/core/poll"
	"github.com/shiroha-a/mk/internal/core/reaction"
	"github.com/shiroha-a/mk/internal/core/role"
	"github.com/shiroha-a/mk/internal/core/search"
	"github.com/shiroha-a/mk/internal/core/timeline"
	"github.com/shiroha-a/mk/internal/core/translate"
	"github.com/shiroha-a/mk/internal/core/ugcvisibility"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/queue"
	"github.com/shiroha-a/mk/internal/queue/driver"
	"github.com/shiroha-a/mk/internal/repository"
	"github.com/shiroha-a/mk/internal/server/middleware"
)

// Handler handles note-related API endpoints.
type Handler struct {
	noteRepo        repository.NoteRepository
	piningRepo      repository.UserNotePiningRepository
	createService   *note.CreateService
	deleteService   *note.DeleteService
	queryService    *note.QueryService
	timelineService *timeline.Service
	reactionService *reaction.Service
	pollService     *poll.Service
	searchService   *search.Service
	idGen           id.Generator
	favoriteRepo    repository.NoteFavoriteRepository
	// materializer はリレー由来で DB に無いノートを昇格させる (#2332)。
	// note_favorite.noteId が note への外部キーなので、行が無いと INSERT が
	// 失敗する。favorite には core service が無く、この handler が FK 行を
	// 書いているためここが差し込み口になる。
	materializer      NoteMaterializer
	driveFileRepo     repository.DriveFileRepository
	draftRepo         repository.NoteDraftRepository
	pollRepo          repository.PollRepository
	threadMutingRepo  repository.NoteThreadMutingRepository
	noteReactionRepo  repository.NoteReactionRepository
	channelRepo       repository.ChannelRepository
	channelMutingRepo repository.ChannelMutingRepository
	// channelFollowingRepo は home timeline で follow 中 channel の note を
	// 含めるため、viewer の followed channel id を引くのに使う (#1686)。
	// 未配線時は home に followed channel note を含めない (= channelId IS NULL のみ)。
	channelFollowingRepo repository.ChannelFollowingRepository
	// userFollowingRepo は HTL の「返信先が followers 限定の投稿」ガードに使う。
	// upstream timeline.ts は取得側の noteFilter で followings を参照する。
	// 未配線ならガードをスキップする。
	userFollowingRepo repository.FollowingRepository
	mutingRepo        repository.MutingRepository
	renoteMutingRepo  repository.RenoteMutingRepository
	// blockingRepo は notes/children・replies・renotes の list 応答で被block /
	// mute / instance-mute filter を適用するのに使う (#1554、upstream
	// generateBaseNoteFilteringQuery 相当)。未配線時は filter skip。
	blockingRepo repository.BlockingRepository
	instanceRepo repository.InstanceRepository
	// metaRepo は list endpoint の blocked-host filter で meta.blockedHosts を
	// 引く (#1783)。未配線時は blocked-host filter を skip。
	metaRepo        repository.MetaRepository
	emojiRepo       repository.EmojiRepository
	driveFolderRepo repository.DriveFolderRepository
	userRepo        repository.UserRepository
	userListRepo    repository.UserListRepository
	// clipRepo / clipNoteRepo / clipFavoriteRepo は notes/clips で「この note を
	// 含む public clip」を Clip entity として返すのに使う (#1554)。未配線時は
	// 空配列 fallback (旧 stub 互換)。
	clipRepo         repository.ClipRepository
	clipNoteRepo     repository.ClipNoteRepository
	clipFavoriteRepo repository.ClipFavoriteRepository
	moderatorChecker ModeratorChecker
	// achievementGranter は notes/favorites/create で local note 著者に
	// myNoteFavorited1 を付与するのに使う (#1762)。未配線時は付与しない。
	achievementGranter AchievementGranter
	bufReader          entity.BufferedReactionsReader
	// ugcVisibilityFn controls what unauthenticated visitors can see.
	//
	// **毎回読む。** 起動時に文字列を焼き込むと、運営者が管理画面で締めても
	// プロセスを再起動するまで反映されない。`/api/meta` と管理画面は新しい値を
	// 返し、robots.txt と SSR も毎回 meta を読むので「ブラウザで見ると効いて
	// いる」ように見えるのに、API は匿名に本文を返し続ける。
	ugcVisibilityFn func() string
	// ugcVisibility controls what unauthenticated visitors can see.
	// "all" (default), "local", "none"
	ugcVisibility string
	translator    *translate.DeepLClient
	// policyProvider は LocalTimeline / GlobalTimeline / HybridTimeline で
	// ltlAvailable / gtlAvailable policy を gate するために使う (#1026)。
	// 匿名 viewer (userID="") に対しても base policies を返す upstream 互換
	// semantics で、middleware ではなく handler 内で gate する。
	policyProvider role.PolicyProvider
	// scheduledNoteEnqueuer は drafts/create で `isActuallyScheduled=true`
	// の draft を delayed queue に enqueue するための narrow interface (#1040)。
	// nil 時は enqueue を skip する (= test fixture / queue 未配線パス互換)。
	scheduledNoteEnqueuer ScheduledNoteEnqueuer
	// timelineCache は config flag enableTimelineCache (MK_ENABLETIMELINECACHE)
	// 有効時のみ non-nil。first-page timeline 応答を per-viewer で短期キャッシュする。
	timelineCache *timelineJSONCache
	// featuredRanking は notes/featured の engagement ランキング (#1687)。nil 時
	// (= 未配線 / test) は SQL count-DESC fallback を使う。
	featuredRanking FeaturedRankingReader
	// featuredGlobalCache は global ranking の 30分 in-memory cache
	// (upstream featured.ts の globalNotesRankingCache)。channel ranking は cache
	// しない (channel ごとなので)。
	featuredGlobalCache featuredGlobalRankingCache
}

// SetPiningRepo wires the repository used to verify that a notes/show target
// is still pinned at request time.
func (h *Handler) SetPiningRepo(r repository.UserNotePiningRepository) {
	h.piningRepo = r
}

// FeaturedRankingReader reads the engagement ranking for notes/featured (#1687).
// 循環依存回避のため narrow interface (実装は core/featured.Service)。
type FeaturedRankingReader interface {
	GetGlobalNotesRanking(ctx context.Context, threshold int) ([]string, error)
	GetInChannelNotesRanking(ctx context.Context, channelID string, threshold int) ([]string, error)
}

// featuredGlobalRankingCache holds the 30-min global ranking cache.
type featuredGlobalRankingCache struct {
	mu        sync.Mutex
	ids       []string
	fetchedAt time.Time
}

// SetFeaturedRanking wires the engagement ranking reader so notes/featured uses
// it (#1687). nil 注入時は SQL count-DESC fallback。
func (h *Handler) SetFeaturedRanking(r FeaturedRankingReader) {
	h.featuredRanking = r
}

// EnableTimelineJSONCache turns on the first-page timeline response cache with
// the given TTL. config flag enableTimelineCache (MK_ENABLETIMELINECACHE) 経由で
// server 配線から呼ぶ。
func (h *Handler) EnableTimelineJSONCache(ttl time.Duration) {
	h.timelineCache = newTimelineJSONCache(ttl)
}

// ScheduledNoteEnqueuer abstracts the delayed enqueue path used by
// drafts/create. Implemented by *queue.Client (= EnqueuePostScheduledNote)。
type ScheduledNoteEnqueuer interface {
	EnqueuePostScheduledNote(payload queue.PostScheduledNotePayload, opts ...driver.EnqueueOption) error
	ClearScheduledNote(draftID string) error
}

// SetScheduledNoteEnqueuer wires a delayed queue client used by drafts/create
// when the request asks for a scheduled publish (#1040). nil disables the
// enqueue path (= test fixture / unwired)。
func (h *Handler) SetScheduledNoteEnqueuer(e ScheduledNoteEnqueuer) {
	h.scheduledNoteEnqueuer = e
}

// SetPolicyProvider wires a role policy source so timeline endpoints gate
// access by ltlAvailable / gtlAvailable (#1026). nil 時は gate を skip する
// (= test 経路 / 旧挙動互換)。匿名 viewer (userID="") には base policies を
// 返す upstream 互換 semantics を要求する。
func (h *Handler) SetPolicyProvider(p role.PolicyProvider) {
	h.policyProvider = p
}

// SetChannelMutingRepo attaches a ChannelMutingRepository so timeline handlers
// can exclude notes posted to channels the viewer has muted.
func (h *Handler) SetChannelMutingRepo(r repository.ChannelMutingRepository) {
	h.channelMutingRepo = r
}

// SetUserFollowingRepo attaches a FollowingRepository used by the home timeline
// followers-only-reply guard.
func (h *Handler) SetUserFollowingRepo(r repository.FollowingRepository) {
	h.userFollowingRepo = r
}

// SetChannelFollowingRepo attaches a ChannelFollowingRepository so the home
// timeline includes notes from channels the viewer follows (#1686).
func (h *Handler) SetChannelFollowingRepo(r repository.ChannelFollowingRepository) {
	h.channelFollowingRepo = r
}

// SetMutingRepo attaches a MutingRepository so timeline handlers can exclude
// notes whose author / renote-source the viewer has muted (= upstream Misskey
// TS の muting JOIN と同 semantics、#874)。
func (h *Handler) SetMutingRepo(r repository.MutingRepository) {
	h.mutingRepo = r
}

// SetRenoteMutingRepo attaches a RenoteMutingRepository so timeline handlers
// can exclude pure renotes by users the viewer has renote-muted (#903)。
// upstream Misskey TS の generateMutedUserRelatedRenotesQuery と同 semantics。
func (h *Handler) SetRenoteMutingRepo(r repository.RenoteMutingRepository) {
	h.renoteMutingRepo = r
}

// SetBlockingRepo attaches a BlockingRepository so notes/children・replies・
// renotes can exclude notes authored by users who block the viewer (#1554)。
func (h *Handler) SetBlockingRepo(r repository.BlockingRepository) {
	h.blockingRepo = r
}

// SetTranslator attaches a DeepL translator for /api/notes/translate.
func (h *Handler) SetTranslator(t *translate.DeepLClient) {
	h.translator = t
}

// SetUGCVisibility sets the visitor content visibility policy from meta.
//
// Deprecated: 固定値を焼き込む形。`SetUGCVisibilityLookup` を使うこと。
// テストの利便のために残してある。
func (h *Handler) SetUGCVisibility(v string) {
	h.ugcVisibility = v
}

// SetUGCVisibilityLookup wires a live lookup of the policy.
func (h *Handler) SetUGCVisibilityLookup(fn func() string) {
	h.ugcVisibilityFn = fn
}

// ugcVisibilityNow resolves the current policy.
func (h *Handler) ugcVisibilityNow() string {
	if h.ugcVisibilityFn != nil {
		return h.ugcVisibilityFn()
	}
	return h.ugcVisibility
}

// visitorHidesAll reports whether viewer is an anonymous visitor who must get
// no content at all under meta.ugcVisibilityForVisitor=none. Mirrors
// upstream's `me == null && ugcVisibilityForVisitor === 'none'` checks and the
// `1=0` that generateVisibilityQuery adds for visitors.
func (h *Handler) visitorHidesAll(viewer *model.User) bool {
	return viewer == nil && ugcvisibility.HidesAll(h.ugcVisibilityNow())
}

// filterVisitorUGC drops notes an anonymous visitor must not see under
// meta.ugcVisibilityForVisitor (`none` → all, `local` → notes whose own author
// is remote). Signed-in viewers get notes unchanged. Mirrors upstream
// QueryService.generateUgcVisibilityQueryForVisitor for paths that are
// filtered after fetching.
func (h *Handler) filterVisitorUGC(viewer *model.User, notes []*model.Note) []*model.Note {
	if viewer != nil {
		return notes
	}
	policy := h.ugcVisibilityNow()
	if !ugcvisibility.HidesAll(policy) && policy != ugcvisibility.Local {
		return notes
	}
	out := make([]*model.Note, 0, len(notes))
	for _, n := range notes {
		if !ugcvisibility.HidesNote(policy, n.UserHost) {
			out = append(out, n)
		}
	}
	return out
}

// SetDriveFileRepo attaches a DriveFileRepository for file resolution.
func (h *Handler) SetDriveFileRepo(r repository.DriveFileRepository) {
	h.driveFileRepo = r
}

// ModeratorChecker reports whether a user holds moderator / administrator
// privileges. core/role.Service implements it. Used by notes/delete to let a
// moderator delete other users' notes (#1538).
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// SetModeratorChecker wires the moderator check used by notes/delete.
func (h *Handler) SetModeratorChecker(m ModeratorChecker) {
	h.moderatorChecker = m
}

// AchievementGranter unlocks a server-side achievement for a user. core/achievement.Service
// implements it. Used by notes/favorites/create to grant myNoteFavorited1 (#1762).
type AchievementGranter interface {
	Grant(ctx context.Context, userID, name string) (bool, error)
}

// SetAchievementGranter wires the achievement grant service. When unset,
// favorite-driven achievements are simply not granted (degrade).
func (h *Handler) SetAchievementGranter(g AchievementGranter) {
	h.achievementGranter = g
}

// SetPollRepo attaches a PollRepository used by notes/polls/recommendation (#1538).
func (h *Handler) SetPollRepo(r repository.PollRepository) {
	h.pollRepo = r
}

// SetNoteReactionRepo attaches a NoteReactionRepository for myReaction resolution.
func (h *Handler) SetNoteReactionRepo(r repository.NoteReactionRepository) {
	h.noteReactionRepo = r
}

// SetChannelRepo attaches a ChannelRepository for channel resolution.
func (h *Handler) SetChannelRepo(r repository.ChannelRepository) {
	h.channelRepo = r
}

// SetInstanceRepo attaches an InstanceRepository so remote user embeds in
// note responses get their `instance` field populated (#277).
func (h *Handler) SetInstanceRepo(r repository.InstanceRepository) {
	h.instanceRepo = r
}

// SetEmojiRepo attaches an EmojiRepository so custom emoji shortcodes in
// note text and user displayNames get resolved to URLs (#330).
func (h *Handler) SetEmojiRepo(r repository.EmojiRepository) {
	h.emojiRepo = r
}

// SetReactionReader wires a BufferedReactionsReader so PackNote / PackNotes
// can merge in-flight buffered reaction deltas (#647)。enableReactionsBuffering
// が無効なら nil でも構わない。
func (h *Handler) SetReactionReader(r entity.BufferedReactionsReader) {
	h.bufReader = r
}

func (h *Handler) reactionReader() entity.BufferedReactionsReader {
	return h.bufReader
}

// SetDriveFolderRepo attaches a DriveFolderRepository so attached DriveFiles
// in note responses can embed the owning folder (#317).
func (h *Handler) SetDriveFolderRepo(r repository.DriveFolderRepository) {
	h.driveFolderRepo = r
}

// SetUserRepo attaches a UserRepository so attached DriveFiles in note
// responses can embed the owning user (#317).
func (h *Handler) SetUserRepo(r repository.UserRepository) {
	h.userRepo = r
}

// SetUserListRepo attaches a UserListRepository for user-list-timeline.
func (h *Handler) SetUserListRepo(r repository.UserListRepository) {
	h.userListRepo = r
}

// instanceLookup returns the repository as an entity.InstanceLookup, or nil
// when no repo has been wired. Narrowing to the entity interface keeps the
// packer independent from repository details.
func (h *Handler) instanceLookup() entity.InstanceLookup {
	if h.instanceRepo == nil {
		return nil
	}
	return h.instanceRepo
}

// emojiLookup returns the repository as an entity.EmojiLookup, or nil when
// no repo has been wired.
func (h *Handler) emojiLookup() entity.EmojiLookup {
	if h.emojiRepo == nil {
		return nil
	}
	return h.emojiRepo
}

// NewHandler creates a new notes Handler.
// queryService/timelineService/reactionService/pollService/searchService が
// nil の場合、それぞれの依存に対応するエンドポイントは利用不可となる
// (テストで一部だけ初期化する用途を許容する)。
func NewHandler(
	noteRepo repository.NoteRepository,
	createService *note.CreateService,
	deleteService *note.DeleteService,
	queryService *note.QueryService,
	timelineService *timeline.Service,
	reactionService *reaction.Service,
	pollService *poll.Service,
	searchService *search.Service,
	idGen id.Generator,
) *Handler {
	return &Handler{
		noteRepo:        noteRepo,
		createService:   createService,
		deleteService:   deleteService,
		queryService:    queryService,
		timelineService: timelineService,
		reactionService: reactionService,
		pollService:     pollService,
		searchService:   searchService,
		idGen:           idGen,
	}
}

// CreateRequest is the request body for notes/create.
type CreateRequest struct {
	Visibility         string   `json:"visibility"`
	VisibleUserIDs     []string `json:"visibleUserIds"`
	CW                 *string  `json:"cw"`
	Text               *string  `json:"text"`
	LocalOnly          bool     `json:"localOnly"`
	ReactionAcceptance *string  `json:"reactionAcceptance"`
	FileIDs            []string `json:"fileIds"`
	// MediaIDs は fileIds の別名 (upstream は fileIds ?? mediaIds、#1538)。
	MediaIDs  []string     `json:"mediaIds"`
	ReplyID   *string      `json:"replyId"`
	RenoteID  *string      `json:"renoteId"`
	ChannelID *string      `json:"channelId"`
	Poll      *PollRequest `json:"poll"`
	// noExtract* は mention/hashtag/emoji の自動抽出を抑止する (#1538)。
	NoExtractMentions bool `json:"noExtractMentions"`
	NoExtractHashtags bool `json:"noExtractHashtags"`
	NoExtractEmojis   bool `json:"noExtractEmojis"`
}

// PollRequest is the poll part of a create note request.
type PollRequest struct {
	Choices      []string `json:"choices"`
	Multiple     bool     `json:"multiple"`
	ExpiresAt    *int64   `json:"expiresAt"`
	ExpiredAfter *int64   `json:"expiredAfter"`
}

// Create handles POST /api/notes/create.
func (h *Handler) Create(c echo.Context) error {
	user := middleware.GetUser(c)

	var req CreateRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}

	// fileIds ?? mediaIds (upstream alias、#1538)。fileIds が空のときのみ
	// mediaIds をフォールバックで使う。
	fileIDs := req.FileIDs
	if len(fileIDs) == 0 && len(req.MediaIDs) > 0 {
		fileIDs = req.MediaIDs
	}

	// 入力検証 (upstream notes/create.ts paramDef、#1538)。違反は 400 INVALID_PARAM。
	if err := validateCreateInput(&req, fileIDs); err != nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", err.Error(), apierr.UUIDInvalidParam))
	}

	in := note.CreateInput{
		User:               user,
		Text:               req.Text,
		CW:                 req.CW,
		Visibility:         model.NoteVisibility(req.Visibility),
		VisibleUserIDs:     req.VisibleUserIDs,
		LocalOnly:          req.LocalOnly,
		ReactionAcceptance: req.ReactionAcceptance,
		FileIDs:            fileIDs,
		ReplyID:            req.ReplyID,
		RenoteID:           req.RenoteID,
		ChannelID:          req.ChannelID,
		NoExtractMentions:  req.NoExtractMentions,
		NoExtractHashtags:  req.NoExtractHashtags,
		NoExtractEmojis:    req.NoExtractEmojis,
	}

	if req.Poll != nil {
		in.Poll = &note.PollInput{
			Choices:  req.Poll.Choices,
			Multiple: req.Poll.Multiple,
		}
		// Misskey TS API 互換: expiresAt (絶対 unix ms) と expiredAfter (相対 ms)
		// のどちらか / 両方が送られる。frontend の form は「期限なし / 日時指定 /
		// X 後」の 3 択で、相対指定時は expiredAfter のみが入る。
		// #2106 L6: 両方来た時は upstream create.ts:230 と同じく expiredAfter (相対) を優先する
		// (旧コメントの『TS PollService』は誤り、endpoint の create.ts が expiry を決定する)。
		switch {
		case req.Poll.ExpiredAfter != nil:
			t := time.Now().Add(time.Duration(*req.Poll.ExpiredAfter) * time.Millisecond)
			in.Poll.ExpiresAt = &t
		case req.Poll.ExpiresAt != nil:
			t := time.UnixMilli(*req.Poll.ExpiresAt)
			in.Poll.ExpiresAt = &t
		}
	}

	created, err := h.createService.Create(in)
	if err != nil {
		switch {
		case errors.Is(err, note.ErrNoteContentRequired):
			return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Text, fileIds, or renoteId is required.", apierr.UUIDInvalidParam))
		case errors.Is(err, note.ErrReplyTargetNotFound):
			return apierr.JSONNoSuchReplyTarget(c)
		case errors.Is(err, note.ErrRenoteTargetNotFound):
			return apierr.JSONNoSuchRenoteTarget(c)
		case errors.Is(err, note.ErrCannotReplyToInvisibleNote):
			return apierr.JSONCannotReplyToAnInvisibleNote(c)
		case errors.Is(err, note.ErrCannotRenoteInvisibleNote):
			return apierr.JSONCannotRenoteDueToVisibility(c)
		case errors.Is(err, note.ErrChannelNotFound):
			return apierr.JSONNoSuchChannel(c)
		case errors.Is(err, note.ErrCannotRenoteToAPureRenote):
			return apierr.JSONCannotRenoteToAPureRenote(c)
		case errors.Is(err, note.ErrCannotReplyToAPureRenote):
			return apierr.JSONCannotReplyToAPureRenote(c)
		case errors.Is(err, note.ErrCannotReplyToSpecifiedVisibility):
			return apierr.JSONCannotReplyToSpecifiedVisibilityNoteWithExtendedVisibility(c)
		case errors.Is(err, note.ErrYouHaveBeenBlocked):
			return apierr.JSONYouHaveBeenBlocked(c)
		case errors.Is(err, note.ErrCannotCreateAlreadyExpiredPoll):
			return apierr.JSONCannotCreateAlreadyExpiredPoll(c)
		case errors.Is(err, note.ErrNoSuchFile):
			return apierr.JSONNoSuchFile(c)
		case errors.Is(err, note.ErrCannotRenoteOutsideOfChannel):
			return apierr.JSONCannotRenoteOutsideOfChannel(c)
		case errors.Is(err, note.ErrContainsProhibitedWords):
			return apierr.JSONContainsProhibitedWords(c)
		case errors.Is(err, note.ErrContainsTooManyMentions):
			return apierr.JSONContainsTooManyMentions(c)
		}
		return apierr.JSONInternalError(c)
	}

	packed := entity.PackNoteWithInstance(c.Request().Context(), created, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	s := []entity.NoteEntity{packed}
	h.fieldResolver().Apply(s, user)
	notehide.HideEmbeds(user, s)
	return c.JSON(http.StatusOK, map[string]any{
		"createdNote": s[0],
	})
}

// maxNoteTextLength mirrors upstream MAX_NOTE_TEXT_LENGTH (const.ts、#1538)。
const maxNoteTextLength = 3000

// validateCreateInput enforces the upstream notes/create.ts paramDef limits
// (text/cw length, poll choices, fileIds count) and the text-only [^\s]+ rule.
// fileIDs is the effective attachment list after the fileIds/mediaIds merge.
// Returns a non-nil error (its message becomes the INVALID_PARAM body) on the
// first violation, matching upstream's 400 rejection (#1538)。
func validateCreateInput(req *CreateRequest, fileIDs []string) error {
	if req.Text != nil && len([]rune(*req.Text)) > maxNoteTextLength {
		return errors.New("text is too long")
	}
	if req.CW != nil {
		if n := len([]rune(*req.CW)); n < 1 || n > 100 {
			return errors.New("cw must be between 1 and 100 characters")
		}
	}
	if len(fileIDs) > 16 {
		return errors.New("too many files (max 16)")
	}
	if req.Poll != nil {
		if n := len(req.Poll.Choices); n < 2 || n > 10 {
			return errors.New("poll must have between 2 and 10 choices")
		}
		seen := make(map[string]bool, len(req.Poll.Choices))
		for _, ch := range req.Poll.Choices {
			if n := len([]rune(ch)); n < 1 || n > 50 {
				return errors.New("each poll choice must be between 1 and 50 characters")
			}
			// upstream notes/create.ts poll.choices は uniqueItems:true (#2027)。
			if seen[ch] {
				return errors.New("poll choices must be unique")
			}
			seen[ch] = true
		}
		// upstream poll.expiredAfter は minimum:1 (#2027)。
		if req.Poll.ExpiredAfter != nil && *req.Poll.ExpiredAfter < 1 {
			return errors.New("expiredAfter must be at least 1")
		}
	}
	// text のみで成立する note (renote/file/poll 無し) は空白だけの text を弾く
	// (upstream の pattern '[^\\s]+')。text=nil の content-less は service 側の
	// ErrNoteContentRequired に委ねる。
	if req.RenoteID == nil && len(fileIDs) == 0 && req.Poll == nil && req.Text != nil {
		if strings.TrimSpace(*req.Text) == "" {
			return errors.New("text must contain at least one non-whitespace character")
		}
	}
	// visibility / reactionAcceptance enum (#1930)。notes/drafts/create と共有する
	// (#1934)。
	return validateNoteEnums(req.Visibility, req.ReactionAcceptance)
}

// validateNoteEnums checks the visibility / reactionAcceptance enum values shared
// by notes/create and notes/drafts/create (upstream create.ts:132,138 /
// drafts/create.ts:161,168). An empty visibility (the service defaults it to
// public) and a nil reactionAcceptance (no restriction) are allowed; any other
// value outside the enum returns an error that maps to INVALID_PARAM (#1930 / #1934).
func validateNoteEnums(visibility string, reactionAcceptance *string) error {
	switch model.NoteVisibility(visibility) {
	case "", model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers, model.NoteVisibilitySpecified:
	default:
		return errors.New("visibility must be one of public, home, followers, specified")
	}
	if reactionAcceptance != nil {
		switch *reactionAcceptance {
		case "likeOnly", "likeOnlyForRemote", "nonSensitiveOnly", "nonSensitiveOnlyForLocalLikeOnlyForRemote":
		default:
			return errors.New("invalid reactionAcceptance")
		}
	}
	return nil
}

// ShowRequest is the request body for notes/show.
type ShowRequest struct {
	NoteID string `json:"noteId"`
}

// Show handles POST /api/notes/show.
func (h *Handler) Show(c echo.Context) error {
	var req ShowRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}

	viewer := middleware.GetUser(c)
	n, err := h.lookupForShow(req.NoteID)
	if err != nil {
		return apierr.JSONNoSuchNote(c)
	}

	// 未ログイン閲覧の制限 (upstream notes/show.ts:69-79, #1538)。
	//   1. 著者が requireSigninToViewContents=true → CONTENT_RESTRICTED_BY_USER
	//   2. server の ugcVisibilityForVisitor='none' → CONTENT_RESTRICTED_BY_SERVER
	//   3. 'local' かつ remote note → CONTENT_RESTRICTED_BY_SERVER
	// ログイン済み viewer には一切適用しない。
	if viewer == nil {
		if n.User != nil && n.User.RequireSigninToViewContents {
			return c.JSON(http.StatusBadRequest, apierr.Error("CONTENT_RESTRICTED_BY_USER", "This content is not available because the author requires sign-in to view it.", "fbcc002d-37d9-4944-a6b0-d9e29f2d33ab"))
		}
		if v := h.ugcVisibilityNow(); v == "none" || (v == "local" && n.UserHost != nil) {
			return c.JSON(http.StatusBadRequest, apierr.Error("CONTENT_RESTRICTED_BY_SERVER", "This content is not available for visitors on this server.", "145f88d2-b03d-4087-8143-a78928883c4b"))
		}
	}

	packed := entity.PackNoteWithInstance(c.Request().Context(), n, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	s := []entity.NoteEntity{packed}
	h.fieldResolver().Apply(s, viewer)
	if h.isCurrentlyPinned(n) {
		notehide.HidePinnedNotes(viewer, s)
	} else {
		notehide.HideEmbeds(viewer, s)
	}
	// #2106 H1: ID-known doctrine (#799) で note は 200 で返すが、follower/specified を
	// 見られない viewer (非フォロワー/匿名/非対象) には upstream NoteEntityService.hideNote
	// 同様に本文 (text/cw/files/poll/visibleUserIds) を blank する。これを欠くと ID を
	// 知るだけで非公開ノートの本文が full leak する。upstream は shouldHideNote=true の
	// とき 200 + blanked content を返す挙動と一致 (status は #799 で 200 を維持)。
	if h.queryService != nil && !h.queryService.CanSee(viewer, n) {
		entity.HideNoteEntity(&s[0])
	}
	return c.JSON(http.StatusOK, s[0])
}

func (h *Handler) isCurrentlyPinned(n *model.Note) bool {
	if h.piningRepo == nil || n == nil {
		return false
	}
	p, err := h.piningRepo.FindByPair(n.UserID, n.ID)
	return err == nil && p != nil
}

// lookupForShow fetches the note for the /api/notes/show endpoint.
//
// upstream Misskey TS は ID 指定の notes/show では visibility 違反でも
// note を返す (= ID を既に知っている viewer には公開する設計、#799)。
// drop-in 互換のため mk-go も follow-only / specified note を 200 で
// 返す。timeline / replies / renotes 等の二次経路では引き続き
// requireVisible (CanSeeNote) で filter するので、visibility leak は
// "直接 ID を知っている人にだけ" に留まる。
//
// QueryService が wire されていない場合は ErrNoteNotFound を返して安全
// 側に倒す (= production では router.go で常に wire される)。
func (h *Handler) lookupForShow(noteID string) (*model.Note, error) {
	if h.queryService == nil {
		return nil, note.ErrNoteNotFound
	}
	return h.queryService.ShowForAPIOnPrimary(noteID)
}

// DeleteRequest is the request body for notes/delete.
type DeleteRequest struct {
	NoteID string `json:"noteId"`
}

// Delete handles POST /api/notes/delete.
func (h *Handler) Delete(c echo.Context) error {
	user := middleware.GetUser(c)

	var req DeleteRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}

	// moderator / admin は他人の note も削除できる (本家 NoteDeleteService の
	// isModerator bypass、#1538)。checker 未配線時は false (= 著者のみ)。
	isMod := h.moderatorChecker != nil && h.moderatorChecker.IsModerator(user.ID)
	if err := h.deleteService.DeleteAs(user, isMod, req.NoteID); err != nil {
		switch {
		case errors.Is(err, note.ErrNoteNotFound):
			// notes/delete は汎用 UUIDNoSuchNote ではなく endpoint 固有 id を使う
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "490be23f-8c1f-4796-819f-94cb4f9d1630"))
		case errors.Is(err, note.ErrNoteAccessDenied):
			return c.JSON(http.StatusBadRequest, apierr.Error("ACCESS_DENIED", "You are not the author of this note.", "fe8d7103-0ea8-4ec3-814d-f8b401dc69e9"))
		default:
			return apierr.JSONInternalError(c)
		}
	}

	return c.NoContent(http.StatusNoContent)
}

// listRequest is the common pagination request shared by renotes/replies/children.
type listRequest struct {
	NoteID    string `json:"noteId"`
	Limit     *int   `json:"limit"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
}

// normalize validates limit and resolves the date cursors. ok=false は
// limit が upstream paramDef の範囲外で、呼び出し側は 400 を返す。
func (r *listRequest) normalize() bool {
	limit, ok := pagination.ResolveLimit(r.Limit, 10, 100)
	if !ok {
		return false
	}
	r.Limit = &limit
	// sinceDate / untilDate を aidx prefix に正規化 (#1166)。serveList から
	// 呼ばれて Renotes / Replies / Children の 3 handler で一括適用される。
	// **列に入らないカーソルもここで ok=false にする (#3025)。**
	cursorSince, cursorUntil, cursorOK := id.NormalizeCursor(r.SinceID, r.UntilID, r.SinceDate, r.UntilDate)
	if !cursorOK {
		return false
	}
	r.SinceID, r.UntilID = cursorSince, cursorUntil
	return true
}

// Renotes handles POST /api/notes/renotes.
func (h *Handler) Renotes(c echo.Context) error {
	return h.serveList(c, "12908022-2e21-46cd-ba6a-3edaf6093f46", func(viewer *model.User, req listRequest) ([]*model.Note, error) {
		return h.queryService.ListRenotes(viewer, req.NoteID, req.UntilID, req.SinceID, *req.Limit)
	})
}

// Replies handles POST /api/notes/replies.
func (h *Handler) Replies(c echo.Context) error {
	return h.serveList(c, apierr.UUIDNoSuchNote, func(viewer *model.User, req listRequest) ([]*model.Note, error) {
		return h.queryService.ListReplies(viewer, req.NoteID, req.UntilID, req.SinceID, *req.Limit)
	})
}

// Children handles POST /api/notes/children.
func (h *Handler) Children(c echo.Context) error {
	return h.serveList(c, apierr.UUIDNoSuchNote, func(viewer *model.User, req listRequest) ([]*model.Note, error) {
		return h.queryService.ListChildren(viewer, req.NoteID, req.UntilID, req.SinceID, *req.Limit)
	})
}

func (h *Handler) serveList(c echo.Context, noSuchNoteID string, fn func(*model.User, listRequest) ([]*model.Note, error)) error {
	var req listRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	if !req.normalize() {
		return apierr.JSONInvalidParam(c)
	}

	viewer := middleware.GetUser(c)
	notes, err := fn(viewer, req)
	if err != nil {
		if errors.Is(err, note.ErrNoteNotFound) {
			// upstream は notes/renotes 等で NO_SUCH_NOTE に endpoint 固有 id を割り当てる
			return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", noSuchNoteID))
		}
		return apierr.JSONInternalError(c)
	}
	// upstream children/replies/renotes の generateVisibilityQuery は、未ログインの
	// 閲覧者に ugcVisibilityForVisitor=none で `1=0` を足す。対象ノートの存在確認
	// (NO_SUCH_NOTE) は従来どおり先に済ませる (upstream renotes も getNote が先)。
	// `local` は upstream もここでは絞らない。
	if h.visitorHidesAll(viewer) {
		return c.JSON(http.StatusOK, []any{})
	}
	// upstream children/replies/renotes は generateBaseNoteFilteringQuery で
	// 被block / mute / instance-mute を除外する (#1554)。QueryService は
	// 可視性しか見ないため、antennas/roles と同じ post-fetch filter をここで
	// 適用する (blocked-host / suspended は別 follow-up)。
	notes, err = h.applyMuteBlock(viewer, notes)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, viewer))
}

// applyMuteBlock filters out notes the viewer should not see per the mute/block
// dimensions of upstream generateBaseNoteFilteringQuery: muted-user / 被block /
// muted-instance (note/reply/renote の author いずれか一致で除外)。
// **channel-mute も適用する。upstream より厳しい側への意図的な乖離。**
// upstream の generateBaseNoteFilteringQuery (QueryService.ts:93-112) は
// blocked-host / suspended / muted-user / blocked-user だけで、channel-mute は
// timeline 系にしか無い。mk-go はミュートしたチャンネルの投稿が
// children/replies/renotes から出てくるほうを問題とみなして渡している。
// **以前この GoDoc は「nil を渡して適用しない」と書いていたが実装と逆だった。**
// blocked-host / suspended は別 follow-up。blockingRepo 未配線 / viewer nil なら
// no-op。fail-closed: lookup / filter エラーは呼び出し側で 500 にする。
func (h *Handler) applyMuteBlock(viewer *model.User, notes []*model.Note) ([]*model.Note, error) {
	// muted-user / blocked-user / muted-channel / muted-instances は viewer 視点
	// なので anonymous では対象外。
	if viewer != nil && h.blockingRepo != nil {
		// channelMutingRepo を渡さないと MutedChannelIDs が空のままになり、
		// チャンネルミュートが post-fetch 側で一切効かない。
		sets, err := notesfilter.LoadMuteBlockSets(viewer, h.mutingRepo, h.blockingRepo, h.channelMutingRepo, h.userRepo)
		if err != nil {
			return nil, err
		}
		notes, err = notesfilter.ApplyMuteBlockChannel(notes, sets, h.noteRepo)
		if err != nil {
			return nil, err
		}
	}
	// suspended-user / blocked-host は upstream generateBaseNoteFilteringQuery と
	// 同じく viewer 不問で常に除外する (#1783。children/replies/renotes/mentions/
	// search-by-tag/featured の post-fetch 経路で漏れていた)。
	notes = notesfilter.ApplySuspended(notes)
	if h.metaRepo != nil {
		blocked, err := h.blockedHosts()
		if err != nil {
			return nil, err
		}
		notes = notesfilter.ApplyBlockedHosts(notes, blocked)
	}
	return notes, nil
}

// SetMetaRepo wires a MetaRepository used for the blocked-host filter on the
// post-fetch list endpoints (#1783, upstream generateBlockedHostQueryForNote)。
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
	return meta.BlockedHosts, nil
}

// applyThreadMute drops notes belonging to a thread the viewer has muted
// (upstream generateMutedNoteThreadQuery、#1554)。threadMutingRepo 未配線 /
// viewer nil なら no-op。fail-closed: lookup エラーは呼び出し側で 500 にする。
func (h *Handler) applyThreadMute(viewer *model.User, notes []*model.Note) ([]*model.Note, error) {
	if viewer == nil || h.threadMutingRepo == nil {
		return notes, nil
	}
	ids, err := h.threadMutingRepo.ListMutedThreadIDs(viewer.ID)
	if err != nil {
		return nil, err
	}
	return notesfilter.ApplyThreadMute(notes, ids), nil
}

// SearchRequest is the request body for notes/search.
//
// Misskey 本家と互換のフィールド構成。`sinceDate` / `untilDate` (unix milli)
// が指定されたときは ID generator で対応する note ID に変換し、`sinceId` /
// `untilId` のフォールバックとして使う。`host == "."` はローカル限定検索。
type SearchRequest struct {
	Query     string `json:"query"`
	Limit     *int   `json:"limit"`
	Offset    int    `json:"offset"`
	SinceID   string `json:"sinceId"`
	UntilID   string `json:"untilId"`
	SinceDate *int64 `json:"sinceDate"`
	UntilDate *int64 `json:"untilDate"`
	UserID    string `json:"userId"`
	ChannelID string `json:"channelId"`
	Host      string `json:"host"`
	// rangeStartAt / rangeEndAt は投稿日時範囲 (epoch ms、upstream #16119 #2069)。
	// sinceDate/untilDate (cursor 正規化用) とは別物で、cursor と独立に AND される。
	RangeStartAt *int64 `json:"rangeStartAt"`
	RangeEndAt   *int64 `json:"rangeEndAt"`
}

// Search handles POST /api/notes/search.
func (h *Handler) Search(c echo.Context) error {
	var req SearchRequest
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	if h.searchService == nil {
		return apierr.JSONInternalError(c)
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

	// upstream search.ts は requireCredential:false で匿名も handler に到達させ、
	// canSearchNotes role policy が false なら UNAVAILABLE を返す (#1783)。匿名は
	// base policy で評価する (GetUserPolicies("") = base)。以前は route 側で
	// RequireAuth(401) + RequireRolePolicy(403 ROLE_PERMISSION_DENIED) を課して
	// いた。policyProvider 未配線時 (test) は gate を skip。
	if h.policyProvider != nil {
		uid := ""
		if viewer != nil {
			uid = viewer.ID
		}
		if v, ok := h.policyProvider.GetUserPolicies(uid)["canSearchNotes"].(bool); !ok || !v {
			return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Search of notes unavailable.", "0b44998d-77aa-4427-80d0-d2c9b8523011"))
		}
	}

	// 未ログインの閲覧者には ugcVisibilityForVisitor を掛ける (upstream
	// SearchService: SQL 経路は generateVisibilityQuery / generateUgcVisibilityQueryForVisitor、
	// Meilisearch 経路は `none` で [] と `local` で userHost IS NULL)。`none` は
	// provider を呼ばずに返す。空クエリは従来どおり provider 側の INVALID_PARAM に任せる。
	localUsersOnly := false
	if viewer == nil && req.Query != "" {
		policy := h.ugcVisibilityNow()
		if ugcvisibility.HidesAll(policy) {
			return c.JSON(http.StatusOK, []any{})
		}
		localUsersOnly = policy == ugcvisibility.Local
	}

	notes, err := h.searchService.SearchNote(
		viewer,
		req.Query,
		search.SearchOpts{
			UserID:         req.UserID,
			ChannelID:      req.ChannelID,
			Host:           req.Host,
			Offset:         req.Offset,
			RangeStartAt:   req.RangeStartAt,
			RangeEndAt:     req.RangeEndAt,
			LocalUsersOnly: localUsersOnly,
		},
		search.Pagination{
			UntilID: untilID,
			SinceID: sinceID,
			Limit:   limit,
		},
	)
	if err != nil {
		// 空クエリは invalidParam として返す。
		// それ以外のエラー (DB障害など) はinternalErrorで返す。
		if errors.Is(err, search.ErrEmptyQuery) {
			return apierr.JSONInvalidParam(c)
		}
		// fulltextSearch.provider="none" 時は ErrUnavailable を 400
		// UNAVAILABLE に翻訳する (= upstream Misskey TS と同 shape、#877)。
		if errors.Is(err, search.ErrUnavailable) {
			return c.JSON(http.StatusBadRequest, apierr.Error("UNAVAILABLE", "Search of notes unavailable.", "0b44998d-77aa-4427-80d0-d2c9b8523011"))
		}
		return apierr.JSONInternalError(c)
	}
	// upstream searchNoteByLike / searchNoteByMeilisearch は generateBaseNoteFilteringQuery
	// で suspended-user / blocked-host (匿名でも) と被block/mute/renote-mute user の note を
	// 除外する。provider は visibility しか掛けないため、search-by-tag 等と同じく post-fetch で
	// applyMuteBlock を適用する (#1938)。upstream search は thread-mute を掛けないため
	// applyThreadMute は不要。
	notes, err = h.applyMuteBlock(viewer, notes)
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, viewer))
}

// State handles POST /api/notes/state.
func (h *Handler) State(c echo.Context) error {
	var req ShowRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	viewer := middleware.GetUser(c)
	st, err := h.queryService.State(viewer, req.NoteID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。`State` は #2799 で
		// raw DB error も返すようになったので、sentinel かどうかで分ける。
		if !errors.Is(err, note.ErrNoteNotFound) {
			return apierr.JSONInternalError(c)
		}
		return apierr.JSONNoSuchNote(c)
	}
	return c.JSON(http.StatusOK, st)
}

// ConversationRequest is the request body for notes/conversation.
type ConversationRequest struct {
	NoteID string `json:"noteId"`
	Limit  *int   `json:"limit"`
	Offset int    `json:"offset"`
}

// Conversation handles POST /api/notes/conversation.
func (h *Handler) Conversation(c echo.Context) error {
	var req ConversationRequest
	if err := c.Bind(&req); err != nil || req.NoteID == "" {
		return apierr.JSONInvalidParam(c)
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	if req.Offset < 0 {
		req.Offset = 0
	}

	viewer := middleware.GetUser(c)
	// upstream conversation.ts は ugcVisibilityForVisitor=none の未ログインの
	// 閲覧者に、ノートを引く前に [] を返す。`local` は upstream も絞らない。
	if h.visitorHidesAll(viewer) {
		return c.JSON(http.StatusOK, []any{})
	}
	notes, err := h.queryService.Conversation(viewer, req.NoteID, limit, req.Offset)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。`Conversation` は #2799 で
		// raw DB error も返すようになったので、sentinel かどうかで分ける。
		if !errors.Is(err, note.ErrNoteNotFound) {
			return apierr.JSONInternalError(c)
		}
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_NOTE", "No such note.", "e1035875-9551-45ec-afa8-1ded1fcb53c8"))
	}
	// Conversation は非可視な祖先も raw に返すので、ここで CanSee 判定して
	// 非可視 note を hideNote stub する (upstream packMany(_, me) の hideNote 挙動、
	// #1948-17)。index ずれ (ApplyHardMute の drop) に強いよう note ID で突合する。
	hidden := make(map[string]struct{})
	for _, n := range notes {
		if !h.queryService.CanSee(viewer, n) {
			hidden[n.ID] = struct{}{}
		}
	}
	packed := h.packMany(c.Request().Context(), notes, viewer)
	if len(hidden) > 0 {
		for i := range packed {
			if _, ok := hidden[packed[i].ID]; ok {
				entity.HideNoteEntity(&packed[i])
			}
		}
	}
	return c.JSON(http.StatusOK, packed)
}

// BulkShow handles POST /api/notes.
//
// #2106 L4 / #2215: hybrid endpoint。upstream notes.ts は {local,reply,renote,withFiles,
// poll,limit,sinceId/untilId/sinceDate/untilDate} で public(visibility=public,
// localOnly=FALSE) note の一覧を時系列ページングする公開タイムライン的 endpoint。mk-go は
// 加えて独自拡張の noteIds bulk lookup も同 endpoint で受ける: noteIds 指定時は bulk
// (visibility filter 付き)、不在時は upstream 互換の public note 一覧を返す。これで
// misskey-js SDK の {local:true,limit:30} 呼び出しも、既存の {noteIds:[...]} 呼び出しも
// 両立する。
func (h *Handler) BulkShow(c echo.Context) error {
	var req struct {
		NoteIDs []string `json:"noteIds"`
		// upstream notes.ts public-note timeline params (noteIds 不在時に使用)。
		Local     bool   `json:"local"`
		Reply     *bool  `json:"reply"`
		Renote    *bool  `json:"renote"`
		WithFiles *bool  `json:"withFiles"`
		Poll      *bool  `json:"poll"`
		Limit     *int   `json:"limit"`
		SinceID   string `json:"sinceId"`
		UntilID   string `json:"untilId"`
		SinceDate *int64 `json:"sinceDate"`
		UntilDate *int64 `json:"untilDate"`
	}
	if err := c.Bind(&req); err != nil {
		return apierr.JSONInvalidParam(c)
	}
	viewer := middleware.GetUser(c)

	// noteIds 指定時は mk-go 拡張の bulk lookup。visibility チェックを通して非公開ノートの
	// 漏洩を防ぐ。queryService 未配線時は fail-closed で空配列 (#509 / #505)。
	if len(req.NoteIDs) > 0 {
		if len(req.NoteIDs) > 100 {
			req.NoteIDs = req.NoteIDs[:100]
		}
		notes, err := h.noteRepo.FindManyByIDsWithUser(req.NoteIDs)
		if err != nil || h.queryService == nil {
			return c.JSON(http.StatusOK, []any{})
		}
		notes = h.queryService.FilterVisible(viewer, notes)
		// mk-go 独自の noteIds 経路にも、一覧側と同じ ugcVisibilityForVisitor の
		// 方針を掛ける (upstream には該当する経路が無い)。
		notes = h.filterVisitorUGC(viewer, notes)
		return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, viewer))
	}

	// noteIds 不在時は upstream notes.ts の public note 一覧。public note のみなので追加の
	// visibility filter は不要 (upstream も requireCredential:false)。未ログインの閲覧者
	// 向けの ugcVisibilityForVisitor は下で掛ける。
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	sinceID, untilID, cursorOK := id.NormalizeCursor(req.SinceID, req.UntilID, req.SinceDate, req.UntilDate)
	if !cursorOK {
		return apierr.JSONInvalidParam(c)
	}
	// upstream notes.ts は未ログインの閲覧者に generateUgcVisibilityQueryForVisitor
	// を掛ける。`local` の条件 (`userHost IS NULL`) は local パラメータと同じなので
	// それを立てて SQL で絞る。
	local := req.Local
	if viewer == nil {
		policy := h.ugcVisibilityNow()
		if ugcvisibility.HidesAll(policy) {
			return c.JSON(http.StatusOK, []any{})
		}
		if policy == ugcvisibility.Local {
			local = true
		}
	}
	notes, err := h.noteRepo.ListPublicNotes(model.PublicNotesFilter{
		Local:     local,
		Reply:     req.Reply,
		Renote:    req.Renote,
		WithFiles: req.WithFiles,
		Poll:      req.Poll,
	}, limit, sinceID, untilID)
	if err != nil {
		return c.JSON(http.StatusOK, []any{})
	}
	return c.JSON(http.StatusOK, h.packMany(c.Request().Context(), notes, viewer))
}

// packReferencedNote packs a single note referenced by a draft (reply/renote)
// for the viewer. upstream の `noteEntityService.pack(noteId, me, {skipHide:false})`
// に合わせ、(1) timeline の hard-mute drop は適用しない (upstream pack には
// hard-mute が無い)、(2) viewer が閲覧不可な note (followers 非フォロワー /
// specified 対象外) は entity.HideNoteEntity で本文を blank し isHidden を立てる
// (upstream shouldHideNote→hideNote)。draft は owner-only だが、replyId/renoteId に
// 任意の note ID を入れて非可視 note の本文を読み出す leak を塞ぐ (#1948-19)。
func (h *Handler) packReferencedNote(ctx context.Context, n *model.Note, viewer *model.User, detail bool) entity.NoteEntity {
	packed := entity.PackNotes(ctx, []*model.Note{n}, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldResolver().Apply(packed, viewer)
	notehide.HideEmbeds(viewer, packed)
	out := packed[0]
	// queryService 未配線時は fail-closed で hide (= 検証不能なら隠す)。
	if h.queryService == nil || !h.queryService.CanSee(viewer, n) {
		entity.HideNoteEntity(&out)
	}
	// PackNotes は top-level を常に detail:true で pack する。draft の reply は
	// upstream で detail:false なので detail block (clippedCount/poll/myReaction/
	// nested reply/renote embed) を落とす (#2016。reactions 自体は detail 外で残す)。
	if !detail {
		out.ClippedCount = nil
		out.Poll = nil
		out.MyReaction = nil
		out.Reply = nil
		out.Renote = nil
	}
	return out
}

// packMany serializes a list of notes into NoteEntity objects.
// driveFileRepoが設定されている場合、ファイル情報を解決してFilesに含める。
// viewerがnon-nilの場合、myReactionなどのviewer依存フィールドも解決する。
//
// Files / MyReaction / Channel の解決は entity.NoteFieldResolver に切り出して
// あり、他の PackNotes 利用 handler (antennas / users / pinned 等) と共通化
// している (#426)。
//
// 全 list endpoint (TL / Search / Conversation / Mentions / UserList /
// SearchByTag) が本関数を経由するので、hardMutedWords の filter (#787) は
// PackNotes に渡す前に一括で適用する。viewer == nil / userRepo 未配線 /
// 規則無し のいずれも no-op に倒す best-effort 設計。
//
// 注意: filter を意図的に bypass したい admin 系 path (例: モデレーション目的で
// 隠さず全件を見せたい / pinned notes 等) は本関数を経由せず entity.PackNotes
// を直接呼ぶ慣習。pinned (i/show, users/show) と i/favorites は upstream に
// 揃えて既に packMany を経由していない。
func (h *Handler) packMany(ctx context.Context, notes []*model.Note, viewer *model.User) []entity.NoteEntity {
	notes = notesfilter.ApplyHardMute(h.userRepo, viewer, notes)
	out := entity.PackNotes(ctx, notes, h.idGen, h.instanceLookup(), h.emojiLookup(), h.reactionReader())
	h.fieldResolver().Apply(out, viewer)
	notehide.HideEmbeds(viewer, out)
	return out
}

// fieldResolver assembles the per-handler NoteFieldResolver from already-wired
// repositories. Returns nil-safe (Apply is no-op when none of the repos are
// configured)。
func (h *Handler) fieldResolver() *entity.NoteFieldResolver {
	return entity.NewNoteFieldResolver(
		nilOrDriveFile(h.driveFileRepo),
		nilOrDriveFolder(h.driveFolderRepo),
		nilOrUser(h.userRepo),
		nilOrNoteReaction(h.noteReactionRepo),
		nilOrChannel(h.channelRepo),
		h.idGen,
	)
}

// nilOr* helpers preserve the「未配線時は interface も nil で渡す」契約。
// 直接代入だと typed-nil が non-nil interface になり resolver の lookup nil
// チェックを擦り抜けてしまうため。
func nilOrDriveFile(r repository.DriveFileRepository) entity.DriveFileLookup {
	if r == nil {
		return nil
	}
	return r
}
func nilOrDriveFolder(r repository.DriveFolderRepository) entity.DriveFolderLookup {
	if r == nil {
		return nil
	}
	return r
}
func nilOrUser(r repository.UserRepository) entity.FileOwnerLookup {
	if r == nil {
		return nil
	}
	return r
}
func nilOrNoteReaction(r repository.NoteReactionRepository) entity.NoteReactionLookup {
	if r == nil {
		return nil
	}
	return r
}
func nilOrChannel(r repository.ChannelRepository) entity.ChannelLookup {
	if r == nil {
		return nil
	}
	return r
}

// PackForEmbed packs a note for the anonymous embed page (#2389).
//
// viewer は nil 固定。埋め込みは認証を伴わないので、閲覧者依存のフィールドを
// 解決してはいけない。packReferencedNote を経由するのは、可視性の判断を
// **既存の 1 箇所に寄せる**ため。ここで独自に判定を書くと、CanSee /
// HideEmbeds に後から入る修正が embed だけ取り残される。
//
// viewer=nil のとき queryService.CanSee は public / home 以外を false にし、
// HideEmbeds は renote / reply の埋め込みを同じ深さで gate する。
func (h *Handler) PackForEmbed(ctx context.Context, n *model.Note) entity.NoteEntity {
	return h.packReferencedNote(ctx, n, nil, true)
}

// HasPolicyProvider reports whether the role policy provider was wired.
//
// 未配線だと timeline の可視性 gate (`timelineAvailable`) に加え、
// `canSearchNotes` (`handler.go`)・`canUseTranslator` (`handler_extra.go`)・
// `noteDraftLimit` (`handler_drafts.go`) も同時に素通しになる。**1 つの nil で
// 4 箇所**。起動時検査に使う (#2683)。
func (h *Handler) HasPolicyProvider() bool { return h.policyProvider != nil }

// HasDriveFileRepo / HasMetaRepo / HasBlockingRepo / HasMutingRepo /
// HasChannelMutingRepo / HasUserFollowingRepo / HasUGCVisibility report whether
// the visibility gates were wired (#2708).
//
// 未配線時にそれぞれ:
//
//   - driveFileRepo: 下書きの添付で他人の drive ファイル ID を指定でき
//     (`handler_drafts.go` の所有権検査が nil で true を返す)、加えて
//     `fieldResolver()` が drive を引けなくなるので **全 note の files が
//     空配列**になる。下書きから note への昇格は `note.driveFileRepo` の
//     checkFileIDs を通るので、他人のファイルの実体までは出ない
//   - metaRepo: blocked-host の note が post-fetch 経路で漏れる
//   - blockingRepo: ブロック / ミュート / チャンネルミュートの post-fetch
//     filter が丸ごと skip される (`applyMuteBlock` は `blockingRepo != nil`
//     で全体を gate している)
//   - mutingRepo: post-fetch filter のミュート集合と、
//     `notes/polls/recommendation` が SQL へ渡す除外集合が空になる。timeline の
//     SQL 側は `UseMutingSubquery` が別に効くので残る
//   - channelMutingRepo: チャンネルミュートが**一切**効かない。別ソースが
//     無いので in-memory・SQL (`TimelineFilter.MutedChannelIDs`)・user-list
//     timeline・HTL の followed-channel 差し引き の全経路が死ぬ
//   - userFollowingRepo: followers 限定投稿への返信が非フォロワーの HTL / STL
//     に出る
//   - ugcVisibility: 匿名 visitor への露出を `ugcVisibilityForVisitor` で
//     gate できない
func (h *Handler) HasDriveFileRepo() bool { return h.driveFileRepo != nil }

// HasMetaRepo reports whether the meta repository was wired.
func (h *Handler) HasMetaRepo() bool { return h.metaRepo != nil }

// HasBlockingRepo reports whether the blocking repository was wired.
func (h *Handler) HasBlockingRepo() bool { return h.blockingRepo != nil }

// HasMutingRepo reports whether the muting repository was wired.
func (h *Handler) HasMutingRepo() bool { return h.mutingRepo != nil }

// HasChannelMutingRepo reports whether the channel muting repository was wired.
func (h *Handler) HasChannelMutingRepo() bool { return h.channelMutingRepo != nil }

// HasRenoteMutingRepo reports whether the renote muting repository was wired.
//
// 未配線だと `loadRenoteMutedUserIDs` が nil を返し、renote ミュートが read 時に
// 一切効かない (#2709 review)。
func (h *Handler) HasRenoteMutingRepo() bool { return h.renoteMutingRepo != nil }

// HasUserFollowingRepo reports whether the following repository was wired.
func (h *Handler) HasUserFollowingRepo() bool { return h.userFollowingRepo != nil }

// HasUGCVisibility reports whether the visitor content visibility policy was set.
//
// **空文字は gate 無効**と同義 (`"none"` でも `"local"` でもないので素通し)。
// 列は `NOT NULL DEFAULT 'local'` なので、空になるのは配線が漏れたときだけ。
func (h *Handler) HasUGCVisibility() bool { return h.ugcVisibilityNow() != "" }
