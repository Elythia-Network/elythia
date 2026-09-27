// Package note provides core business logic services for notes.
package note

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub/mfm"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/hashtag"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/keyword"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"

	corechannel "github.com/shiroha-a/mk/internal/core/channel"
	"github.com/shiroha-a/mk/internal/core/role"
)

// featured ランキング更新の sampling rate / note 年齢上限 (upstream
// NoteCreateService.incRenoteCount と同値)。renote は score=5。
const (
	featuredSampleRate = 0.3
	featuredMaxNoteAge = 3 * 24 * time.Hour
)

// Errors returned by NoteCreateService.
var (
	// ErrNoteContentRequired is returned when text, fileIds, renoteId and poll are all empty.
	ErrNoteContentRequired = errors.New("text, fileIds, or renoteId is required")
	// ErrReplyTargetNotFound is returned when the replyId references a missing note.
	ErrReplyTargetNotFound = errors.New("reply target not found")
	// ErrRenoteTargetNotFound is returned when the renoteId references a missing note.
	ErrRenoteTargetNotFound = errors.New("renote target not found")
	// ErrCannotReplyToInvisibleNote is returned when the replier cannot see the reply target.
	ErrCannotReplyToInvisibleNote = errors.New("cannot reply to this note")
	// ErrCannotRenoteInvisibleNote is returned when the renoter cannot see the renote target.
	ErrCannotRenoteInvisibleNote = errors.New("cannot renote this note")
	// ErrChannelNotFound is returned when the channelId references a missing
	// (or archived) channel. It is the same value as channel.ErrChannelNotFound
	// so that a ChannelHook can report not-found without importing core/note.
	//
	// hook が返すエラーのうち not-found だけを NO_SUCH_CHANNEL にし、DB 障害は
	// そのまま返すために同一の値にしてある (#2792)。core/channel は core/role
	// 経由で core/note から到達されるので、逆向きに core/note を import できない。
	ErrChannelNotFound = corechannel.ErrChannelNotFound
	// ErrCannotRenoteToAPureRenote is returned when the renote target is a pure
	// renote (renote of another note with no original content). Misskey disallows
	// renoting a pure renote to avoid redundant propagation chains.
	ErrCannotRenoteToAPureRenote = errors.New("cannot renote a pure renote")
	// ErrCannotReplyToAPureRenote is returned when the reply target is a pure renote.
	ErrCannotReplyToAPureRenote = errors.New("cannot reply to a pure renote")
	// ErrCannotReplyToSpecifiedVisibility is returned when replying to a
	// "specified" visibility note with a wider visibility (non-specified).
	ErrCannotReplyToSpecifiedVisibility = errors.New("cannot reply to specified visibility note with extended visibility")
	// ErrYouHaveBeenBlocked is returned when the target user (reply/renote target)
	// has blocked the actor.
	ErrYouHaveBeenBlocked = errors.New("you have been blocked by the target user")
	// ErrCannotCreateAlreadyExpiredPoll is returned when poll.ExpiresAt is in
	// the past at creation time.
	ErrCannotCreateAlreadyExpiredPoll = errors.New("poll is already expired")
	// ErrNoSuchFile is returned when one or more fileIds reference missing files.
	ErrNoSuchFile = errors.New("some files are not found")
	// ErrCannotRenoteOutsideOfChannel is returned when the renote target belongs
	// to a channel that disallows renoting outside of its context.
	ErrCannotRenoteOutsideOfChannel = errors.New("cannot renote outside of channel")
	// ErrContainsProhibitedWords is returned when the note text contains a word
	// in meta.prohibitedWords.
	ErrContainsProhibitedWords = errors.New("note contains prohibited words")
	// ErrContainsTooManyMentions is returned when the note exceeds the role
	// policy's mentionLimit.
	ErrContainsTooManyMentions = errors.New("note contains too many mentions")
)

// CreateInput is the input parameter for CreateService.Create.
type CreateInput struct {
	User               *model.User
	Text               *string
	CW                 *string
	Visibility         model.NoteVisibility
	VisibleUserIDs     []string
	LocalOnly          bool
	ReactionAcceptance *string
	FileIDs            []string
	ReplyID            *string
	RenoteID           *string
	ChannelID          *string
	Poll               *PollInput
	// NoExtract* suppress automatic mention/hashtag/emoji extraction from text.
	// upstream notes/create.ts は apMentions/apHashtags/apEmojis に [] を渡して
	// 抽出を抑止する (= 主に AP inbox / bridge 経由の二重抽出防止、#1538)。
	NoExtractMentions bool
	NoExtractHashtags bool
	NoExtractEmojis   bool
}

// PollInput represents the poll part of a create note input.
type PollInput struct {
	Choices   []string
	Multiple  bool
	ExpiresAt *time.Time
}

// TimelineFanoutHook is invoked after a note has been persisted so that the
// timeline subsystem can deliver the note to interested feeds. パッケージ間の
// 循環依存を避けるためinterfaceで受け取る (実装は core/timeline)。
type TimelineFanoutHook interface {
	OnNoteCreated(note *model.Note, author *model.User)
}

// NotificationHook is invoked after a note has been persisted so that the
// notification subsystem can create notification entries for mentions, replies,
// renotes, etc. パッケージ間の循環依存を避けるためinterfaceで受け取る。
type NotificationHook interface {
	OnNoteCreated(note *model.Note, author *model.User, replyTarget, renoteTarget *model.Note)
}

// FederationHook is invoked after a note has been persisted so that the
// ActivityPub layer can deliver the note to remote followers.
// パッケージ間の循環依存を避けるためinterfaceで受け取る (実装は core/federation)。
type FederationHook interface {
	OnNoteCreated(note *model.Note, author *model.User)
}

// ChannelHook is invoked when a note is posted to a channel so the channel
// service can validate existence ahead of insertion and bump counters / the
// last-noted timestamp afterwards. パッケージ間の循環依存を避けるため
// interface で受け取る (実装は core/channel)。
type ChannelHook interface {
	// EnsureChannelExists is called before insertion. Returning a non-nil
	// error aborts the note creation.
	EnsureChannelExists(channelID string) error
	// OnNotePosted is called after the note row has been persisted. noteID
	// / authorID are passed so the hook can fan out per-follower unread
	// tracking (hasUnreadChannel) while skipping self-posts.
	OnNotePosted(channelID, noteID, authorID string)
}

// AntennaHook is invoked after a note has been persisted so antenna service
// can fan it out into matching antenna timelines. パッケージ間の循環依存を
// 避けるため interface で受け取る (実装は core/antenna)。
type AntennaHook interface {
	OnNoteCreated(note *model.Note, author *model.User)
}

// IndexHook is invoked after a note has been persisted (or deleted) so the
// search subsystem can update its full-text index. 失敗してもメイン処理に
// 影響させたくないため、戻り値は捨てる前提のベストエフォート呼び出し。
// パッケージ間の循環依存を避けるため interface で受け取る (実装は core/search)。
type IndexHook interface {
	OnNoteCreated(note *model.Note)
	OnNoteDeleted(note *model.Note)
}

// ChartHook is invoked after a note has been persisted so the chart
// subsystem can fan the event into NotesChart, PerUserNotesChart,
// InstanceChart and ActiveUsersChart. パッケージ間の循環依存を避ける
// ため interface で受け取る (実装は core/chart/charthook)。
//
// federation 経由のリモートノートに対しては同 shape の interface
// `core/federation.NoteChartHook` が processor.handleCreate /
// handleAnnounce 側で同じ役目を果たす (#1156)。命名が分かれているのは
// federation パッケージ内に既存の `ChartHook` (= 新規 remote user 用、
// OnRemoteUserCreated) があるため。配線対象は同じ `charthook.Hooks` 集約で、
// router.go から両 setter (note.SetChartHook / federation.SetNoteChartHook)
// に渡される。
type ChartHook interface {
	OnNoteCreated(note *model.Note)
}

