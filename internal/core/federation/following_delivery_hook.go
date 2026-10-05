package federation

import (
	"encoding/json"
	"log/slog"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
)

// FollowingDeliveryHook implements core/following.FederationHook by emitting
// Follow / Undo Follow / Accept activities to remote inboxes.
//
// 配信は次のケースだけ行う:
//   - ローカル user が remote user をフォロー → Follow を remote inbox に送信
//   - ローカル user が remote user をアンフォロー → Undo Follow を送信
//   - ローカル user が remote user の follow request を受諾 → Accept を送信
//     (req.FollowerID がリモート、followee がローカル)
type FollowingDeliveryHook struct {
	deliver  *DeliverService
	renderer *activitypub.Renderer
	urls     *activitypub.URLBuilder
}

// NewFollowingDeliveryHook constructs a FollowingDeliveryHook.
func NewFollowingDeliveryHook(deliver *DeliverService, renderer *activitypub.Renderer, urls *activitypub.URLBuilder) *FollowingDeliveryHook {
	return &FollowingDeliveryHook{deliver: deliver, renderer: renderer, urls: urls}
}

// OnLocalFollowed sends a Follow activity to a remote followee.
func (h *FollowingDeliveryHook) OnLocalFollowed(follower, followee *model.User) {
	if !shouldDeliverFollow(follower, followee) {
		return
	}
	follow := h.renderer.RenderFollow(follower.ID, *followee.URI)
	body, _ := json.Marshal(follow)
	if err := h.deliver.DeliverToUser(follower.ID, followee, body); err != nil {
		slog.Warn("following delivery: follow failed",
			"follower", follower.ID, "followee", followee.ID, "err", err)
	}
}

// OnLocalUnfollowed sends an Undo Follow (local→remote unfollow) or Reject
// Follow (local user removing a remote follower via /following/invalidate)
// activity so that the remote server clears its side of the relationship.
//
// 本家 UserFollowingService.unfollow / decrementFollowing と同じ分岐:
//   - follower local  / followee remote: Undo(Follow)
//   - follower remote / followee local : Reject(Follow)
//   - それ以外 (両方ローカル等) は no-op
func (h *FollowingDeliveryHook) OnLocalUnfollowed(follower, followee *model.User) {
	if follower == nil || followee == nil {
		return
	}
	switch {
	case shouldDeliverFollow(follower, followee):
		// 標準的な local→remote unfollow。Undo(Follow) を follower 名義で送る。
		// renderer 経由で組み、upstream renderUndo と同じく published(.000Z) を付ける (#1993)。
		undo := h.renderer.RenderUndoFollow(follower.ID, *followee.URI)
		body, _ := json.Marshal(undo)
		if err := h.deliver.DeliverToUser(follower.ID, followee, body); err != nil {
			slog.Warn("following delivery: unfollow failed",
				"follower", follower.ID, "followee", followee.ID, "err", err)
		}
	case !follower.IsLocal() && followee.IsLocal() && follower.URI != nil && *follower.URI != "":
		// local followee が remote follower を剥がす (/following/invalidate)
		// 場合は Reject(Follow) を followee 名義で送って相手側の Following
		// 関係を解消させる (本家 decrementFollowing 相当)。
		follow := &activitypub.Follow{
			Activity: activitypub.Activity{
				Object: activitypub.Object{Type: "Follow"},
				Actor:  *follower.URI,
			},
			Object: h.urls.UserURI(followee.ID),
		}
		reject := h.renderer.RenderReject(followee.ID, follow)
		body, _ := json.Marshal(reject)
		if err := h.deliver.DeliverToUser(followee.ID, follower, body); err != nil {
			slog.Warn("following delivery: reject (remove follower) failed",
				"follower", follower.ID, "followee", followee.ID, "err", err)
		}
	}
}

