// Package reaction provides ReactionService for managing note reactions.
package reaction

import (
	"context"
	"errors"
	"math/rand"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/reactionlegacy"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/forPelevin/gomoji"
)

// featured ランキング更新の sampling rate / note 年齢上限 (upstream
// ReactionService / NoteCreateService と同値)。reaction は score=1。
const (
	featuredSampleRate = 0.3
	featuredMaxNoteAge = 3 * 24 * time.Hour
)

// FallbackReaction is the default reaction used when no reaction is provided.
// 本家Misskeyは Heart "❤" を fallback にしている。
const FallbackReaction = "\u2764"

// reactionMaxRunes は `note_reaction.reaction` の varchar(260)
// (migration/000001_initial.up.sql)。
const reactionMaxRunes = 260

// reactionAcceptance values a note may carry (upstream Misskey).
const (
	reactionAcceptanceLikeOnly                          = "likeOnly"
	reactionAcceptanceLikeOnlyForRemote                 = "likeOnlyForRemote"
	reactionAcceptanceNonSensitiveOnly                  = "nonSensitiveOnly"
	reactionAcceptanceNonSensitiveForLocalLikeForRemote = "nonSensitiveOnlyForLocalLikeOnlyForRemote"
)

// Errors returned by Service.
var (
	// ErrNoteNotFound is returned when the target note does not exist.
	ErrNoteNotFound = errors.New("note not found")
	// ErrNoteNotVisible is returned when the user cannot see the target note.
	ErrNoteNotVisible = errors.New("note not visible to user")
	// ErrAlreadyReacted is returned when the user has already reacted with the same reaction.
	ErrAlreadyReacted = errors.New("user has already reacted with this reaction")
	// ErrReactionNotFound is returned when there is no reaction to delete.
	ErrReactionNotFound = errors.New("reaction not found")
	// ErrCannotReactToPureRenote is returned when the user attempts to react to a pure renote.
	ErrCannotReactToPureRenote = errors.New("cannot react to a pure renote")
	// ErrBlocked is returned when the note author has blocked the reactor.
	ErrBlocked = errors.New("blocked by note author")
)

// BlockingChecker reports whether one user has blocked another. パッケージ間の
// 循環依存を避けるためinterfaceで受け取る (実装は core/blocking)。
type BlockingChecker interface {
	IsBlocked(blockerID, blockeeID string) (bool, error)
}

// customEmojiPattern matches a custom emoji shortcode like ":smile:" or
// ":smile@example.com:".
var customEmojiPattern = regexp.MustCompile(`^:([\w+\-]+)(?:@([\w.\-]+))?:$`)

// ConvertLegacy maps a stored reaction string to the read-time representation
// upstream's ReactionService.convertLegacyReaction returns: legacy text aliases
// ("like" → 👍) are mapped, and a local custom emoji ":name@.:" is decoded back
// to ":name:" (the "@." local-host suffix is stripped). Remote custom emoji and
// plain unicode pass through unchanged. Pure (no DB); used by read paths such as
// users/reactions where the raw DB string must not leak (#1547)。
func ConvertLegacy(raw string) string {
	if v, ok := reactionlegacy.Convert(raw); ok {
		return v
	}
	if m := customEmojiPattern.FindStringSubmatch(raw); m != nil {
		host := ""
		if len(m) >= 3 {
			host = m[2]
		}
		if host == "" || host == "." {
			return ":" + m[1] + ":"
		}
	}
	return raw
}

// NotificationHook is invoked after a reaction is created or removed.
type NotificationHook interface {
	OnReactionCreated(notifieeID, notifierID, noteID, reaction string)
	// OnReactionRemoved removes the notification the reaction left (#3201).
	// 取り消しと、別のリアクションへの付け替えの両方で呼ぶ。
	OnReactionRemoved(notifieeID, notifierID, noteID string)
}

// FederationHook is invoked after a reaction is created or removed so that
// the ActivityPub layer can deliver Like / Undo Like activities. パッケージ間
// の循環依存を避けるためinterfaceで受け取る (実装は core/federation)。
type FederationHook interface {
	OnReactionAdded(reactor *model.User, target *model.Note, reaction string)
	OnReactionRemoved(reactor *model.User, target *model.Note, reaction string)
}

// ChartHook is invoked after a reaction is created so the chart
// subsystem can record per-user reaction counts. パッケージ間の循環
// 依存を避けるため interface で受け取る (実装は core/chart/charthook)。
type ChartHook interface {
	OnReactionCreated(reactor *model.User, note *model.Note)
}