// HashtagHook is invoked after a note has been persisted so the hashtag
// subsystem can record mentionedUsersCount / mentionedUserIds (#680)。
// 各 tag に対して per-user dedup された upsert を実行する。失敗は best-effort
// で握り潰す (note 作成自体は成功扱い)。
//
// 実装契約 (#719): OnNoteCreated は **non-blocking** でなければならず、
// repo 書き込みが必要な場合は実装側で goroutine を起こすこと。caller
// (note_create_service / federation/resolver) は他 hook と異なり safeGo で
// wrap せず直接呼び出すので、panic recovery も実装側の責務。本 contract は
// federation 経路で tag 数 N に比例して inbox drain time が直列化する退行を
// 構造的に防ぐためにある。
type HashtagHook interface {
	OnNoteCreated(note *model.Note, author *model.User)
}

// WebhookHook is invoked after a note has been persisted so that user
// webhooks subscribed to `note` / `reply` / `renote` / `mention` events can
// deliver the packed note to external endpoints. 循環依存を避けるため
// interface で受け取る (実装は core/webhook)。
type WebhookHook interface {
	OnNoteCreated(note *model.Note, author *model.User, replyTarget, renoteTarget *model.Note)
}

// MainStreamPublisher emits real-time events to a single target user's `main`
// WebSocket channel. Used here to deliver `reply`, `renote`, and `mention`
// events so the frontend can reflect note-creation interactions immediately.
// 循環依存を避けるためinterfaceで受け取る(実装はinternal/stream)。
type MainStreamPublisher interface {
	PublishMainEvent(userID, eventType string, body any)
}

// SilencingProvider reports whether a user has been silenced by their role
// policies (= canPublicNote=false). When wired, CreateService demotes
// `visibility=public` notes from non-channel posts to `home` to match
// upstream Misskey TS NoteCreateService behaviour (#1024). 循環依存を避ける
// ため interface で受け取り、実装は core/role.Service が提供する。
type SilencingProvider interface {
	IsSilenced(userID string) bool
}

// RolePolicyProvider abstracts role-policy lookup for the mentionLimit gate
// (#2321)。循環依存を避けるため interface で受け取り、実装は core/role.Service。
type RolePolicyProvider interface {
	GetUserPolicies(userID string) map[string]any
}

// CreateService provides note creation logic.
type CreateService struct {
	noteRepo repository.NoteRepository
	// rolePolicyProvider は mentionLimit の gate に使う (#2321)。nil なら
	// DefaultMentionLimit にフォールバックする。
	rolePolicyProvider RolePolicyProvider
	pollRepo           repository.PollRepository
	followingRepo      repository.FollowingRepository
	idGen              id.Generator
	fanoutHook         TimelineFanoutHook
	// materializer はリレー由来で DB に無い返信 / renote 対象を昇格させる
	// (#2332)。note.replyId / renoteId は note への外部キーなので、行が無いと
	// INSERT が失敗する。
	materializer        NoteMaterializer
	notificationHook    NotificationHook
	federationHook      FederationHook
	channelHook         ChannelHook
	antennaHook         AntennaHook
	indexHook           IndexHook
	chartHook           ChartHook
	hashtagHook         HashtagHook
	webhookHook         WebhookHook
	mainStreamPublisher MainStreamPublisher
	userRepo            repository.UserRepository
	blockingRepo        repository.BlockingRepository
	driveFileRepo       repository.DriveFileRepository
	metaRepo            repository.MetaRepository
	channelRepo         repository.ChannelRepository
	silencingProvider   SilencingProvider
	threadMuteRepo      repository.NoteThreadMutingRepository
	featuredRanking     FeaturedRanking
	// randFn は featured ランキング更新の 30% sampling 用。テストで固定する。
	randFn func() float64
}

// FeaturedRanking abstracts the engagement ranking store (#1687). 循環依存回避の
// ため narrow interface で受け取る (実装は core/featured.Service)。renote は
// renote 対象 note の global / in-channel / per-user ranking を boost する。
type FeaturedRanking interface {
	UpdateGlobalNotesRanking(ctx context.Context, noteID string, score float64) error
	UpdateInChannelNotesRanking(ctx context.Context, channelID, noteID string, score float64) error
	UpdatePerUserNotesRanking(ctx context.Context, userID, noteID string, score float64) error
}

// SetFeaturedRanking wires the engagement ranking store so renotes boost the
// renoted note's featured ranking (#1687). nil 注入時 (= 未配線 / test) は skip。
func (s *CreateService) SetFeaturedRanking(r FeaturedRanking) {
	s.featuredRanking = r
	if s.randFn == nil {
		s.randFn = rand.Float64
	}
}

// SetUserRepo attaches a UserRepository for resolving mention usernames to IDs.
func (s *CreateService) SetUserRepo(r repository.UserRepository) {
	s.userRepo = r
}

// SetRolePolicyProvider wires role-policy lookup so note creation honours the
// per-user `mentionLimit` policy (#2321). nil (未配線 / test) のときは
// DefaultMentionLimit にフォールバックする。
func (s *CreateService) SetRolePolicyProvider(p RolePolicyProvider) {
	s.rolePolicyProvider = p
}

// SetSilencingProvider attaches a SilencingProvider so public-visibility
// notes from `canPublicNote=false` users are demoted to `home` (#1024).
// nil 時は demotion skip (= 旧挙動)。
func (s *CreateService) SetSilencingProvider(p SilencingProvider) {
	s.silencingProvider = p
}

// SetBlockingRepo attaches a BlockingRepository for detecting reply/renote
// block violations. When unset the block check is skipped (backward compat).
func (s *CreateService) SetBlockingRepo(r repository.BlockingRepository) {
	s.blockingRepo = r
}

// SetThreadMutingRepo attaches a NoteThreadMutingRepository so reply/mention
// main-stream events are suppressed for recipients who thread-muted the thread,
// matching upstream NoteCreateService (#1954). nil disables the gate.
func (s *CreateService) SetThreadMutingRepo(r repository.NoteThreadMutingRepository) {
	s.threadMuteRepo = r
}

// SetDriveFileRepo attaches a DriveFileRepository for validating fileIds.
// When unset fileId validation is skipped (backward compat).
func (s *CreateService) SetDriveFileRepo(r repository.DriveFileRepository) {
	s.driveFileRepo = r
}

// SetMetaRepo attaches a MetaRepository for prohibited-word detection.
// When unset the check is skipped.
func (s *CreateService) SetMetaRepo(r repository.MetaRepository) {
	s.metaRepo = r
}

// SetChannelRepo attaches a ChannelRepository for the "renote outside of
// channel" rule evaluation. Required alongside ChannelHook for strict checks.
func (s *CreateService) SetChannelRepo(r repository.ChannelRepository) {
	s.channelRepo = r
}

// NewCreateService creates a new CreateService.
// followingRepo は省略可 (nil)。指定された場合、followers可視性ノートへの
// reply/renoteで閲覧権限を厳格にチェックする。
// fanoutHookも省略可 (nil)。設定されていればnote作成成功時にコールバックされる。
func NewCreateService(
	noteRepo repository.NoteRepository,
	pollRepo repository.PollRepository,
	idGen id.Generator,
	followingRepo repository.FollowingRepository,
) *CreateService {
	return &CreateService{
		noteRepo:      noteRepo,
		pollRepo:      pollRepo,
		followingRepo: followingRepo,
		idGen:         idGen,
	}
}

// SetFanoutHook attaches a TimelineFanoutHook to be invoked on note creation.
// 後付けセッターにすることで、Wireのような循環依存問題を起こさず、テストでも
// 簡単に差し替え可能にする。
func (s *CreateService) SetFanoutHook(h TimelineFanoutHook) {
	s.fanoutHook = h
}

// SetNotificationHook attaches a NotificationHook invoked on note creation.
func (s *CreateService) SetNotificationHook(h NotificationHook) {
	s.notificationHook = h
}

// SetFederationHook attaches a FederationHook invoked on note creation.
func (s *CreateService) SetFederationHook(h FederationHook) {
	s.federationHook = h
}

// SetChannelHook attaches a ChannelHook invoked when a note is posted to a
// channel. nil 渡しは無効化と同義。
func (s *CreateService) SetChannelHook(h ChannelHook) {
	s.channelHook = h
}

// SetAntennaHook attaches an AntennaHook invoked after note creation so the
// antenna service can fan the note out into matching antenna timelines.
func (s *CreateService) SetAntennaHook(h AntennaHook) {
	s.antennaHook = h
}

// SetIndexHook attaches an IndexHook invoked after note creation so the
// search backend can index the new note.
func (s *CreateService) SetIndexHook(h IndexHook) {
	s.indexHook = h
}

