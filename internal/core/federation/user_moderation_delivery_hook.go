package federation

import (
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// UserModerationDeliveryHook emits AP Delete / Undo(Delete) activities for a
// local user's actor when the account is suspended, deleted, or unsuspended
// (#1759). Mirrors upstream UserSuspendService / DeleteAccountService, which
// deliver Delete(actor) to every known sharedInbox on suspend/delete and
// Undo(Delete) on unsuspend.
//
// 配信ルール (NoteDeleteDeliveryHook と同方針):
//   - user がリモート → 配信不要 (当該インスタンスが発火元)
//   - それ以外 → フォロワー (リモート) + 既知のリモートインスタンス全体
//     (sharedInbox 単位) に配信する。フォロー関係に無い instance も ap/show
//     で actor を pull 済みのことがあるためブロードキャストで補償する。
type UserModerationDeliveryHook struct {
	deliver  *DeliverService
	renderer *activitypub.Renderer
	userRepo repository.UserRepository
}

// NewUserModerationDeliveryHook constructs a UserModerationDeliveryHook.
func NewUserModerationDeliveryHook(
	deliver *DeliverService,
	renderer *activitypub.Renderer,
	userRepo repository.UserRepository,
) *UserModerationDeliveryHook {
	return &UserModerationDeliveryHook{deliver: deliver, renderer: renderer, userRepo: userRepo}
}

// OnUserDeleted delivers a Delete(actor) for a suspended or deleted local user.
// delete-account 経路では keypair が cascade で消える前に呼ぶこと (署名に必要)。
func (h *UserModerationDeliveryHook) OnUserDeleted(user *model.User) {
	if user == nil || h.renderer == nil {
		return
	}
	h.broadcast(user, h.renderer.RenderDeleteActor(user))
}

// OnUserRestored delivers an Undo(Delete) for an unsuspended local user.
func (h *UserModerationDeliveryHook) OnUserRestored(user *model.User) {
	if user == nil || h.renderer == nil {
		return
	}
	h.broadcast(user, h.renderer.RenderUndoDeleteActor(user))
}

// broadcast signs the activity with the user's key and delivers it to the
// user's remote followers plus every known remote sharedInbox. Errors are
// best-effort logged (moderation actions must not fail on delivery hiccups).
func (h *UserModerationDeliveryHook) broadcast(user *model.User, activity any) {
	if h.deliver == nil || !user.IsLocal() {
		return
	}
	body, err := json.Marshal(activity)
	if err != nil {
		slog.Warn("user moderation delivery: marshal failed", "userId", user.ID, "err", err)
		return
	}
	// **フォロワー配送と重ねない (#2575)。** 既知リモートはフォロワーの上位集合
	// (どちらも COALESCE(NULLIF(sharedInbox,''), inbox) で解決する) なので、両方
	// 走らせると全フォロワーが同じ activity を同じ URL に 2 回受け取る。
	//
	// 一覧が引けないときはフォロワーには送る。何も送らないのは今より悪い。
	if h.broadcast_(user, body) {
		return
	}
	if err := h.deliver.DeliverToFollowers(user.ID, body); err != nil {
		slog.Warn("user moderation delivery: followers deliver failed", "userId", user.ID, "err", err)
	}
}

// broadcast_ sends to every known remote inbox, reporting whether it took over
// delivery.
func (h *UserModerationDeliveryHook) broadcast_(user *model.User, body []byte) bool {
	if h.userRepo == nil {
		return false
	}
	inboxes, err := h.userRepo.ListRemoteInboxes()
	if err != nil {
		slog.Warn("user moderation delivery: list remote inboxes failed", "userId", user.ID, "err", err)
		return false
	}
	if len(inboxes) == 0 {
		// **0 件を「送るものが無い」と解釈しない。** 既知リモートがフォロワーの
		// 上位集合であることに寄りかかると、片方のクエリだけが変わったときに
		// 黙って配送が消える。空ならフォロワー配送に任せる。
		return false
	}
	if err := h.deliver.DeliverToInboxes(user.ID, body, inboxes); err != nil {
		slog.Warn("user moderation delivery: broadcast failed", "userId", user.ID, "err", err)
	}
	return true
}
