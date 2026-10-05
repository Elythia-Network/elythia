package stream

import (
	"testing"

	"github.com/stretchr/testify/require"

	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// 申請の受付通知を realtime でも pack できること (#2987)。
//
// **`Pack` が lookup を渡していないと、通知ごと drop される。** そうなると
// 「一覧には出るが realtime でも未読バッジでも出ない」という非対称になる
// (`notification_service` は packer が nil を返すと publish も未読の更新も
// 丸ごと skip する)。router 側の配線は別の gate が見ているが、**その値を
// 使う側**はここでしか押さえられない。
func TestNotificationPublisher_PassesSignupApplicationLookup(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	p := &NotificationPublisher{}
	p.SetRepos(&stubNotifUserRepo{}, nil, idGen)

	n := &corenotification.Notification{
		ID: "n1", Type: corenotification.TypeSignupApplicationReceived,
		Extra: map[string]any{"applicationId": "s1"},
	}

	// lookup 未配線なら drop される (fail-closed)。
	require.Nil(t, p.Pack("mod1", n), "lookup 未配線で登録申請の通知を返している")

	var asked string
	p.SetSignupApplicationLookup(func(applicationID string) (entity.SignupApplicationStatus, bool) {
		asked = applicationID
		return entity.SignupApplicationStatus{Status: "pending"}, true
	})

	packed, ok := p.Pack("mod1", n).(map[string]any)
	require.True(t, ok, "lookup を配線しても登録申請の通知が返らない")
	require.Equal(t, "s1", asked, "Pack が lookup に applicationId を渡していない")
	app, ok := packed["signupApplication"].(map[string]any)
	require.True(t, ok, "read 時の状態が pack されていない: %v", packed)
	require.Equal(t, "pending", app["status"])
}

// 申請が消えていたら realtime でも drop する。
func TestNotificationPublisher_DropsDeletedSignupApplication(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	p := &NotificationPublisher{}
	p.SetRepos(&stubNotifUserRepo{}, nil, idGen)
	p.SetSignupApplicationLookup(func(string) (entity.SignupApplicationStatus, bool) {
		return entity.SignupApplicationStatus{}, false
	})

	require.Nil(t, p.Pack("mod1", &corenotification.Notification{
		ID: "n1", Type: corenotification.TypeSignupApplicationReceived,
		Extra: map[string]any{"applicationId": "gone"},
	}))
}

// 絵文字の申請も同じ経路を通る (lookup は Processed と共用)。
func TestNotificationPublisher_PassesEmojiApplicationLookupForReceived(t *testing.T) {
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	p := &NotificationPublisher{}
	p.SetRepos(&stubNotifUserRepo{user: &model.User{ID: "applicant", Username: "applicant"}}, nil, idGen)

	n := &corenotification.Notification{
		ID: "n1", Type: corenotification.TypeEmojiApplicationReceived, NotifierID: "applicant",
		Extra: map[string]any{"applicationId": "a1"},
	}
	require.Nil(t, p.Pack("mgr1", n), "lookup 未配線で絵文字の申請の通知を返している")

	p.SetEmojiApplicationLookup(func(string) (entity.EmojiApplicationStatus, bool) {
		return entity.EmojiApplicationStatus{Name: "sushi", Status: "pending"}, true
	})
	packed, ok := p.Pack("mgr1", n).(map[string]any)
	require.True(t, ok)
	app, ok := packed["emojiApplication"].(map[string]any)
	require.True(t, ok, "read 時の状態が pack されていない: %v", packed)
	require.Equal(t, "sushi", app["name"])
}
