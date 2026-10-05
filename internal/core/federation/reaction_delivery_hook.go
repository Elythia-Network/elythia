package federation

import (
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ReactionDeliveryHook implements core/reaction.FederationHook by emitting
// Like / Undo Like activities so the reaction reaches every instance that
// observed the original note.
//
// 配信ルール (Misskey TS の ReactionService.create と一致 / drop-in 互換):
//   - reactor がローカル & note が localOnly でない:
//   - note 作者が remote → 作者の inbox に DirectRecipe
//   - visibility public / home / followers → reactor の remote followers
//     に fanout
//   - visibility specified → visible な remote user 全員に DirectRecipe
//   - reactor がリモート → 配信不要 (どこかからの取り込み済みリアクション)
//   - localOnly note → 配信不要
//
// note 作者がローカル user の場合でも followers fanout は実行する。これに
// より「自分の public note に自分でリアクションした」場合に remote
// followers のタイムラインに反映される (#636)。
type ReactionDeliveryHook struct {
	deliver  *DeliverService
	renderer *activitypub.Renderer
	urls     *activitypub.URLBuilder
	idGen    id.Generator
	userRepo repository.UserRepository
}

// NewReactionDeliveryHook constructs a ReactionDeliveryHook.
func NewReactionDeliveryHook(
	deliver *DeliverService,
	renderer *activitypub.Renderer,
	urls *activitypub.URLBuilder,
	idGen id.Generator,
	userRepo repository.UserRepository,
) *ReactionDeliveryHook {
	return &ReactionDeliveryHook{
		deliver:  deliver,
		renderer: renderer,
		urls:     urls,
		idGen:    idGen,
		userRepo: userRepo,
	}
}

// OnReactionAdded emits a Like activity to all instances that should observe
// the reaction (note author + reactor's followers / specified recipients).
func (h *ReactionDeliveryHook) OnReactionAdded(reactor *model.User, target *model.Note, reaction string) {
	if !h.shouldDeliver(reactor, target) {
		return
	}
	like := h.buildLike(reactor, target, reaction)
	body, _ := json.Marshal(like)
	h.fanout(reactor, target, body, "like")
}

// OnReactionRemoved emits an Undo Like activity with the same recipient set.
func (h *ReactionDeliveryHook) OnReactionRemoved(reactor *model.User, target *model.Note, reaction string) {
	if !h.shouldDeliver(reactor, target) {
		return
	}
	like := h.buildLike(reactor, target, reaction)
	undo := h.renderer.RenderUndoLike(reactor, like)
	body, _ := json.Marshal(undo)
	h.fanout(reactor, target, body, "undo like")
}

// shouldDeliver returns false when the reaction is a no-op for federation
// (remote reactor, localOnly note, missing args).
func (h *ReactionDeliveryHook) shouldDeliver(reactor *model.User, target *model.Note) bool {
	if reactor == nil || target == nil {
		return false
	}
	if !reactor.IsLocal() {
		return false
	}
	if target.LocalOnly {
		return false
	}
	return true
}

// fanout collects DirectRecipe inboxes (note author + specified recipients)
// and follower inboxes per the note visibility, then enqueues the activity
// body to each. visibility に応じて TS の DeliverManager と同じ recipient
// 集合になるよう揃える。
//
// kind は失敗ログ識別用 ("like" / "undo like")。
func (h *ReactionDeliveryHook) fanout(reactor *model.User, target *model.Note, body []byte, kind string) {
	directInboxes := h.directInboxes(target)
	if len(directInboxes) > 0 {
		if err := h.deliver.DeliverActivity(reactor.ID, body, directInboxes); err != nil {
			slog.Warn("reaction delivery: direct failed",
				"kind", kind, "reactor", reactor.ID, "noteId", target.ID, "err", err)
		}
	}
	if h.shouldFanoutToFollowers(target) {
		// direct バッチで送った inbox は followers バッチから除外する。
		// 同一 Like ID を同一 inbox に 2 回 POST しないため、送信側で
		// 重複を排除する (#2567)。受信側の idempotent 処理 (重複排除) に
		// 依存しない設計で、除外は inbox URL 完全一致。
		exclude := make(map[string]bool, len(directInboxes))
		for _, inbox := range directInboxes {
			exclude[inbox] = true
		}
		if err := h.deliver.DeliverToFollowersExcluding(reactor.ID, body, exclude); err != nil {
			slog.Warn("reaction delivery: followers fanout failed",
				"kind", kind, "reactor", reactor.ID, "noteId", target.ID, "err", err)
		}
	}
}

// directInboxes returns the explicit DirectRecipe inboxes: note author (if
// remote) + visible specified users (if remote). 作者が local の場合は省く
// (TS と同じ条件)。
//
// gorm.ErrRecordNotFound (= 削除済み user) と DB transient error を分けて
// log する (#644)。VisibleUserIDs に target.UserID が含まれているケースは
// skip して FindManyByIDs の N+1 を避ける。
func (h *ReactionDeliveryHook) directInboxes(target *model.Note) []string {
	var inboxes []string
	author, err := h.userRepo.FindByID(target.UserID)
	switch {
	case err == nil:
		if !author.IsLocal() {
			if inbox := fanoutInbox(author); inbox != "" {
				inboxes = append(inboxes, inbox)
			}
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 投稿時点で著者が消えているケース。ログ過剰を避けて debug 止まり。
		slog.Debug("reaction delivery: author no longer exists",
			"noteId", target.ID, "userId", target.UserID)
	default:
		slog.Warn("reaction delivery: author lookup failed",
			"noteId", target.ID, "userId", target.UserID, "err", err)
	}

	if target.Visibility == model.NoteVisibilitySpecified && len(target.VisibleUserIDs) > 0 {
		// 著者は上で済んでいるので uid == target.UserID は除外して bulk
		// fetch する (#644 #5 / #7)。
		uniqueIDs := make([]string, 0, len(target.VisibleUserIDs))
		seen := make(map[string]struct{}, len(target.VisibleUserIDs))
		for _, uid := range target.VisibleUserIDs {
			if uid == "" || uid == target.UserID {
				continue
			}
			if _, dup := seen[uid]; dup {
				continue
			}
			seen[uid] = struct{}{}
			uniqueIDs = append(uniqueIDs, uid)
		}
		if len(uniqueIDs) > 0 {
			users, err := h.userRepo.FindManyByIDs(uniqueIDs)
			if err != nil {
				slog.Warn("reaction delivery: visible users lookup failed",
					"noteId", target.ID, "count", len(uniqueIDs), "err", err)
			} else {
				// missing rows は repo 側で silent skip される。debug log
				// で skip 件数だけ残しておく (#644 #2)。
				if len(users) < len(uniqueIDs) {
					slog.Debug("reaction delivery: some visible users missing",
						"noteId", target.ID, "requested", len(uniqueIDs), "found", len(users))
				}
				for _, u := range users {
					if u == nil || u.IsLocal() {
						continue
					}
					if inbox := fanoutInbox(u); inbox != "" {
						inboxes = append(inboxes, inbox)
					}
				}
			}
		}
	}
	return inboxes
}

// shouldFanoutToFollowers reports whether the note visibility triggers
// follower fanout. specified の場合は DirectRecipe で済ませる (TS と一致)。
func (h *ReactionDeliveryHook) shouldFanoutToFollowers(target *model.Note) bool {
	switch target.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers:
		return true
	default:
		return false
	}
}

// buildLike constructs the Like activity used both for create and undo paths.
func (h *ReactionDeliveryHook) buildLike(reactor *model.User, target *model.Note, reaction string) *activitypub.Like {
	targetURI := h.urls.NoteURI(target.ID)
	if target.URI != nil && *target.URI != "" {
		targetURI = *target.URI
	}
	likeID := h.urls.UserURI(reactor.ID) + "/likes/" + h.idGen.Generate(time.Now())
	return h.renderer.RenderLike(reactor, targetURI, reaction, likeID)
}
