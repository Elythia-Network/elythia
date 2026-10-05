package emojiapplication

import (
	"context"
	"errors"
	"log/slog"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/elythia-network/elythia/internal/model"
)

// InAppNotifier is the subset of the notification service this package needs.
type InAppNotifier interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// ReviewerLister enumerates the users who may review emoji applications.
//
// 実装は `core/role.Service.GetUsersWithPolicy(canManageCustomEmojis)`。
type ReviewerLister interface {
	GetUsersWithPolicy(policyKey string) ([]*model.User, error)
}

// ReceivedNotifier tells the reviewers that a new application arrived (#2987).
//
// 通知は副作用なので、失敗しても申請自体は成立させる (呼び出し側で握る)。
type ReceivedNotifier interface {
	NotifyEmojiApplicationReceived(ctx context.Context, app *model.EmojiApplication) error
}

// NewReceivedNotifier adapts the notification service to ReceivedNotifier.
//
// **宛先は「審査できる人」で、モデレーターではない** (#2987)。審査 endpoint は
// `canManageCustomEmojis` で gate されており、`HasRolePolicy` が短絡するのは
// root と管理者だけ。モデレーターへ配ると「届いたのに押せない」「押せるのに
// 届かない」が同時に起きる。
func NewReceivedNotifier(n InAppNotifier, lister ReviewerLister) ReceivedNotifier {
	if n == nil || lister == nil {
		return nil
	}
	return &receivedNotifier{inner: n, lister: lister}
}

type receivedNotifier struct {
	inner  InAppNotifier
	lister ReviewerLister
}

func (r *receivedNotifier) NotifyEmojiApplicationReceived(ctx context.Context, app *model.EmojiApplication) error {
	if app == nil {
		return nil
	}
	reviewers, err := r.lister.GetUsersWithPolicy(role.PolicyCanManageCustomEmojis)
	if err != nil {
		return err
	}
	for _, u := range reviewers {
		if u == nil {
			continue
		}
		// **本文は載せない。** 名前・ライセンス・画像は読み出し時に申請の行から
		// 引き直す (#2868 の abuseReport と同じ形)。通知へ複製すると、申請を
		// 消しても内容が Redis に残る。
		//
		// **NotifierID は申請者。** 通知サービスは notifier == notifiee を
		// ErrSelfNotification で弾くので、審査できる人が自分で出した申請は
		// 自分の通知欄に出ない。それは正しい (自分が出したことは知っている)。
		_, nerr := r.inner.Create(ctx, notification.CreateInput{
			NotifieeID: u.ID,
			NotifierID: app.UserID,
			Type:       notification.TypeEmojiApplicationReceived,
			Extra: map[string]any{
				"applicationId": app.ID,
			},
		})
		if nerr != nil && !errors.Is(nerr, notification.ErrSelfNotification) {
			slog.Warn("emoji-application: received notification failed",
				"reviewer", u.ID, "application", app.ID, "err", nerr)
		}
	}
	return nil
}

// NewResultNotifier adapts the notification service to ResultNotifier.
//
// **結果そのものは通知に載せない。** 載せるのは applicationId だけで、承認か
// 却下か・却下の理由は読み出し時に申請の行から引き直す (#2868 の abuseReport と
// 同じ形)。理由を通知へ複製すると、申請を消しても文面が Redis に残り続ける。
func NewResultNotifier(n InAppNotifier) ResultNotifier {
	if n == nil {
		return nil
	}
	return &resultNotifier{inner: n}
}

type resultNotifier struct {
	inner InAppNotifier
}

func (r *resultNotifier) NotifyEmojiApplicationProcessed(ctx context.Context, app *model.EmojiApplication) error {
	if app == nil || app.ProcessedByID == nil {
		return nil
	}
	// **NotifierID を渡さない。** 通知サービスは notifier == notifiee を
	// ErrSelfNotification で弾くので、渡すと**自分で自分の申請を処理したときに
	// 通知が出なくなる**。abuseReport はそれで正しい (自分が出した通報の状態は
	// 本人が知っている) が、申請の結果は**相手が決めたこと**で、承認か却下かは
	// 受け取る価値がある。1 人運用のサーバーでは全件が自己処理になるので、
	// 弾くと「機能が動いていない」ように見える。
	//
	// 誰が処理したかは元々通知に出していない (申請者に見せると個人への抗議に
	// 繋がりやすい) ので、落としても失うものが無い。監査は emoji_application の
	// processedById と moderation log が持つ。
	_, err := r.inner.Create(ctx, notification.CreateInput{
		NotifieeID: app.UserID,
		Type:       notification.TypeEmojiApplicationProcessed,
		Extra: map[string]any{
			"applicationId": app.ID,
		},
	})
	return err
}