// WebhookHook is invoked after a reaction has been created so that user
// webhooks subscribed to the `reaction` event can fire. 循環依存を避けるため
// interface で受け取る (実装は core/webhook)。
type WebhookHook interface {
	OnReactionCreated(note *model.Note, reactor *model.User, reaction string)
}

// NoteStreamEmoji is the custom-emoji descriptor included in the
// `reacted` payload published to `noteStream:<noteID>`. Name は TS upstream の
// wire format に合わせて `<short>@<host>` または `<short>@.` (local) 形式。
type NoteStreamEmoji struct {
	Name string
	URL  string
}

// NoteStreamHook is invoked after a reaction is created or removed so the
// stream layer can publish `reacted` / `unreacted` events to the per-note
// `noteStream:<noteID>` topic. This drives real-time WebSocket updates for
// clients that have called `subNote` / `sn` (#700)。実装は server 配線層で
// stream.NoteEventPublisher を注入する adapter として与える。
type NoteStreamHook interface {
	OnReacted(noteID, userID, reaction string, emoji *NoteStreamEmoji)
	OnUnreacted(noteID, userID, reaction string)
}

// Service manages note reactions.
// UserRolesProvider reports the roles a user holds, for role-gated custom emoji
// reactions (#1538). *role.Service implements it. nil 注入時は role gate を
// skip する (= pre-#1538 挙動)。
type UserRolesProvider interface {
	GetUserRoles(userID string) ([]*model.Role, error)
}

// MediaSilenceChecker reports whether a (remote) host is media-silenced, for
// rejecting custom emoji reactions from such hosts (#1538). *instance.Service
// implements it (IsMediaSilenced). nil 注入時は media-silence gate を skip する。
type MediaSilenceChecker interface {
	IsMediaSilenced(host string) bool
}

// FeaturedRanking abstracts the engagement ranking store (#1687). 循環依存回避の
// ため narrow interface で受け取る (実装は core/featured.Service)。reaction は
// 対象 note の global / in-channel / per-user ranking を更新する。
type FeaturedRanking interface {
	UpdateGlobalNotesRanking(ctx context.Context, noteID string, score float64) error
	UpdateInChannelNotesRanking(ctx context.Context, channelID, noteID string, score float64) error
	UpdatePerUserNotesRanking(ctx context.Context, userID, noteID string, score float64) error
}

type Service struct {
	noteRepo         repository.NoteRepository
	reactionRepo     repository.NoteReactionRepository
	emojiRepo        repository.EmojiRepository
	followingRepo    repository.FollowingRepository
	idGen            id.Generator
	notificationHook NotificationHook
	blockingChecker  BlockingChecker
	federationHook   FederationHook
	chartHook        ChartHook
	webhookHook      WebhookHook
	noteStreamHook   NoteStreamHook
	countWriter      ReactionCountWriter
	// materializer はリレー由来で DB に無いノートを昇格させる (#2332)。
	// note_reaction.noteId が note への外部キーなので、行が無いと INSERT が
	// 失敗する。nil なら従来どおり DB のみを見る。
	materializer    NoteMaterializer
	userRoles       UserRolesProvider
	mediaSilence    MediaSilenceChecker
	featuredRanking FeaturedRanking
	// randFn は featured ランキング更新の 30% sampling 用。テストで固定する。
	randFn func() float64
}

// SetFeaturedRanking wires the engagement ranking store so reactions update the
// featured ranking (#1687). nil 注入時 (= 未配線 / test) は ranking 更新を skip。
func (s *Service) SetFeaturedRanking(r FeaturedRanking) {
	s.featuredRanking = r
}

// SetUserRolesProvider wires role lookup for role-gated emoji reaction gating (#1538).
func (s *Service) SetUserRolesProvider(p UserRolesProvider) {
	s.userRoles = p
}

// SetMediaSilenceChecker wires the media-silence host check for reaction gating (#1538).
func (s *Service) SetMediaSilenceChecker(c MediaSilenceChecker) {
	s.mediaSilence = c
}

// SetCountWriter replaces the default direct writer with a buffered one.
func (s *Service) SetCountWriter(w ReactionCountWriter) {
	s.countWriter = w
}

// CountWriter returns the active ReactionCountWriter (for read merge).
func (s *Service) CountWriter() ReactionCountWriter {
	return s.countWriter
}