// SetChartHook attaches a ChartHook invoked after note creation so the
// chart subsystem can record the event in the relevant time series.
func (s *CreateService) SetChartHook(h ChartHook) {
	s.chartHook = h
}

// SetWebhookHook attaches a WebhookHook invoked after note creation so that
// user webhooks subscribed to note / reply / renote / mention events can fire.
func (s *CreateService) SetWebhookHook(h WebhookHook) {
	s.webhookHook = h
}

// SetHashtagHook attaches a HashtagHook invoked after note creation so the
// hashtag subsystem can update per-tag mentionedUsersCount / userIds (#680)。
func (s *CreateService) SetHashtagHook(h HashtagHook) {
	s.hashtagHook = h
}

// SetMainStreamPublisher attaches a publisher used to emit `reply`, `renote`,
// and `mention` events to the main channel. Optional — nil disables emit.
func (s *CreateService) SetMainStreamPublisher(p MainStreamPublisher) {
	s.mainStreamPublisher = p
}

// Create creates a new note. It returns the persisted note (with the User
// relation preloaded when possible).
func (s *CreateService) Create(in CreateInput) (*model.Note, error) {
	if in.User == nil {
		return nil, errors.New("user is required")
	}

	// #2106 L33: upstream NoteCreateService は text を trim し、trim 後が空なら null に正規化
	// してから content-less 判定・保存を行う。先頭/末尾空白を upstream と同じく除去して
	// 保存値・AP 配送値を揃える。
	if in.Text != nil {
		trimmed := strings.TrimSpace(*in.Text)
		if trimmed == "" {
			in.Text = nil
		} else {
			in.Text = &trimmed
		}
	}

	// notes/createのバリデーション: text/fileIds/renoteId/poll のいずれかが必須。
	// upstream create.ts の JSON schema は renoteId/fileIds/mediaIds/poll が
	// すべて null のときだけ text を required にするので、アンケートだけの
	// 投稿 (text なし) も有効なコンテンツとして通す。
	if (in.Text == nil || *in.Text == "") && in.RenoteID == nil && len(in.FileIDs) == 0 && in.Poll == nil {
		return nil, ErrNoteContentRequired
	}

	visibility := in.Visibility
	if visibility == "" {
		visibility = model.NoteVisibilityPublic
	}
	// localOnly は renote / reply 対象が local-only のとき伝播させる (#1849 / #1855)。
	localOnly := in.LocalOnly

	// reply/renote 先を取得する。reply 先の channel を継承する re-scoping (#1859) と
	// channel-force / silencing が effective channel に依存するため、channel 判定より
	// 前に fetch する。fetch error は後段の validation block で報告するので、報告順
	// (reply → renote) は変わらない。ReplyID / RenoteID の fetch は独立なので並列に
	// 走らせる (#300 2-5)。
	var (
		replyTarget, renoteTarget *model.Note
		replyFetchErr             error
		renoteFetchErr            error
	)
	{
		var wg sync.WaitGroup
		if in.ReplyID != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				replyTarget, replyFetchErr = s.noteRepo.FindByIDWithUser(*in.ReplyID)
				if s.materializeIfMissing(*in.ReplyID, replyFetchErr) {
					replyTarget, replyFetchErr = s.noteRepo.FindByIDWithUser(*in.ReplyID)
				}
			}()
		}
		if in.RenoteID != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				renoteTarget, renoteFetchErr = s.noteRepo.FindByIDWithUser(*in.RenoteID)
				if s.materializeIfMissing(*in.RenoteID, renoteFetchErr) {
					renoteTarget, renoteFetchErr = s.noteRepo.FindByIDWithUser(*in.RenoteID)
				}
			}()
		}
		wg.Wait()
	}

	// effective channel を決める (#1859):
	//   - 空文字 channelId は不正入力なので非 channel に正規化する (part 2)。
	//   - reply がある場合は upstream NoteCreateService:444-458 ("reply は対象の
	//     スコープに合わせる") に倣い reply 先の channel を継承する (part 1)。これにより
	//     channel 外への reply は channel を外れ、channel note への reply は同 channel に
	//     入る。specifiedChannelID は user 指定 channel の存在検証専用に別途保持する。
	specifiedChannelID := normalizeChannelID(in.ChannelID)
	effectiveChannelID := specifiedChannelID
	if replyTarget != nil {
		effectiveChannelID = normalizeChannelID(replyTarget.ChannelID)
	}

	// channel note は upstream NoteCreateService:463-465 と同じく visibility=public /
	// localOnly=true を強制し、visibleUserIds を空にする (channel 機構が露出範囲を
	// 管理するため、#1855)。re-scope 後の effective channel で判定する。
	isChannelNote := effectiveChannelID != nil
	if isChannelNote {
		visibility = model.NoteVisibilityPublic
		localOnly = true
	}

	// canPublicNote=false の user が channel 外の public note を投げた場合、
	// upstream Misskey TS の NoteCreateService と同様に visibility を home
	// に降格する (silencing)。連合互換性のため reject ではなく降格扱い (=
	// 投稿は成功するが timeline 表出範囲だけ絞られる)。channel 内の note は
	// channel 機構自体が露出範囲を管理するので降格しない (#1024)。
	//
	// channel 判定は upstream の `data.channel == null` に対応する effectiveChannelID
	// (空文字正規化 + reply 継承済、#1859) を使う。channel note は上で public 強制
	// 済なのでこの gate は実質 channel 外の note にだけ効く。
	if visibility == model.NoteVisibilityPublic && effectiveChannelID == nil {
		if s.silencingProvider != nil && s.silencingProvider.IsSilenced(in.User.ID) {
			visibility = model.NoteVisibilityHome
		}
	}

	// meta を 1 度だけ fetch して sensitive / prohibited 両 check に reuse
	// (PR #1107 perf bundle)。旧版は両 helper が独立に Fetch を呼んでいて L1
	// cache cold な環境では note 作成あたり 2 round-trip 発生していた。Fetch
	// 失敗は両 helper とも fail-open (返り値 nil 扱いで素通し) なので、ここ
	// でも error は捨てて meta=nil で先に進む = 振る舞いは旧版と完全一致。
	var meta *model.Meta
	if s.metaRepo != nil {
		if m, err := s.metaRepo.Fetch(); err == nil {
			meta = m
		}
	}

	// meta.sensitiveWords にマッチする text / CW を持つ public note は home に
	// 降格する (upstream Misskey TS NoteCreateService.ts:467-471 と同一挙動 +
	// PR #1106 で CW/text 独立 check に強化済)。channel 内は降格対象外
	// (上の silencing と同じ理由)。投稿は成功するが public timeline /
	// federation broadcast から外れる effect で、admin が設定したセンシティブ
	// ワード設定が実機で効くようにする (drop-in regression fix)。upstream と
	// 同じく filter は `/regex/flags` または space 区切り AND match。
	if visibility == model.NoteVisibilityPublic && effectiveChannelID == nil {
		if matchesSensitiveWords(meta, in.Text, in.CW) {
			visibility = model.NoteVisibilityHome
		}
	}

	// プロhibited wordsチェック (meta.prohibitedWordsマッチ)。#2106 N12: poll choices も検査。
	var prohibitedPollChoices []string
	if in.Poll != nil {
		prohibitedPollChoices = in.Poll.Choices
	}
	if err := checkProhibitedWords(meta, in.Text, in.CW, prohibitedPollChoices); err != nil {
		return nil, err
	}

	// mention の解決はここで 1 回だけ行い、上限チェックと note.Mentions の
	// 両方で使い回す (host ごとの batch query を二重に投げないため)。
	var (
		mentionUserIDs   []string
		mentionRawCount  int
		mentionsResolved bool
	)
	if in.Text != nil && *in.Text != "" {
		mentions := ExtractMentionStructs(*in.Text)
		// 上限の判定には NoExtractMentions でも text 中の mention を数える
		// (AP 経路で tag 由来の mention に置き換える場合でも本文の量は効く)。
		mentionRawCount = len(mentions)
		if len(mentions) > 0 && !in.NoExtractMentions && s.userRepo != nil {
			mentionUserIDs = s.resolveMentionUserIDs(mentions)
			mentionsResolved = true
		}
	}

	// mentionLimitチェック (role policiesの制限)
	if err := s.checkMentionLimit(in, visibility, replyTarget, mentionUserIDs, mentionRawCount); err != nil {
		return nil, err
	}

	// pollのexpiresAtが既に過去なら弾く
	if in.Poll != nil && in.Poll.ExpiresAt != nil && in.Poll.ExpiresAt.Before(time.Now()) {
		return nil, ErrCannotCreateAlreadyExpiredPoll
	}

	// fileIdsのうち存在しないIDがあれば弾く
	if err := s.checkFileIDs(in.User.ID, in.FileIDs); err != nil {
		return nil, err
	}

	// user 指定 channel の存在チェック (空文字正規化済 specifiedChannelID)。reply
	// 継承で得た channel は元 note が属する実在 channel なので検証不要 (#1859)。
	// channelHook 未設定なら channel 機能無効として扱いエラーは返さない。
	// 注: reply 先 channel が削除済の極端ケースでは upstream (findOneBy→null で
	// channel=null) と異なり dangling な channelId を残す。mk-go に channel 削除
	// 経路が無く到達しない上、OnNotePosted も no-op で無害なため許容する。
	if specifiedChannelID != nil && s.channelHook != nil {
		if err := s.channelHook.EnsureChannelExists(*specifiedChannelID); err != nil {
			// **DB 障害を not-found に丸めない** (#2792)。丸めると障害が
			// NO_SUCH_CHANNEL (400) に化け、監視でも 5xx が立たない。
			if errors.Is(err, ErrChannelNotFound) {
				return nil, ErrChannelNotFound
			}
			return nil, fmt.Errorf("ensure channel exists: %w", err)
		}
	}

	// reply/renote 先の取得 (FindByIDWithUser) は visibility 決定のため上方へ移動済。
	// ここでは取得済みの replyTarget / renoteTarget を validation に使う。報告順は
	// reply → renote。
	if in.ReplyID != nil {
		if replyFetchErr != nil {
			// **DB 障害を not-found に丸めない** (#2799)。lookup が上の
			// goroutine に hoist されているので gate の射程外 — ここは
			// テストが唯一の回帰検知になる。
			if !repository.IsNotFound(replyFetchErr) {
				return nil, replyFetchErr
			}
			return nil, ErrReplyTargetNotFound
		}
		t := replyTarget
		// 可視性チェックを先に行う。FindByIDWithUserは無条件に行を返すため、
		// 不可視noteに対して別error (pure renote 等) を返すと「対象noteが
		// 何であるか」を攻撃者が推測できる情報漏洩になる。Devin review #270。
		if !CanSeeNote(in.User, t, s.followingRepo) {
			return nil, ErrCannotReplyToInvisibleNote
		}
		// pure renote (renoteIdあり、text/files/poll/cwなし) への返信は許可しない
		if IsPureRenote(t) {
			return nil, ErrCannotReplyToAPureRenote
		}
		// specified可視性noteへの返信時は visibility も specified でなければ拒否
		if t.Visibility == model.NoteVisibilitySpecified && visibility != model.NoteVisibilitySpecified {
			return nil, ErrCannotReplyToSpecifiedVisibility
		}
		// reply対象のユーザーに block されていたら拒否
		if t.UserID != in.User.ID {
			if err := s.checkBlocked(t.UserID, in.User.ID); err != nil {
				return nil, err
			}
		}
		// 返信対象の可視性に応じて段階クランプする (upstream 2026.7.0 #17747)。
		visibility = ClampVisibilityForReply(t.Visibility, visibility)
		// local-only な対象への reply は local-only にする (channel 外のみ、
		// upstream:541-543)。
		if t.LocalOnly && effectiveChannelID == nil {
			localOnly = true
		}
	}
	if in.RenoteID != nil {
		if renoteFetchErr != nil {
			// **DB 障害を not-found に丸めない** (#2799、reply 側と同じ)。
			if !repository.IsNotFound(renoteFetchErr) {
				return nil, renoteFetchErr
			}
			return nil, ErrRenoteTargetNotFound
		}
		t := renoteTarget
		// 可視性チェックを先に行う (Devin review #270 — 情報漏洩防止)。
		if !CanSeeNote(in.User, t, s.followingRepo) {
			return nil, ErrCannotRenoteInvisibleNote
		}
		// renote 対象の可視性 gate (upstream NoteCreateService、#1821)。CanSeeNote は
		// follower なら他人の followers note でも true を返すため、それだけだと
		// followers 限定 note を renote 経由で公開 TL / 連合へ漏洩させられる。
		// upstream は follow 関係を問わず「他人の followers note」と「(自分のもの
		// 含む) specified note」を無条件 reject する。
		if t.Visibility == model.NoteVisibilityFollowers && t.UserID != in.User.ID {
			return nil, ErrCannotRenoteInvisibleNote
		}
		if t.Visibility == model.NoteVisibilitySpecified {
			return nil, ErrCannotRenoteInvisibleNote
		}
		// 拒否を通過した renote 対象の可視性に応じて renote 自身の可視性を調整する
		// (upstream NoteCreateService の switch、#1849)。silencing/sensitive 降格の
		// 後に走らせるのは upstream と同順。
		switch t.Visibility {
		case model.NoteVisibilityHome:
			// home 対象を public で renote したら home に降格 (home 以下のみ可)。
			if visibility == model.NoteVisibilityPublic {
				visibility = model.NoteVisibilityHome
			}
		case model.NoteVisibilityFollowers:
			// ここに来るのは自分の followers note のみ (他人は上で reject)。upstream
			// #15961: public / home の引用のみ followers に降格し、ユーザーが選んだより
			// 狭い可視性 (specified / direct / followers) は維持する。
			if visibility == model.NoteVisibilityPublic || visibility == model.NoteVisibilityHome {
				visibility = model.NoteVisibilityFollowers
			}
		}
		// local-only な対象を renote したら local-only にする (channel 外のみ、
		// upstream `data.renote.localOnly && data.channel == null`)。
		if t.LocalOnly && effectiveChannelID == nil {
			localOnly = true
		}
		// pure renoteを更にrenoteするのは禁止 (TS: isRenote && !isQuote)
		if IsPureRenote(t) {
			return nil, ErrCannotRenoteToAPureRenote
		}
		// renote対象のユーザーに block されていたら拒否
		if t.UserID != in.User.ID {
			if err := s.checkBlocked(t.UserID, in.User.ID); err != nil {
				return nil, err
			}
		}
		// チャンネル外への renote 可否: 対象がチャンネル note で、ユーザー指定の
		// channel が対象と異なる (= チャンネル外への転送) 場合は channel の
		// allowRenoteToExternal を確認する。upstream は本 gate を createNote
		// wrapper で **re-scope 前の data.channelId** で評価するため、reply 継承後の
		// effectiveChannelID ではなく specifiedChannelID (= 正規化した user 指定) を
		// 使う (#1859 review)。
		if t.ChannelID != nil && !sameChannel(t.ChannelID, specifiedChannelID) {
			if err := s.checkRenoteOutsideOfChannel(*t.ChannelID); err != nil {
				return nil, err
			}
		}
	}

	now := time.Now()
	noteID := s.idGen.Generate(now)

	// hashtag 抽出: text/cw から #tag を拾い note.tags 列に格納する。
	// hashtags/trend の動的集計や hashtag 検索が機能するためには
	// note.tags が常に正しく埋められている必要がある (#655)。
	var tags []string
	if !in.NoExtractHashtags {
		var hashtagParts []string
		if in.Text != nil {
			hashtagParts = append(hashtagParts, *in.Text)
		}
		if in.CW != nil {
			hashtagParts = append(hashtagParts, *in.CW)
		}
		// upstream NoteCreateService は note.tags を normalizeForSearch (NFKC +
		// lowercase) で正規化し、>128 char を drop、32 件 cap してから格納する。
		// search-by-tag が同 normalize を query に適用するため一致させる (#1948-18)。
		tags = hashtag.ExtractNoteTags(hashtagParts...)
	}

	note := &model.Note{
		ID:                 noteID,
		UserID:             in.User.ID,
		Text:               in.Text,
		CW:                 in.CW,
		Visibility:         visibility,
		LocalOnly:          localOnly,
		ReactionAcceptance: in.ReactionAcceptance,
		ReplyID:            in.ReplyID,
		RenoteID:           in.RenoteID,
		ChannelID:          effectiveChannelID,
		FileIDs:            in.FileIDs,
		UserHost:           in.User.Host,
		Tags:               model.StringArray(tags),
	}

	// reply/renote先の非正規化フィールドを埋める
	if replyTarget != nil {
		note.ReplyUserID = &replyTarget.UserID
		note.ReplyUserHost = replyTarget.UserHost
		// thread-muting が深い thread でも効くよう threadId を thread root に揃える
		// (upstream NoteCreateService.ts:623-627: threadId = data.reply.threadId ??
		// data.reply.id)。reply の threadId が NULL だと thread-muting / isMutedThread /
		// timeline filter が reply を root と紐付けられず深さ2以上で破綻する (#1928)。
		threadID := replyTarget.ID
		if replyTarget.ThreadID != nil && *replyTarget.ThreadID != "" {
			threadID = *replyTarget.ThreadID
		}
		note.ThreadID = &threadID
	}
	if renoteTarget != nil {
		note.RenoteUserID = &renoteTarget.UserID
		note.RenoteUserHost = renoteTarget.UserHost
		note.RenoteChannelID = renoteTarget.ChannelID
	}

	// visibleUserIds は visibility=specified のときだけ保存する
	// (upstream insertNote:667 `visibility === 'specified' ? ... : []`)。
	// channel note は public 強制済なので同様に空 (upstream:464、#1855)。
	// 他 visibility に stray な指定が残ると、可視判定や list timeline の
	// push-down が「宛先」として拾ってしまう。
	if in.VisibleUserIDs != nil && !isChannelNote && note.Visibility == model.NoteVisibilitySpecified {
		note.VisibleUserIDs = in.VisibleUserIDs
	}

	// #2106 N13: specified(direct) note では reply target を必ず visibleUserIds に含める
	// (upstream NoteCreateService.ts:603-605)。これを欠くと reply 相手が note を閲覧できず
	// (CanSeeNote)・reply 通知も飛ばず (notifyVisibleToTarget)・AP の to にも入らないため
	// 連合先 reply 相手にも配送されない。client が visibleUserIds に reply 相手を含めずに
	// specified reply を送るケース (upstream contract は backend 補完を前提) を救う。
	// なお mention 対象は upstream も visibleUsers には追加しない (597-601 は visibleUsers
	// → mentionedUsers の向きで逆ではない) ため、ここでも追加しない。
	if note.Visibility == model.NoteVisibilitySpecified && replyTarget != nil && replyTarget.UserID != note.UserID {
		alreadyVisible := false
		for _, id := range note.VisibleUserIDs {
			if id == replyTarget.UserID {
				alreadyVisible = true
				break
			}
		}
		if !alreadyVisible {
			note.VisibleUserIDs = append(note.VisibleUserIDs, replyTarget.UserID)
		}
	}

	// メンションの抽出。ユーザー名+ホストをユーザーIDに解決する。
	// ローカル (host="") は host IS NULL で、リモート (host!="") は
	// host 列の等価比較で lookup する。未知のユーザーは単にスキップする
	// (remote webfinger 解決は pre-lookup されている前提)。
	// userRepo が未設定のときは後方互換のため username 文字列をそのまま格納する。
	if in.Text != nil && !in.NoExtractMentions {
		if s.userRepo != nil {
			if mentionsResolved && len(mentionUserIDs) > 0 {
				note.Mentions = mentionUserIDs
			}
		} else {
			note.Mentions = ExtractMentions(*in.Text)
		}
	}

	// upstream NoteCreateService.ts:610-621 と同じく、reply 先の投稿者と
	// (specified のとき) visibleUserIds を mentions に含める。notes/mentions や
	// followers note の可視判定 (shouldHideNote の mentions 分岐) がこの列を
	// 見ているため、欠けると「返信された相手」が自分宛ての返信を辿れない。
	// userRepo 未設定時は note.Mentions が username 文字列なので触らない。
	if s.userRepo != nil {
		if replyTarget != nil && replyTarget.UserID != note.UserID {
			note.Mentions = appendUniqueID(note.Mentions, replyTarget.UserID)
		}
		if note.Visibility == model.NoteVisibilitySpecified {
			for _, id := range note.VisibleUserIDs {
				note.Mentions = appendUniqueID(note.Mentions, id)
			}
		}
	}

	// Custom emoji 名抽出: text + cw を MFM parse して :code: トークンを集める
	// (#629)。連合配信時に renderer.addEmojiTags が note.Emojis を walk して
	// AP Note.tag に Emoji エントリを足すので、ここで埋めないと連合先で
	// custom emoji が画像化されず文字列のまま表示される。
	if !in.NoExtractEmojis {
		var text, cw string
		if in.Text != nil {
			text = *in.Text
		}
		if in.CW != nil {
			cw = *in.CW
		}
		if names := mfm.CollectEmojiCodes(text, cw); len(names) > 0 {
			note.Emojis = names
		}
	}

	if err := s.noteRepo.Create(note); err != nil {
		return nil, err
	}

	// reply/renote先のカウンタを更新する。
	if replyTarget != nil {
		_ = s.noteRepo.IncrementCount(replyTarget.ID, "repliesCount", 1)
	}
	// upstream NoteCreateService の incRenoteCount 呼び出し条件と同一
	// (`data.renote && data.renote.userId !== user.id && !user.isBot`)。
	// quote renote も加算対象に含まれる — upstream に pure renote 限定の
	// 条件は無い (#2283)。自己 renote と bot の renote は加算しない。
	//
	// featured ランキング更新も upstream では incRenoteCount の中にあるので
	// 同じ条件で発火させる (#1687)。
	if renoteTarget != nil && renoteTarget.UserID != in.User.ID && !in.User.IsBot {
		_ = s.noteRepo.IncrementCount(renoteTarget.ID, "renoteCount", 1)
		s.updateFeaturedOnRenote(renoteTarget)
	}

	// 投票が指定されていればPollレコードを作成しnote.hasPollを更新
	if in.Poll != nil && len(in.Poll.Choices) > 0 {
		votes := make([]int64, len(in.Poll.Choices))
		poll := &model.Poll{
			NoteID:         noteID,
			Multiple:       in.Poll.Multiple,
			Choices:        in.Poll.Choices,
			Votes:          votes,
			NoteVisibility: visibility,
			UserID:         in.User.ID,
			UserHost:       in.User.Host,
			ChannelID:      effectiveChannelID,
			ExpiresAt:      in.Poll.ExpiresAt,
		}
		if err := s.pollRepo.Create(poll); err != nil {
			return nil, err
		}
		if err := s.noteRepo.Update(note, "hasPoll", true); err != nil {
			return nil, err
		}
		note.HasPoll = true
	}

	// User / Renote / Reply リレーションをpreloadして返す。失敗時は引数の
	// Userをそのまま埋めて返す。fanout / hook 先で renote/reply embed を
	// 触るため full version を使う (#425)。
	finalNote := note
	if loaded, err := s.noteRepo.FindByIDWithRelations(noteID); err == nil && loaded != nil {
		finalNote = loaded
	} else {
		finalNote.User = in.User
	}

	// ベストエフォートフックを非同期で並列実行する。各フックは独立しており、
	// 失敗してもノート作成自体は成功扱い。TS版と同様にfire-and-forgetで配信する。
	if s.fanoutHook != nil {
		safeGoKind(hookFanout, func() { s.fanoutHook.OnNoteCreated(finalNote, in.User) })
	}
	if s.notificationHook != nil {
		safeGo(func() { s.notificationHook.OnNoteCreated(finalNote, in.User, replyTarget, renoteTarget) })
	}
	if s.federationHook != nil {
		safeGo(func() { s.federationHook.OnNoteCreated(finalNote, in.User) })
	}
	// OnNotePosted は note が実際に属する effective channel (re-scope 後、#1859) で
	// 発火する。reply で別 channel に入った/外れた場合も正しい channel に通知する。
	if s.channelHook != nil && effectiveChannelID != nil {
		chID := *effectiveChannelID
		noteID := finalNote.ID
		authorID := in.User.ID
		safeGo(func() { s.channelHook.OnNotePosted(chID, noteID, authorID) })
	}
	if s.antennaHook != nil {
		// アンテナへの fan-out は同期で行う。upstream は
		// `antennaService.addNoteToAntennas(...)` を await しないが、Node の
		// シングルスレッドでは同一ティック内で Redis pipeline の発行まで
		// 進むため、投稿直後に antennas/notes を読んでも取りこぼさない。
		// goroutine に逃がすと Go ではその保証が無く、アンテナ数が多いほど
		// 遅れて実際に取りこぼす。判定は 1 note につき follow / list を
		// 1 回だけ引くようメモ化済み。
		s.antennaHook.OnNoteCreated(finalNote, in.User)
	}
	if s.indexHook != nil {
		safeGo(func() { s.indexHook.OnNoteCreated(finalNote) })
	}
	if s.chartHook != nil {
		safeGo(func() { s.chartHook.OnNoteCreated(finalNote) })
	}
	if s.hashtagHook != nil {
		// HashtagHook は実装側 (core/hashtag.Service) が内部で goroutine を
		// 起こす fire-and-forget 設計 (#719)。caller での safeGo wrap は不要。
		s.hashtagHook.OnNoteCreated(finalNote, in.User)
	}
	if s.webhookHook != nil {
		safeGo(func() { s.webhookHook.OnNoteCreated(finalNote, in.User, replyTarget, renoteTarget) })
	}
	// reply / renote / mention 各イベントを対象local userのmainにemit。
	// Misskey本家NoteCreateServiceと同じfan-out方針 (TS lines 792 / 809 / 935)。
	s.publishNoteMainEvents(finalNote, in.User, replyTarget, renoteTarget)

	// notesCount の増加は user 行への直接 UPDATE。userRepo 経由で叩くこと
	// で CachedUserRepository wrapper の invalidate が走り、stale notesCount
	// が profile API に出続ける問題を防ぐ (Devin review #552 BUG-2)。
	// userRepo 未配線の旧経路は noteRepo の同等メソッドにフォールバック。
	safeGo(func() {
		if s.userRepo != nil {
			_ = s.userRepo.IncrementNotesCount(in.User.ID, 1)
			return
		}
		_ = s.noteRepo.IncrementUserNotesCount(in.User.ID, 1)
	})

	return finalNote, nil
}

