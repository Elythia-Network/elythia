package entity

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
)

// packEmojiApplicationNotification is a thin wrapper so each case reads as
// "this notification, with this lookup, packs to this".
func packEmojiApplicationNotification(t *testing.T, applicationID string, opts ...NotificationOption) map[string]any {
	t.Helper()
	n := &notification.Notification{
		ID:        "n1",
		CreatedAt: time.Unix(0, 0),
		Type:      notification.TypeEmojiApplicationProcessed,
		Extra:     map[string]any{"applicationId": applicationID},
	}
	po := &packOptions{}
	for _, o := range opts {
		o(po)
	}
	return packNotificationCore(n, nil, nil, nil, nil, nil, po)
}

// TestEmojiApplicationNotificationPacksCurrentState asserts that the result and
// the reject reason are resolved at read time (#2934).
//
// **通知には applicationId しか積んでいない。** 結果を通知へ複製すると、申請を
// 消しても文面が Redis に残る。abuseReport (#2868) と同じ形。
func TestEmojiApplicationNotificationPacksCurrentState(t *testing.T) {
	t.Run("approved", func(t *testing.T) {
		out := packEmojiApplicationNotification(t, "a1", WithEmojiApplicationLookup(
			func(id string) (EmojiApplicationStatus, bool) {
				require.Equal(t, "a1", id)
				return EmojiApplicationStatus{Name: "sushi", Status: "approved"}, true
			}))
		require.NotNil(t, out)
		app, ok := out["emojiApplication"].(map[string]any)
		require.True(t, ok, "emojiApplication が入っていない")
		require.Equal(t, "approved", app["status"])
		require.Equal(t, "sushi", app["name"])
		// 承認なら理由の欄自体を出さない。空文字を出すと、フロントが
		// 「理由あり」として空の枠を描く。
		require.NotContains(t, app, "rejectReason")
	})

	t.Run("rejected は理由も出す", func(t *testing.T) {
		out := packEmojiApplicationNotification(t, "a2", WithEmojiApplicationLookup(
			func(string) (EmojiApplicationStatus, bool) {
				return EmojiApplicationStatus{
					Name: "kusa", Status: "rejected", RejectReason: "本文サイズだと潰れます",
				}, true
			}))
		require.NotNil(t, out)
		app := out["emojiApplication"].(map[string]any)
		require.Equal(t, "rejected", app["status"])
		require.Equal(t, "本文サイズだと潰れます", app["rejectReason"])
	})

	// **申請が消えていたら通知ごと落とす。** 経緯を辿れない「処理されました」
	// だけの通知は読んだ人に何も伝えない (roleAssigned と同じ形)。
	t.Run("申請が消えていたら通知ごと落とす", func(t *testing.T) {
		out := packEmojiApplicationNotification(t, "gone", WithEmojiApplicationLookup(
			func(string) (EmojiApplicationStatus, bool) { return EmojiApplicationStatus{}, false }))
		require.Nil(t, out)
	})

	// **未配線でも落とす (fail-closed)。** 解決できないまま素通りさせると、
	// 結果の分からない通知が出る。
	t.Run("lookup が未配線なら落とす", func(t *testing.T) {
		require.Nil(t, packEmojiApplicationNotification(t, "a1"))
	})
}