// NewService constructs a new ReactionService.
func NewService(
	noteRepo repository.NoteRepository,
	reactionRepo repository.NoteReactionRepository,
	emojiRepo repository.EmojiRepository,
	followingRepo repository.FollowingRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		noteRepo:      noteRepo,
		reactionRepo:  reactionRepo,
		emojiRepo:     emojiRepo,
		followingRepo: followingRepo,
		idGen:         idGen,
		countWriter:   NewDirectWriter(noteRepo),
		randFn:        rand.Float64,
	}
}

// SetNotificationHook attaches a NotificationHook used after Create succeeds.
func (s *Service) SetNotificationHook(h NotificationHook) {
	s.notificationHook = h
}

// SetBlockingChecker attaches a BlockingChecker used by Create.
func (s *Service) SetBlockingChecker(c BlockingChecker) {
	s.blockingChecker = c
}

// SetFederationHook attaches a FederationHook used after Create / Delete
// succeed.
func (s *Service) SetFederationHook(h FederationHook) {
	s.federationHook = h
}

// SetChartHook attaches a ChartHook invoked after a reaction is
// created so the chart subsystem can record the event.
func (s *Service) SetChartHook(h ChartHook) {
	s.chartHook = h
}

// SetWebhookHook attaches a WebhookHook invoked after a reaction has been
// created so that user webhooks subscribed to the reaction event can fire.
func (s *Service) SetWebhookHook(h WebhookHook) {
	s.webhookHook = h
}

// NoteMaterializer promotes a relay-delivered note out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
type NoteMaterializer interface {
	EnsureNote(ctx context.Context, noteID string) (*model.Note, error)
}

// SetNoteMaterializer attaches the ephemeral-note materializer. Optional.
func (s *Service) SetNoteMaterializer(m NoteMaterializer) {
	s.materializer = m
}

// materializeIfMissing promotes an ephemeral note only when the database
// lookup already failed.
//
// 通常のノートでは Redis を一切引かないので、ホットパスに追加コストが無い。
func (s *Service) materializeIfMissing(noteID string, lookupErr error) bool {
	if lookupErr == nil || s.materializer == nil {
		return false
	}
	_, err := s.materializer.EnsureNote(context.Background(), noteID)
	return err == nil
}

// SetNoteStreamHook attaches a NoteStreamHook invoked after a reaction is
// created or removed so the stream layer can publish reacted/unreacted
// events to the per-note pubsub topic (#700)。
func (s *Service) SetNoteStreamHook(h NoteStreamHook) {
	s.noteStreamHook = h
}