// updateFeaturedOnRenote boosts the renoted target note's featured engagement
// ranking (#1687, upstream NoteCreateService.incRenoteCount 内、score=5)。30%
// sampling、target が 3日以内、の gate を通った場合のみ。channel note は
// in-channel ranking、それ以外は public かつ local かつ非 reply のときに
// global + per-user ranking を更新する。caller 側で self-renote / bot を除外済。
func (s *CreateService) updateFeaturedOnRenote(target *model.Note) {
	if s.featuredRanking == nil || s.randFn == nil {
		return
	}
	if s.randFn() >= featuredSampleRate {
		return
	}
	created, err := s.idGen.ParseTime(target.ID)
	if err != nil || time.Since(created) >= featuredMaxNoteAge {
		return
	}
	ctx := context.Background()
	if target.ChannelID != nil && *target.ChannelID != "" {
		if target.ReplyID == nil {
			_ = s.featuredRanking.UpdateInChannelNotesRanking(ctx, *target.ChannelID, target.ID, 5)
		}
		return
	}
	if target.Visibility == model.NoteVisibilityPublic && target.UserHost == nil && target.ReplyID == nil {
		_ = s.featuredRanking.UpdateGlobalNotesRanking(ctx, target.ID, 5)
		_ = s.featuredRanking.UpdatePerUserNotesRanking(ctx, target.UserID, target.ID, 5)
	}
}