// SendAcceptForInboundFollow implements InboundFollowAcceptor. 処理中の
// inbound Follow activity の raw JSON をそのまま Accept の object に包んで
// follower (remote) の inbox に送る。original Follow の `id` / `actor` /
// `object` が保持されるので、相手サーバーが自分の送った Follow と matching
// できる。
func (h *FollowingDeliveryHook) SendAcceptForInboundFollow(follower, followee *model.User, originalFollow json.RawMessage) error {
	if follower == nil || followee == nil {
		return nil
	}
	if follower.IsLocal() || !followee.IsLocal() {
		// local→remoteやlocal→local方向ならAP配信不要。
		return nil
	}
	// OnLocalFollowAcceptedと同じdefensive guard。現状の呼び出し元はResolveActor
	// 直後なのでURIは必ず埋まっているが、将来DBから再取得するケース等で防御。
	if follower.URI == nil || *follower.URI == "" {
		return nil
	}
	// raw をそのまま innerObject として渡す。json.RawMessage は Marshal 時に
	// そのまま出力されるので、Accept.object フィールドに原 Follow が JSON
	// 構造としてネストされる。
	accept := h.renderer.RenderAccept(followee.ID, originalFollow)
	body, err := json.Marshal(accept)
	if err != nil {
		return err
	}
	return h.deliver.DeliverToUser(followee.ID, follower, body)
}

// SendRejectForInboundFollow implements InboundFollowAcceptor. local followee
// が remote follower を block している状態で inbound Follow を受けた場合に、
// original Follow の raw JSON を Reject に包んで follower の inbox に送る
// (#1631、upstream UserFollowingService.follow の「ブロックしていた場合は
// エラーにするのではなく Reject を送り返しておしまい」相当)。これが無いと
// 相手サーバーの pending follow が永遠に解消されず再送が続く。
func (h *FollowingDeliveryHook) SendRejectForInboundFollow(follower, followee *model.User, originalFollow json.RawMessage) error {
	if follower == nil || followee == nil {
		return nil
	}
	if follower.IsLocal() || !followee.IsLocal() {
		return nil
	}
	if follower.URI == nil || *follower.URI == "" {
		return nil
	}
	// SendAcceptForInboundFollow と同じく raw をそのまま object に包む。
	// 相手サーバーが自分の送った Follow の id と matching できる。
	reject := h.renderer.RenderReject(followee.ID, originalFollow)
	body, err := json.Marshal(reject)
	if err != nil {
		return err
	}
	return h.deliver.DeliverToUser(followee.ID, follower, body)
}

// OnLocalFollowAccepted sends an Accept activity to a remote follower.
// 引数: follower がリモート、followee がローカル (Accept の actor)。
func (h *FollowingDeliveryHook) OnLocalFollowAccepted(follower, followee *model.User) {
	if follower == nil || followee == nil {
		return
	}
	if follower.IsLocal() || !followee.IsLocal() {
		// follower がローカル / followee がリモート の組み合わせは AP配信不要。
		return
	}
	if follower.URI == nil || *follower.URI == "" {
		return
	}
	// 元の Follow を再構成する: actor は remote follower の URI、
	// object はローカル followee の正規URI。
	localFolloweeURI := h.urls.UserURI(followee.ID)
	follow := &activitypub.Follow{
		Activity: activitypub.Activity{
			Object: activitypub.Object{Type: "Follow"},
			Actor:  *follower.URI,
		},
		Object: localFolloweeURI,
	}
	accept := h.renderer.RenderAccept(followee.ID, follow)
	body, _ := json.Marshal(accept)
	if err := h.deliver.DeliverToUser(followee.ID, follower, body); err != nil {
		slog.Warn("following delivery: accept failed",
			"follower", follower.ID, "followee", followee.ID, "err", err)
	}
}

// shouldDeliverFollow reports whether a Follow / Undo Follow should be sent.
// 必須条件: follower がローカル, followee がリモートで URI を持っている。
func shouldDeliverFollow(follower, followee *model.User) bool {
	if follower == nil || followee == nil {
		return false
	}
	if !follower.IsLocal() {
		return false
	}
	if followee.IsLocal() {
		return false
	}
	if followee.URI == nil || *followee.URI == "" {
		return false
	}
	return true
}