// Create attaches a reaction by user to the target note.
// 同じユーザーが既に同じリアクションをしている場合は ErrAlreadyReacted。
// 異なるリアクションを既にしている場合は古い方を削除して新しい方を作成する。
func (s *Service) Create(user *model.User, noteID, rawReaction string) (string, error) {
	if user == nil {
		return "", errors.New("user is required")
	}

	// webhookHook.OnReactionCreated が target を PackNote 経由で payload に
	// 詰めるため、Renote / Reply embed を含む full 経路を踏む (#425)。軽量な
	// FindByIDWithUser を使うと webhook 受信側が note.renote / reply の
	// 欠落で Misskey TS 互換が崩れる。
	target, err := s.noteRepo.FindByIDWithRelations(noteID)
	// **materialize の前に分ける** (#2799)。not-found でない error は
	// materialize しても直らないのに、EnsureUser / EnsureNote が ephemeral
	// store の引きを 1 回余分に走らせる。ヒットすればそのまま WebFinger +
	// actor fetch まで進むので、DB 断中は 1 リクエストごとに outbound HTTP が
	// 出うる。
	if err != nil && !repository.IsNotFound(err) {
		return "", err
	}
	if s.materializeIfMissing(noteID, err) {
		target, err = s.noteRepo.FindByIDWithRelations(noteID)
	}
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。materialize の再試行後も
		// 種別は残るので、ここで分ける。
		if !repository.IsNotFound(err) {
			return "", err
		}
		return "", ErrNoteNotFound
	}
	if !note.CanSeeNote(user, target, s.followingRepo) {
		return "", ErrNoteNotVisible
	}

	// 投稿者にブロックされている場合はリアクション不可
	if s.blockingChecker != nil && target.UserID != user.ID {
		if blocked, err := s.blockingChecker.IsBlocked(target.UserID, user.ID); err != nil {
			return "", err
		} else if blocked {
			return "", ErrBlocked
		}
	}

	// pure renote (text/cw/file/poll を伴わない renote) にはリアクションできない
	if isPureRenote(target) {
		return "", ErrCannotReactToPureRenote
	}

	reaction, reactionEmoji := s.resolveReaction(rawReaction, user.Host)
	// note.reactionAcceptance + role/sensitive/media-silence gate を適用し、
	// 受理できない reaction は ❤ にフォールバックする (#1538, 本家
	// ReactionService.create 準拠)。local / 連合 inbound 双方が本 Create を通る。
	reaction = s.applyReactionAcceptance(user, target, reaction, reactionEmoji)

	// 既存リアクションを確認
	if existing, err := s.reactionRepo.FindByPair(user.ID, target.ID); err == nil {
		if existing.Reaction == reaction {
			return "", ErrAlreadyReacted
		}
		// 別のリアクションがすでにあるので置き換える
		affected, err := s.reactionRepo.Delete(existing)
		if err != nil {
			return "", err
		}
		if affected == 0 {
			return "", ErrReactionNotFound
		}
		// 集計列も古いリアクションを-1
		_ = s.countWriter.Increment(target.ID, existing.Reaction, -1)
		// 連合先には古いリアクションを Undo Like で送る
		if s.federationHook != nil {
			s.federationHook.OnReactionRemoved(user, target, existing.Reaction)
		}
		// 古いリアクションの通知を消す (#3201)。残すと付け替えのたびに通知が
		// 増え、取り消し済みのリアクションが並ぶ。新しい通知は下で作る。
		if s.notificationHook != nil && target.UserID != user.ID {
			s.notificationHook.OnReactionRemoved(target.UserID, user.ID, target.ID)
		}
		// #2106 N16: noteStream に古いリアクションの unreacted を publish する。upstream
		// ReactionService.create の置き換えは delete() 経由で unreacted を発火するが、mk-go は
		// 新 reaction の reacted のみ送っており、subNote 購読クライアントで古い reaction の
		// カウントが reload まで残存していた。reacted/unreacted を対称にする。
		if s.noteStreamHook != nil {
			s.noteStreamHook.OnUnreacted(target.ID, user.ID, decodeReactionForStream(existing.Reaction))
		}
	}

	rec := &model.NoteReaction{
		ID:       s.idGen.Generate(time.Now()),
		UserID:   user.ID,
		NoteID:   target.ID,
		Reaction: reaction,
	}
	if err := s.reactionRepo.Create(rec); err != nil {
		// `FindByPair` 実行後の窓で並行 Create (= 別 inbox process / 別
		// reaction 経路) が同 user×note を入れたとき、PG unique constraint
		// で `ErrDuplicateReaction` を取る。AP の Like activity は
		// idempotent に処理する (= 重複は silent ignore する) のが仕様
		// なので、repository level の sentinel を service level の
		// `ErrAlreadyReacted` に変換して caller (= handleLike / API
		// handler) の既存 swallow path を使わせる (#1186)。
		//
		// 注意: mk-go 内で `reactionRepo.Create` を直接呼ぶ caller は
		// 本 service.Create のみ (= 単一 chokepoint)。将来 service 経由
		// しない直接 caller を増やす場合、その経路でも同等の sentinel
		// 変換が必要 (= 直接呼ぶより service 経由に揃えるのが望ましい)。
		if errors.Is(err, repository.ErrDuplicateReaction) {
			return "", ErrAlreadyReacted
		}
		return "", err
	}
	_ = s.countWriter.Increment(target.ID, reaction, 1)

	// 通知発行 (自分自身へのリアクションは内部で抑制される)
	if s.notificationHook != nil && target.UserID != user.ID {
		s.notificationHook.OnReactionCreated(target.UserID, user.ID, target.ID, reaction)
	}
	// AP配信もベストエフォート。
	if s.federationHook != nil {
		s.federationHook.OnReactionAdded(user, target, reaction)
	}
	// チャート集計もベストエフォート。
	if s.chartHook != nil {
		s.chartHook.OnReactionCreated(user, target)
	}
	// Webhook配信もベストエフォート。
	if s.webhookHook != nil {
		s.webhookHook.OnReactionCreated(target, user, reaction)
	}
	// noteStream に reacted を publish して subNote 購読中の WebSocket
	// クライアントへ即時反映する (#700)。
	if s.noteStreamHook != nil {
		s.noteStreamHook.OnReacted(target.ID, user.ID, decodeReactionForStream(reaction), s.resolveStreamEmoji(reaction))
	}
	// featured engagement ランキング更新もベストエフォート (#1687)。
	s.updateFeaturedOnReaction(target, user.ID)

	return reaction, nil
}