// publishNoteMainEvents fans out `reply`, `renote`, and `mention` events to
// the relevant local users' main channels. TS本家と同じくdedupはせず、
// 同一userに対して複数イベント(例: 相手へのリプライかつメンション)が
// 並列で届きうる(frontendはイベント種別ごとに意味付けるため)。
//
// 旧実装は target が note を見られるか検証せずに `entity.PackNote` の full
// payload (Text / CW / Files / Renote / Reply embed) を main stream に流して
// いた (#1472)。followers visibility note を非フォロワー reply/renote/mention
// target に届けたり、specified visibility note を visibleUserIDs 外の
// mentioned user に届けて本文が漏れる IDOR が成立していた。各 publish の前に
// CanSeeNote 同等の判定で gate する: REST `i/notifications` (#1444) / stream
// `notifications:` (#1471) と doctrine を揃える。
func (s *CreateService) publishNoteMainEvents(note *model.Note, author *model.User, replyTarget, renoteTarget *model.Note) {
	if s.mainStreamPublisher == nil {
		return
	}
	packed := entity.PackNote(note, s.idGen)
	// reply: reply先がlocal (UserHost == nil) かつ自分自身へのリプライで
	// ない場合にemit。visibility gate (#1472): reply target が note を見ら
	// れないとき (= 非 follower や visibleUserIds 外) は本文 leak になるので
	// publish skip。CanSeeNote は viewer.ID == note.UserID で短絡するため
	// 著者自身への self-reply は元から exclude されている `UserID != author.ID`
	// と重ねて防御。
	if replyTarget != nil && replyTarget.UserHost == nil && replyTarget.UserID != author.ID {
		viewer := &model.User{ID: replyTarget.UserID}
		// upstream はスレッドミュート中の reply 先には reply event を出さない
		// (#1954)。thread は reply 先ノートの threadId (無ければ自身の id)。
		if canSeeNoteForStream(viewer, note, s.followingRepo) && !s.isThreadMuted(replyTarget.UserID, replyTarget) {
			s.mainStreamPublisher.PublishMainEvent(replyTarget.UserID, "reply", packed)
		}
	}
	// renote: 同上 (TS: caller ≠ target author条件あり)。visibility gate も
	// reply と同じ理由で必要。renote target が non-follower の場合、quote
	// renote (= Text/CW 持ち) は本文ごと leak する。
	if renoteTarget != nil && renoteTarget.UserHost == nil && renoteTarget.UserID != author.ID {
		viewer := &model.User{ID: renoteTarget.UserID}
		if canSeeNoteForStream(viewer, note, s.followingRepo) {
			s.mainStreamPublisher.PublishMainEvent(renoteTarget.UserID, "renote", packed)
		}
	}
	// mention: note.Mentionsは(userRepo設定時)user ID配列なので、
	// 各userをfetchしてlocalかつauthor自身でなければemitする。
	// userRepo未設定時はMentionsがusername文字列のため解決不能でスキップ。
	if s.userRepo == nil {
		return
	}
	for _, uid := range note.Mentions {
		if uid == author.ID {
			continue
		}
		u, err := s.userRepo.FindByID(uid)
		if err != nil || u == nil {
			continue
		}
		if u.Host != nil {
			continue // remote user — AP配送(別レイヤ)が担当
		}
		// visibility gate (#1472): specified visibility 経路では note.Mentions
		// と note.VisibleUserIDs が乖離しうる (本文に @x が含まれるが visible
		// 指定は別) ため、CanSeeNote の slices.Contains で visibleUserIDs 外の
		// mention target を弾く。followers visibility 経路でも non-follower
		// mention をここで止める。
		if !canSeeNoteForStream(u, note, s.followingRepo) {
			continue
		}
		// mention 先がこのスレッドをミュートしていれば mention event を出さない
		// (#1954)。thread は新規ノート自身の threadId (無ければ自身の id)。
		if s.isThreadMuted(uid, note) {
			continue
		}
		s.mainStreamPublisher.PublishMainEvent(uid, "mention", packed)
	}
}

