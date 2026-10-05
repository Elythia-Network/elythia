package signupapplication

import (
	"context"
	"errors"
	"log/slog"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
)

// InAppNotifier is the subset of the notification service this package needs.
type InAppNotifier interface {
	Create(ctx context.Context, in notification.CreateInput) (*notification.Notification, error)
}

// ModeratorLister enumerates the users who may review signup applications.
//
// 実装は `core/role.Service.GetModerators` (モデレーター + 管理者 + root)。
// 審査 endpoint (`admin/signup-application/*`) が `RequireModerator` なので、
// この集合とそのまま一致する。
type ModeratorLister interface {
	GetModerators() ([]*model.User, error)
}

// ReceivedNotifier tells moderators that a new signup request arrived (#2987).
//
// 通知は副作用なので、失敗しても申請自体は成立させる (呼び出し側で握る)。
type ReceivedNotifier interface {
	NotifySignupApplicationReceived(ctx context.Context, app *model.SignupApplication) error
}

// NewReceivedNotifier adapts the notification service to ReceivedNotifier.
func NewReceivedNotifier(n InAppNotifier, lister ModeratorLister) ReceivedNotifier {
	if n == nil || lister == nil {
		return nil
	}
	return &receivedNotifier{inner: n, lister: lister}
}

type receivedNotifier struct {
	inner  InAppNotifier
	lister ModeratorLister
}

func (r *receivedNotifier) NotifySignupApplicationReceived(ctx context.Context, app *model.SignupApplication) error {
	if app == nil {
		return nil
	}
	// **宛先の数だけ同期で作る。** `/api/signup-application/apply` は未認証だが、
	// 前段に captcha と form-token があるので任意の回数は叩けない。宛先は
	// モデレーター (通常は数人) なので、非同期化するほどの量にならない。
	// abuseReport (#2868) も同じ構造。**宛先が数百に育つ構成では応答時間が
	// 比例して伸びる**ので、そうなったら queue へ逃がす。
	mods, err := r.lister.GetModerators()
	if err != nil {
		return err
	}
	for _, m := range mods {
		if m == nil {
			continue
		}
		// **NotifierID は空。** 申請者はまだアカウントを持っていないので、
		// 指せる利用者が存在しない。通知サービスは notifiee だけを要求する。
		//
		// **回答は載せない。** 申請フォームの回答は運営者が定義した任意の
		// 項目で、氏名や連絡先が入りうる。通知欄では読めないうえ、申請を
		// 消しても Redis に複製が残る。読み出し時に申請の行から引き直す
		// (#2868 の abuseReport と同じ形)。
		_, nerr := r.inner.Create(ctx, notification.CreateInput{
			NotifieeID: m.ID,
			Type:       notification.TypeSignupApplicationReceived,
			Extra: map[string]any{
				"applicationId": app.ID,
			},
		})
		if nerr != nil && !errors.Is(nerr, notification.ErrSelfNotification) {
			slog.Warn("signup-application: received notification failed",
				"moderator", m.ID, "application", app.ID, "err", nerr)
		}
	}
	return nil
}