// updateFeaturedOnReaction boosts the reacted note's featured engagement ranking
// (#1687, upstream ReactionService)。30% sampling、self-reaction 除外、note が
// 3日以内、の gate を通った場合のみ。channel note は in-channel ranking、それ以外は
// public かつ local かつ非 reply のときに global + per-user ranking を更新する。
func (s *Service) updateFeaturedOnReaction(target *model.Note, reactorID string) {
	if s.featuredRanking == nil || s.randFn == nil {
		return
	}
	if s.randFn() >= featuredSampleRate {
		return
	}
	if target.UserID == reactorID {
		return
	}
	created, err := s.idGen.ParseTime(target.ID)
	if err != nil || time.Since(created) >= featuredMaxNoteAge {
		return
	}
	ctx := context.Background()
	if target.ChannelID != nil && *target.ChannelID != "" {
		if target.ReplyID == nil {
			_ = s.featuredRanking.UpdateInChannelNotesRanking(ctx, *target.ChannelID, target.ID, 1)
		}
		return
	}
	if target.Visibility == model.NoteVisibilityPublic && target.UserHost == nil && target.ReplyID == nil {
		_ = s.featuredRanking.UpdateGlobalNotesRanking(ctx, target.ID, 1)
		_ = s.featuredRanking.UpdatePerUserNotesRanking(ctx, target.UserID, target.ID, 1)
	}
}

// Delete removes the user's reaction from the note.
func (s *Service) Delete(user *model.User, noteID string) error {
	if user == nil {
		return errors.New("user is required")
	}
	target, err := s.noteRepo.FindByID(noteID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNoteNotFound
	}
	existing, err := s.reactionRepo.FindByPair(user.ID, target.ID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrReactionNotFound
	}
	affected, err := s.reactionRepo.Delete(existing)
	if err != nil {
		return err
	}
	// #2106 L40: 並行 unreact で既に削除済 (affected==0) の場合はカウント減算・Undo・stream
	// 発火を全てスキップする (upstream の `if (result.affected !== 1) throw` 相当、二重デクリ防止)。
	if affected == 0 {
		return ErrReactionNotFound
	}
	_ = s.countWriter.Increment(target.ID, existing.Reaction, -1)
	if s.federationHook != nil {
		s.federationHook.OnReactionRemoved(user, target, existing.Reaction)
	}
	// 取り消したリアクションの通知を消す (#3201)。連合の Undo(Like) もここを通る。
	// upstream は消さない。自分へのリアクションは通知を作っていない。
	if s.notificationHook != nil && target.UserID != user.ID {
		s.notificationHook.OnReactionRemoved(target.UserID, user.ID, target.ID)
	}
	// noteStream に unreacted を publish して subNote 購読中の WebSocket
	// クライアントへ即時反映する (#700)。
	if s.noteStreamHook != nil {
		s.noteStreamHook.OnUnreacted(target.ID, user.ID, decodeReactionForStream(existing.Reaction))
	}
	return nil
}

// List returns reactions for the given note id.
func (s *Service) List(user *model.User, noteID, untilID, sinceID string, limit int, reaction string) ([]*model.NoteReaction, error) {
	target, err := s.noteRepo.FindByIDWithUser(noteID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrNoteNotFound
	}
	// #2106 L37 (意図的 divergence): upstream notes/reactions は requireCredential:false で
	// 可視性 filter を一切掛けず、followers/specified note の reaction list も誰にでも 200 で
	// 返す。mk-go は閲覧不可 viewer に CanSeeNote gate を掛け 404 NO_SUCH_NOTE を返す
	// (本文だけでなく reaction list も漏らさない privacy 強化)。worse な upstream 露出に
	// 揃えず mk-go の堅牢な挙動を維持する。
	if !note.CanSeeNote(user, target, s.followingRepo) {
		return nil, ErrNoteNotVisible
	}
	if limit <= 0 {
		limit = 10
	}
	var reactions []string
	if reaction != "" {
		// #2106 N17: type filter は emoji 存在検証なしで正規化する。resolveReaction だと
		// 未キャッシュの remote custom emoji が ❤ に化けて誤った reaction を返していた。
		reactions = reactionVariants(s.normalizeReactionForFilter(reaction))
	}
	return s.reactionRepo.ListByNoteID(target.ID, untilID, sinceID, limit, reactions)
}