// isThreadMuted reports whether userID has muted the thread that threadNote
// belongs to. The thread root is threadNote.ThreadID (falling back to its own
// id), mirroring upstream `note.threadId ?? note.id`. Returns false when the
// repository is not wired (gate disabled) or on lookup error (fail-open: emit).
func (s *CreateService) isThreadMuted(userID string, threadNote *model.Note) bool {
	if s.threadMuteRepo == nil || threadNote == nil {
		return false
	}
	threadID := threadNote.ID
	if threadNote.ThreadID != nil && *threadNote.ThreadID != "" {
		threadID = *threadNote.ThreadID
	}
	muted, err := s.threadMuteRepo.Exists(userID, threadID)
	return err == nil && muted
}

// mentionRegex matches @username and @username@host occurrences anywhere in
// text. Misskey本家のクライアント実装(misskey-js)と同じく、ユーザー名は
// 英数とアンダースコアおよびハイフンからなり、長さは制限しない。
// ホスト部はドメイン形式 (英数・ハイフン・ドット) を許容する。
var mentionRegex = regexp.MustCompile(`@([A-Za-z0-9_-]+)(?:@([A-Za-z0-9.\-]+))?`)

// Mention represents a single user mention extracted from note text.
type Mention struct {
	Username string
	Host     string // ホスト指定がない場合は空文字
}

