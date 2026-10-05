package emojiapplication

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
)

type capturingNotifier struct {
	got  notification.CreateInput
	err  error
	made bool
}

func (c *capturingNotifier) Create(_ context.Context, in notification.CreateInput) (*notification.Notification, error) {
	c.got = in
	c.made = true
	return &notification.Notification{}, c.err
}

// TestResultNotifierOmitsNotifier asserts that the applicant is notified even
// when a moderator processes their own request (#2934).
//
// **通知サービスは notifier == notifiee を ErrSelfNotification で弾く。**
// NotifierID を渡すと、自分で自分の申請を処理したときに通知が出なくなる。
// 1 人運用のサーバーでは全件がその形になるので、機能が動いていないように見える。
func TestResultNotifierOmitsNotifier(t *testing.T) {
	inner := &capturingNotifier{}
	moderator := "u1"
	app := &model.EmojiApplication{
		ID:            "a1",
		UserID:        "u1", // 申請者 == 処理者
		Status:        model.EmojiApplicationApproved,
		ProcessedByID: &moderator,
	}

	require.NoError(t, NewResultNotifier(inner).NotifyEmojiApplicationProcessed(context.Background(), app))
	require.True(t, inner.made, "通知が作られていない")

	// **ここが本体。** NotifierID が入っていると notifiee と同じになり、
	// 実サービスなら ErrSelfNotification で弾かれる。
	require.Empty(t, inner.got.NotifierID,
		"NotifierID を渡すと、自分の申請を自分で処理したときに通知が出なくなる")
	require.Equal(t, "u1", inner.got.NotifieeID)
	require.Equal(t, notification.TypeEmojiApplicationProcessed, inner.got.Type)

	// 結果と却下理由は積まない。読み出し時に申請から引き直す。
	require.Equal(t, map[string]any{"applicationId": "a1"}, inner.got.Extra)
}

// TestResultNotifierPropagatesFailure asserts the error is not swallowed.
//
// 呼び出し側 (Service.notify) が握るので通知の失敗で審査は壊れないが、
// **ここで握ると握ったことすら分からなくなる。**
func TestResultNotifierPropagatesFailure(t *testing.T) {
	boom := errors.New("boom")
	inner := &capturingNotifier{err: boom}
	moderator := "mod"
	app := &model.EmojiApplication{ID: "a1", UserID: "u2", ProcessedByID: &moderator}

	require.ErrorIs(t, NewResultNotifier(inner).NotifyEmojiApplicationProcessed(context.Background(), app), boom)
}

// TestResultNotifierSkipsUnprocessed asserts nothing is sent for a row that was
// never reviewed (cancel など)。
func TestResultNotifierSkipsUnprocessed(t *testing.T) {
	inner := &capturingNotifier{}
	app := &model.EmojiApplication{ID: "a1", UserID: "u2"} // ProcessedByID が nil

	require.NoError(t, NewResultNotifier(inner).NotifyEmojiApplicationProcessed(context.Background(), app))
	require.False(t, inner.made, "審査していない申請で通知が作られている")
}
