package emojiapplication

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

type countingReceivedNotifier struct {
	apps []*model.EmojiApplication
	err  error
}

func (c *countingReceivedNotifier) NotifyEmojiApplicationReceived(_ context.Context, app *model.EmojiApplication) error {
	c.apps = append(c.apps, app)
	return c.err
}

// **申請の作成が通知を発火すること自体を固定する (#2987)。** notifier 単体の
// テストだけだと、`Create` から呼ばれなくなっても両方緑のまま通る。
func TestCreate_NotifiesReviewers(t *testing.T) {
	svc := newService(t, newFakeApps(), &fakeEmojis{}, nil)
	notifier := &countingReceivedNotifier{}
	svc.SetReceivedNotifier(notifier)

	app, err := svc.Create(validInput())
	require.NoError(t, err)
	require.Len(t, notifier.apps, 1, "申請を作っても審査者に通知が飛んでいない")
	assert.Equal(t, app.ID, notifier.apps[0].ID)
}

// **通知の失敗で申請を失わせない。** 申請は永続化済みで、通知はそれに付随する
// 副作用。
func TestCreate_SucceedsWhenNotificationFails(t *testing.T) {
	svc := newService(t, newFakeApps(), &fakeEmojis{}, nil)
	svc.SetReceivedNotifier(&countingReceivedNotifier{err: errors.New("redis down")})

	app, err := svc.Create(validInput())
	require.NoError(t, err)
	assert.NotEmpty(t, app.ID)
}

// 未配線でも落ちない (既存の挙動)。
func TestCreate_WithoutReceivedNotifier(t *testing.T) {
	svc := newService(t, newFakeApps(), &fakeEmojis{}, nil)
	_, err := svc.Create(validInput())
	require.NoError(t, err)
}

// **作成に失敗したら通知しない。** 出ていない申請の通知が飛ぶと、審査画面を
// 開いても何も無い。
func TestCreate_DoesNotNotifyWhenCreateFails(t *testing.T) {
	apps := newFakeApps()
	apps.createErr = errors.New("db down")
	svc := newService(t, apps, &fakeEmojis{}, nil)
	notifier := &countingReceivedNotifier{}
	svc.SetReceivedNotifier(notifier)

	_, err := svc.Create(validInput())
	require.Error(t, err)
	assert.Empty(t, notifier.apps)
}