// normalizeReactionForFilter normalizes a reaction string for use as the
// notes/reactions type filter. Unlike resolveReaction it does NOT verify custom
// emoji existence (#2106 N17: an uncached remote custom emoji like
// ":foo@remote.example:" was rewritten to ❤ and the filter returned hearts instead
// of the requested reaction) and does NOT fall back non-emoji unicode to ❤ (a
// garbage filter simply matches nothing). Local custom emoji are normalized to the
// "@." canonical so reactionVariants also queries the bare ":name:" form; remote
// custom emoji and unicode are kept verbatim (unicode only loses its variation
// selector to match the stored canonical). Mirrors upstream notes/reactions which
// filters by the requested type verbatim.
func (s *Service) normalizeReactionForFilter(raw string) string {
	if v, ok := reactionlegacy.Convert(raw); ok {
		return v
	}
	if m := customEmojiPattern.FindStringSubmatch(raw); m != nil {
		name := m[1]
		host := ""
		if len(m) >= 3 {
			host = m[2]
		}
		if host == "" || host == "." {
			return ":" + name + "@.:"
		}
		return ":" + name + "@" + host + ":"
	}
	return stripVariationSelector(raw)
}

// resolveReaction returns the canonical form of a reaction string together with
// the resolved custom emoji (nil for legacy/unicode/empty reactions or when a
// custom emoji is not found and falls back). Create uses the emoji for
// reactionAcceptance gating (#1538) so it does not re-query the emoji table.
//
// 正規化の規則:
//   - 空の文字列はFallback (heart) に置換
//   - レガシー文字列(like等)はUnicode絵文字に変換
//   - カスタム絵文字 ":name:" は絵文字テーブルで存在確認後 ":name@.:" に正規化
//   - リモート ":name@host:" はそのまま検証して残す
//   - その他は Unicode 絵文字として**検証する** — 絵文字以外を含めば Fallback へ倒し
//     (#2106 N15)、絵文字なら variation selector (U+FE0F) を落とした canonical 形に
//     揃える (#864)。列 (varchar(260)) に収まらない長さも Fallback (#2726)
//
// actorHost は reaction を投稿した側のホスト。リモートユーザーが
// `:name:` 形式 (ホスト省略) で送ってきたとき、Misskey TS upstream は
// reactor のホストで emoji table を引き直すので、本実装でも actorHost
// にフォールバックする (#459)。actorHost が nil/空なら従来通りローカル
// として扱う。
func (s *Service) resolveReaction(raw string, actorHost *string) (string, *model.Emoji) {
	reaction, emoji := s.resolveReactionValue(raw, actorHost)
	// `note_reaction.reaction` は varchar(260)。**絵文字だけで構成された長い
	// 文字列**は下の gomoji gate を通ってしまうので、ここで列に収まるかを見る
	// (絵文字 300 個の Like は 22001 で Create ごと落ち、Like 配送が retry を
	// 使い切って dead になる、#2726)。
	//
	// 切ると grapheme が壊れるうえ別の reaction になるので、収まらなければ
	// **❤ に倒す**。upstream の normalize は `emojiRegex.exec()` の**先頭 1 つ
	// だけ**を採るので同じ入力でも溢れない (mk-go は全体を保存する点で乖離)。
	if !colfit.Fits(reaction, reactionMaxRunes) {
		return FallbackReaction, nil
	}
	return reaction, emoji
}

// resolveReactionValue is resolveReaction without the storage-fit check.
func (s *Service) resolveReactionValue(raw string, actorHost *string) (string, *model.Emoji) {
	if raw == "" {
		return FallbackReaction, nil
	}
	if v, ok := reactionlegacy.Convert(raw); ok {
		return v, nil
	}
	if m := customEmojiPattern.FindStringSubmatch(raw); m != nil {
		name := m[1]
		host := ""
		if len(m) >= 3 {
			host = m[2]
		}
		// "@." はローカルを表すcanonical suffix (TS互換)
		if host == "." {
			host = ""
		}
		// reaction 文字列に @host が含まれず reactor が remote なら、
		// 文字列 host が省略されているだけで実体はリモート絵文字。
		// upstream ReactionService.create がやっているのと同じく
		// actor.host を採用して emoji table を引く。
		if host == "" && actorHost != nil && *actorHost != "" {
			host = *actorHost
		}
		var hostPtr *string
		if host != "" {
			hostPtr = &host
		}
		if emoji, err := s.emojiRepo.FindByNameAndHost(name, hostPtr); err == nil {
			if host == "" {
				return ":" + name + "@.:", emoji
			}
			return ":" + name + "@" + host + ":", emoji
		}
		// 見つからなければFallbackにする
		return FallbackReaction, nil
	}
	// #2106 N15: upstream ReactionService.normalize は emojiRegex に一致しない
	// (= 非絵文字・非legacy・非custom の) 入力を ❤ (FallbackReaction) に矯正する。
	// mk-go は未検証で任意文字列をそのまま reaction として保存していたため、gomoji で
	// raw が全て絵文字かを判定し、非絵文字を含むなら fallback する。resolveReaction は
	// Create / AP inbound (Like の _misskey_reaction) 双方が通る chokepoint なので、
	// ここに置けば local/連合とも任意文字列 reaction の蓄積を防げる。
	if gomoji.RemoveEmojis(raw) != "" {
		return FallbackReaction, nil
	}
	// Unicode emoji は variation selector (U+FE0F) を strip して upstream
	// Misskey TS と同じ canonical form に揃える (#864)。同じ emoji の異なる
	// encode (例: U+2764 と U+2764 + U+FE0F) を 1 つの key として扱う。
	return stripVariationSelector(raw), nil
}