// appendUniqueID appends id to ids unless it is already present.
func appendUniqueID(ids []string, id string) []string {
	if id == "" || slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// ExtractMentions extracts mention usernames from a note text.
// 戻り値はusername (ホストなしの場合) または "username@host" (リモートユーザー指定の場合)。
// Misskeyのnote.mentions列はユーザーIDの配列だが、本サービスではユーザー解決を
// 別レイヤで行う前提で、ここではユーザー名形式のままで返す。重複は除去する。
func ExtractMentions(text string) []string {
	matches := mentionRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	var out []string
	for _, m := range matches {
		username := m[1]
		host := ""
		if len(m) >= 3 {
			host = m[2]
		}
		key := username
		if host != "" {
			key = username + "@" + host
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// ExtractMentionStructs returns the mentions as structured Mention values.
// 主にNotificationServiceなどリモート/ローカル区別を要する呼び出し向け。
func ExtractMentionStructs(text string) []Mention {
	matches := mentionRegex.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	var out []Mention
	for _, m := range matches {
		username := m[1]
		host := ""
		if len(m) >= 3 {
			host = m[2]
		}
		key := username + "@" + host
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, Mention{Username: username, Host: host})
	}
	return out
}

// IsPureRenote reports whether the persisted note is a pure renote (no text,
// no cw, no poll, no files). 連合先で Create と Announce を切り替える際に使う。
func IsPureRenote(n *model.Note) bool {
	if n == nil || n.RenoteID == nil {
		return false
	}
	if n.Text != nil && *n.Text != "" {
		return false
	}
	if n.CW != nil && *n.CW != "" {
		return false
	}
	if len(n.FileIDs) > 0 {
		return false
	}
	if n.HasPoll {
		return false
	}
	// renote + reply は quote 扱い (upstream isQuote は replyId != null も quote)
	// なので pure renote ではない。Announce ではなく Create で配信される (#1882)。
	if n.ReplyID != nil && *n.ReplyID != "" {
		return false
	}
	return true
}

// sameChannel reports whether two optional channel IDs refer to the same channel.
// Both nil or same pointer or same string → true; differing strings → false.
func sameChannel(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// normalizeChannelID は channelId pointer を正規化する: nil / 空文字 (不正入力) は
// 「非 channel」を表す nil に畳む (#1859)。これで silencing / force / 構築など全 gate
// で空文字 channelId を一貫して非 channel 扱いできる (upstream は misskey:id schema で
// 空文字を 400 reject する)。
func normalizeChannelID(id *string) *string {
	if id == nil || *id == "" {
		return nil
	}
	return id
}

// matchesSensitiveWords reports whether the note's CW or text contains any
// word listed in meta.sensitiveWords. Used by Create to demote public note
// visibility to home.
//
// **mk-go independent hardening (Ed25519 / TOTP replay / bcrypt 72-byte と
// 同系列の judgment call、upstream には存在しない strict 化)**:
//
// upstream Misskey TS NoteCreateService.ts:469 は `cw ?? text ?? ”` で 1
// field だけを check する JS nullish coalescing 仕様で、admin が CW を
// 設定するだけで text 側の sensitive 検出を bypass できる known な穴が
// ある。mk-go では PR #1105 (drop-in fix) で upstream parity を維持し、
// PR #1106 で **CW と text を独立に check** して bypass を塞いだ。
//
// 各 field を独立に IsKeyWordIncluded に渡すことで、regex フィルタの
// `^/$` boundary も field 単位で正しく解釈される (concat 案だと改行
// 越境 match が発生して挙動が予測しにくくなる)。
//
// meta は呼び元の Create が 1 度だけ fetch して渡す (perf: 連続 Fetch を
// 統合)。meta が nil または sensitiveWords が空 → false (no match)。
// caller が fetch error で nil を渡すケースも fail-open で false: 反映漏れ
// の方が「投稿が思わぬ form で blocked される」より影響軽微で、upstream
// も同タイミングで throw しないので挙動も揃う。
func matchesSensitiveWords(meta *model.Meta, text, cw *string) bool {
	if meta == nil || len(meta.SensitiveWords) == 0 {
		return false
	}
	words := []string(meta.SensitiveWords)
	if cw != nil && *cw != "" && keyword.IsKeyWordIncluded(*cw, words) {
		return true
	}
	if text != nil && *text != "" && keyword.IsKeyWordIncluded(*text, words) {
		return true
	}
	return false
}

// checkProhibitedWords scans cw + text + poll choices for any word listed in
// meta.prohibitedWords. Returns ErrContainsProhibitedWords on match.
// meta が nil または prohibitedWords が空なら skip する (= caller の
// fetch error も nil 渡しで fail-open される)。meta は呼び元の Create が
// 1 度だけ fetch して渡す (perf: matchesSensitiveWords と統合)。
//
// #2106 N12: 旧実装は text+cw のみを strings.ToLower→Contains で素朴に検査し、
// poll choices を見ず、`/regex/flags` / スペース区切り AND も解釈せず、強制 lowercase で
// case-sensitive な upstream とも乖離していた。matchesSensitiveWords と同じく
// keyword.IsKeyWordIncluded (regex / space-AND / case-sensitive) に揃え、upstream
// checkProhibitedWordsContain と同じく poll choices も検査対象に含める。各 field を
// 独立に渡すのは sensitiveWords (#1106) と同方針 (concat 案は改行越境 match で挙動が読みにくい)。
func checkProhibitedWords(meta *model.Meta, text, cw *string, pollChoices []string) error {
	if meta == nil || len(meta.ProhibitedWords) == 0 {
		return nil
	}
	words := []string(meta.ProhibitedWords)
	if cw != nil && *cw != "" && keyword.IsKeyWordIncluded(*cw, words) {
		return ErrContainsProhibitedWords
	}
	if text != nil && *text != "" && keyword.IsKeyWordIncluded(*text, words) {
		return ErrContainsProhibitedWords
	}
	for _, choice := range pollChoices {
		if choice != "" && keyword.IsKeyWordIncluded(choice, words) {
			return ErrContainsProhibitedWords
		}
	}
	return nil
}

// resolveMentionUserIDs maps `@username[@host]` mentions to local user IDs,
// dropping the ones that do not resolve to a known user.
//
// host ごとに 1 query にまとめる (#300 1-5)。元実装は mention 数 N に対して
// N 回 FindByUsernameLower を直列に叩いていたが、host 単位で IN(?) にして
// ラウンドトリップを host 種類数まで削減する。
func (s *CreateService) resolveMentionUserIDs(mentions []Mention) []string {
	if s.userRepo == nil || len(mentions) == 0 {
		return nil
	}
	byHost := make(map[string][]string)
	for _, m := range mentions {
		byHost[m.Host] = append(byHost[m.Host], m.Username)
	}
	// resolved[host][usernameLower] = userID
	resolved := make(map[string]map[string]string, len(byHost))
	for host, names := range byHost {
		var hostPtr *string
		if host != "" {
			h := host
			hostPtr = &h
		}
		users, err := s.userRepo.FindManyByUsernamesAndHost(names, hostPtr)
		if err != nil {
			// 1 host の lookup 失敗は当該 host の mention を諦めて後続を
			// 続行する (元実装も err 時 skip だった)。
			continue
		}
		m := make(map[string]string, len(users))
		for _, u := range users {
			m[u.UsernameLower] = u.ID
		}
		resolved[host] = m
	}
	ids := make([]string, 0, len(mentions))
	for _, mn := range mentions {
		if hostMap, ok := resolved[mn.Host]; ok {
			if id, ok := hostMap[strings.ToLower(mn.Username)]; ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// checkMentionLimit counts the note's mention targets and compares them against
// the author's `mentionLimit` role policy (#2321).
//
// upstream NoteCreateService は text 中の mention に加えて、返信先の投稿者と
// ダイレクト投稿の宛先 (visibleUsers) も同じ `mentionedUsers` 集合に入れて
// 数える。text に mention が 1 つも無いダイレクト投稿でも上限に当たるのは
// このため。宛先と text mention が同じ相手なら 1 件として数える。
//
// 解決できなかった mention は upstream では数に入らないが、mk-go は存在しない
// ユーザーへの mention を大量に含む note を弾くため文字列のまま数える (意図的
// な差分。upstream 自身も AP 経路では raw 数との max を取る方針 #17576)。
func (s *CreateService) checkMentionLimit(in CreateInput, visibility model.NoteVisibility, replyTarget *model.Note, mentionUserIDs []string, mentionRawCount int) error {
	targets := make(map[string]struct{})
	for _, id := range mentionUserIDs {
		targets[id] = struct{}{}
	}
	// 未解決の mention はどれが該当するか特定できないので、件数分だけ
	// 一意なキーを足して数に入れる。
	for i := len(mentionUserIDs); i < mentionRawCount; i++ {
		targets["\x00unresolved:"+strconv.Itoa(i)] = struct{}{}
	}
	if replyTarget != nil {
		targets[replyTarget.UserID] = struct{}{}
	}
	if visibility == model.NoteVisibilitySpecified {
		for _, id := range in.VisibleUserIDs {
			targets[id] = struct{}{}
		}
	}
	// upstream の条件は `mentionCount > 0 && mentionCount > mentionLimit` で、
	// limit 側にガードは無い。mk-go は `limit > 0` を条件にしていたため、
	// ロールで mentionLimit=0 (メンション全面禁止) を設定しても判定ごと
	// スキップされ、いくらでもメンションできてしまっていた。
	if len(targets) > 0 && len(targets) > s.mentionLimitFor(in.User.ID) {
		return ErrContainsTooManyMentions
	}
	return nil
}

// mentionLimitFor resolves the effective mentionLimit for userID, falling back
// to DefaultMentionLimit when the provider is unwired or the policy is absent /
// of an unexpected type. fail-soft にするのは、policy が引けないことを理由に
// 投稿そのものを止めるのは過剰なため (上限は既定値で効き続ける)。
func (s *CreateService) mentionLimitFor(userID string) int {
	if s.rolePolicyProvider == nil || userID == "" {
		return DefaultMentionLimit
	}
	policies := s.rolePolicyProvider.GetUserPolicies(userID)
	if policies == nil {
		return DefaultMentionLimit
	}
	// 戻り値が int なので小数は切り捨てる。**厳しい側に倒れる**ので
	// ゲートが消える方向ではない (0.5 → 0 = メンション不可、#2613)。
	if v, ok := role.PolicyNumber(policies["mentionLimit"]); ok {
		return int(math.Floor(v))
	}
	return DefaultMentionLimit
}

// DefaultMentionLimit mirrors role.DefaultPolicies().mentionLimit. corenote から
// 直接 role.DefaultPolicies() を import すると循環依存になるため、同期を保つ
// 意図で本家TSの現行既定値 (20) を const として直書きしている。
//
// local の note 作成では role policy の値が優先され、本定数は provider 未配線 /
// policy 不在時のフォールバックとして使う (#2321)。連合の inbound 経路
// (Resolver.IngestNote) はリモートユーザーがローカルの role を持たないため、
// 引き続き本定数のみで判定する。
const DefaultMentionLimit = 20

// checkFileIDs verifies all provided fileIds exist and belong to the user.
// DriveFileRepo未設定ならskipする。
// TS本家は user.id 所有の files のみ拾うが、Go の DriveFileRepo は ID ベース
// の bulk 取得しか持たないため、取得後に userId 一致をチェックする。
// 本家TSは Promise.all で個別解決するため fileIds に重複があっても通る。
// Go 側も `WHERE id IN ?` の SQL 重複排除による false positive を避けるため、
// 返却 file を「所有者一致の ID 集合」に畳んだ上で、入力 ID が全て集合に
// 含まれることを確認する (Devin review #270)。
func (s *CreateService) checkFileIDs(userID string, fileIDs []string) error {
	if len(fileIDs) == 0 || s.driveFileRepo == nil {
		return nil
	}
	files, err := s.driveFileRepo.ListByFileIDs(fileIDs)
	if err != nil {
		return err
	}
	owned := make(map[string]struct{}, len(files))
	for _, f := range files {
		if f.UserID != nil && *f.UserID == userID {
			owned[f.ID] = struct{}{}
		}
	}
	for _, id := range fileIDs {
		if _, ok := owned[id]; !ok {
			return ErrNoSuchFile
		}
	}
	return nil
}

// checkBlocked returns ErrYouHaveBeenBlocked when blockerID has blocked
// blockeeID. BlockingRepo未設定ならskip。
func (s *CreateService) checkBlocked(blockerID, blockeeID string) error {
	if s.blockingRepo == nil {
		return nil
	}
	blocked, err := s.blockingRepo.Exists(blockerID, blockeeID)
	if err != nil {
		return err
	}
	if blocked {
		return ErrYouHaveBeenBlocked
	}
	return nil
}

// checkRenoteOutsideOfChannel resolves the channel referenced by channelID
// and rejects renoting outside of it when the channel forbids it.
// ChannelRepo未設定ならskip (strictチェック不能として許可側に倒す)。
func (s *CreateService) checkRenoteOutsideOfChannel(channelID string) error {
	if s.channelRepo == nil {
		return nil
	}
	ch, err := s.channelRepo.FindByID(channelID)
	if err != nil {
		// チャンネル不明時は TS 側は NO_SUCH_CHANNEL を投げるが、本経路は
		// 既に renote ターゲットが作られている (= channel は存在した時点が
		// ある) 前提なので、取得失敗は allow 側に倒す。
		return nil
	}
	if !ch.AllowRenoteToExternal {
		return ErrCannotRenoteOutsideOfChannel
	}
	return nil
}

// NoteMaterializer promotes a relay-delivered note out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
type NoteMaterializer interface {
	EnsureNote(ctx context.Context, noteID string) (*model.Note, error)
}

// SetNoteMaterializer attaches the ephemeral-note materializer. Optional.
func (s *CreateService) SetNoteMaterializer(m NoteMaterializer) {
	s.materializer = m
}

// materializeIfMissing promotes an ephemeral note only when the database
// lookup already failed.
//
// 通常のノートでは Redis を一切引かないので、ホットパスに追加コストが無い。
func (s *CreateService) materializeIfMissing(noteID string, lookupErr error) bool {
	if lookupErr == nil || s.materializer == nil {
		return false
	}
	// **DB 障害では materialize しない** (#2799)。not-found でない error は
	// materialize しても直らないのに、ephemeral store の引きが 1 回余分に走る。
	if !repository.IsNotFound(lookupErr) {
		return false
	}
	_, err := s.materializer.EnsureNote(context.Background(), noteID)
	return err == nil
}

// HasBlockingRepo / HasMetaRepo / HasSilencingProvider report whether the
// note-creation gates were wired.
//
// 未配線時にそれぞれ: **自分をブロックしている**相手への返信・引用が通る
// (gate は `checkBlocked(t.UserID, in.User.ID)` = 返信先の著者が自分を
// ブロックしているか)、meta のセンシティブワードと禁止ワードの両方が効かない、
// silenced な利用者の public 投稿が home へ降格されない。起動時検査に使う
// (#2683)。
func (s *CreateService) HasBlockingRepo() bool { return s.blockingRepo != nil }

// HasMetaRepo reports whether the meta repository was wired.
func (s *CreateService) HasMetaRepo() bool { return s.metaRepo != nil }

// HasSilencingProvider reports whether the silencing provider was wired.
func (s *CreateService) HasSilencingProvider() bool { return s.silencingProvider != nil }

// HasDriveFileRepo reports whether the drive file repository was wired.
//
// 未配線だと `checkFileIDs` が素通しになり、**他人の drive file ID を
// `notes/create` の `fileIds` に指定して自分の note に貼れる** (IDOR)。
// page の eyecatch (#2683 の `pages.driveFileRepo`) より経路が広い。
func (s *CreateService) HasDriveFileRepo() bool { return s.driveFileRepo != nil }