// applyReactionAcceptance enforces note.reactionAcceptance + role-gated /
// sensitive / media-silenced custom emoji rules, mirroring upstream
// ReactionService.create. It returns the (possibly downgraded-to-❤) reaction.
// reaction/emoji come from resolveReaction; emoji is nil for non-custom reactions.
// Notes without reactionAcceptance and reactions that hit none of the gates keep
// their resolved value, so the default reaction path is unchanged (#1538).
func (s *Service) applyReactionAcceptance(user *model.User, target *model.Note, reaction string, emoji *model.Emoji) string {
	acc := ""
	if target.ReactionAcceptance != nil {
		acc = *target.ReactionAcceptance
	}
	remote := !user.IsLocal()

	// likeOnly は常に ❤。likeOnlyForRemote / nonSensitiveOnlyForLocalLikeOnlyForRemote
	// は remote reactor のとき ❤ (本家 ReactionService.create)。
	if acc == reactionAcceptanceLikeOnly ||
		((acc == reactionAcceptanceLikeOnlyForRemote || acc == reactionAcceptanceNonSensitiveForLocalLikeForRemote) && remote) {
		return FallbackReaction
	}

	// 以降の gate は custom emoji reaction のみ対象。
	if emoji == nil {
		return reaction
	}
	// role-gated emoji: 使用可能 role を持たなければ ❤。
	if len(emoji.RoleIDsThatCanBeUsedThisEmojiAsReaction) > 0 && !s.reactorHasAllowedRole(user.ID, emoji.RoleIDsThatCanBeUsedThisEmojiAsReaction) {
		return FallbackReaction
	}
	// media-silenced reacter host (remote のみ) は ❤。
	if remote && user.Host != nil && s.mediaSilence != nil && s.mediaSilence.IsMediaSilenced(*user.Host) {
		return FallbackReaction
	}
	// nonSensitive 系で sensitive emoji は ❤。
	if (acc == reactionAcceptanceNonSensitiveOnly || acc == reactionAcceptanceNonSensitiveForLocalLikeForRemote) && emoji.IsSensitive {
		return FallbackReaction
	}
	return reaction
}

// reactorHasAllowedRole reports whether the user holds any of the roles allowed
// to use a role-gated emoji. When no UserRolesProvider is wired the gate is
// skipped (returns true = pre-#1538 behavior); production always wires it.
func (s *Service) reactorHasAllowedRole(userID string, allowed []string) bool {
	if s.userRoles == nil {
		return true
	}
	roles, err := s.userRoles.GetUserRoles(userID)
	if err != nil {
		return false
	}
	for _, r := range roles {
		if slices.Contains(allowed, r.ID) {
			return true
		}
	}
	return false
}

// stripVariationSelector removes Unicode emoji variation selector (U+FE0F)
// from the input. emoji-style 表示用の variation selector は実体の emoji
// codepoint を変えないため、reaction key の正規化として safe に削除できる。
// upstream Misskey TS は同様の正規化を行うため、両 backend の reaction
// 文字列を揃えるのに必要 (#864)。
func stripVariationSelector(s string) string {
	// #2106 L39: upstream normalize \u306f ZWJ (U+200D) \u3092\u542b\u3080\u5408\u5b57\u7d75\u6587\u5b57 (\u4e00\u90e8\u306e\u8077\u696d/\u5bb6\u65cf\u7d75\u6587\u5b57)
	// \u3067\u306f U+FE0F \u3092\u6b8b\u3059 (`unicode.match('\u200d') ? unicode : unicode.replace(/\ufe0f/g, '')`)\u3002
	// ZWJ \u3092\u542b\u3080\u5834\u5408\u306f variation selector \u3092 strip \u305b\u305a\u539f\u6587\u3092\u8fd4\u3057 reaction key \u3092 upstream \u306b\u63c3\u3048\u308b\u3002
	if strings.Contains(s, "\u200d") {
		return s
	}
	return strings.ReplaceAll(s, "\ufe0f", "")
}

// localCanonicalPattern matches the canonical local emoji form `:name@.:`.
var localCanonicalPattern = regexp.MustCompile(`^:([\w+\-]+)@\.:$`)

// reactionVariants returns a slice of reaction strings to match in the DB.
// TS時代のレコードは `:name:` 形式、mk時代は `:name@.:` 形式で保存されて
// いるため、ローカルカスタム絵文字の場合は両方の形式で検索する必要がある。
func reactionVariants(normalized string) []string {
	if m := localCanonicalPattern.FindStringSubmatch(normalized); m != nil {
		return []string{normalized, ":" + m[1] + ":"}
	}
	return []string{normalized}
}

// decodeReactionForStream returns the canonical wire form of a reaction
// string for noteStream payload (`:name@host:` / `:name@.:` for custom emoji,
// raw string for unicode/legacy). 上流 ReactionService.ts の decodeReaction と
// 同形式で、ホスト省略時 `.` を補う。resolveReaction と違い emoji 存在検証
// やフォールバックは行わない (DB に既に保存された値をそのまま wire 化する用途)。
func decodeReactionForStream(raw string) string {
	m := customEmojiPattern.FindStringSubmatch(raw)
	if m == nil {
		return raw
	}
	name := m[1]
	host := "."
	if len(m) >= 3 && m[2] != "" && m[2] != "." {
		host = m[2]
	}
	return ":" + name + "@" + host + ":"
}

// resolveStreamEmoji returns the NoteStreamEmoji descriptor for a custom
// reaction, looking up the emoji in the repository. Returns nil for
// unicode / legacy reactions or when the emoji is unknown. URL は
// publicUrl 優先で originalUrl に fallback する (上流互換)。
func (s *Service) resolveStreamEmoji(reaction string) *NoteStreamEmoji {
	m := customEmojiPattern.FindStringSubmatch(reaction)
	if m == nil {
		return nil
	}
	name := m[1]
	host := ""
	if len(m) >= 3 {
		host = m[2]
	}
	if host == "." {
		host = ""
	}
	var hostPtr *string
	if host != "" {
		hostPtr = &host
	}
	emoji, err := s.emojiRepo.FindByNameAndHost(name, hostPtr)
	if err != nil || emoji == nil {
		return nil
	}
	url := emoji.PublicURL
	if url == "" {
		url = emoji.OriginalURL
	}
	suffix := host
	if suffix == "" {
		suffix = "."
	}
	return &NoteStreamEmoji{
		Name: name + "@" + suffix,
		URL:  url,
	}
}

// isPureRenote reports whether the given note is a pure renote (no text/cw/files/poll).
func isPureRenote(n *model.Note) bool {
	if n.RenoteID == nil {
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
	// renote + reply は quote 扱いなので pure renote ではない (#1886、upstream isQuote)。
	if n.ReplyID != nil && *n.ReplyID != "" {
		return false
	}
	return true
}

// HasUserRolesProvider reports whether the user roles provider was wired.
//
// 未配線だとロール限定カスタム絵文字の gate が素通しになり、誰でも使える。
// 起動時検査に使う (#2683)。
func (s *Service) HasUserRolesProvider() bool { return s.userRoles != nil }

// HasMediaSilenceChecker reports whether the media-silence checker was wired.
//
// 未配線だと media-silenced な host からのカスタム絵文字リアクションがそのまま
// 出る (フォールバックの ❤ にならない)。起動時検査に使う (#2708)。
func (s *Service) HasMediaSilenceChecker() bool { return s.mediaSilence != nil }

// HasBlockingChecker reports whether the blocking checker was wired.
//
// 未配線だと**自分をブロックしている**相手の投稿にリアクションできる。gate は
// `IsBlocked(target.UserID, user.ID)` (blocker が note 著者) なので、自分が
// ブロックした相手への操作はそもそもここを通らない。起動時検査に使う (#2708)。
func (s *Service) HasBlockingChecker() bool { return s.blockingChecker != nil }
