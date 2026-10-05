package notifications

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
)

// upstreamNotificationTypes is misskey-js の notificationTypes をリテラルで
// 写したもの (packages/misskey-js/src/consts.ts)。
//
// **notificationTypeList を使わない。** あれを送る形だと、リストに型を足した
// ときテストの入力も一緒に変わり、**upstream 由来のクライアントが送る入力を
// 誰も検査しなくなる**。#2898 の初版で実際にそうなり、既読位置が飛ぶ回帰が
// 緑のまま通った。
var upstreamNotificationTypes = []string{
	"note", "follow", "mention", "reply", "renote",
	"quote", "reaction", "pollEnded", "scheduledNotePosted",
	"scheduledNotePostFailed", "receiveFollowRequest", "followRequestAccepted",
	"roleAssigned", "chatRoomInvitationReceived", "achievementEarned",
	"exportCompleted", "login", "createToken", "app", "test",
}

// upstream 由来のクライアントが「すべて無効」で送る入力が、早期 return に
// 当たること (#2898)。
//
// fork frontend の MkNotificationSelectWindow.disableAll() と、misskey-js を
// 使う外部クライアントがこの入力を作る。抜けると maybeMarkAsRead まで進み、
// 1 件も返していないのにユーザーが受け取っていない通知まで既読になる。
func TestShow_ExcludeAllUpstreamTypesDoesNotMarkAsRead(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "bob", Type: notification.TypeFollow,
	})
	require.NoError(t, err)
	// mk-go 固有の型も 1 件積む。excludeTypes には入っていないが、
	// 「全部除外」と判定される以上こちらも返らない。
	_, err = svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeAbuseReport,
		Extra: map[string]any{"reportId": "r1"},
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	body, err := json.Marshal(map[string]any{"excludeTypes": upstreamNotificationTypes})
	require.NoError(t, err)
	c, rec := newJSONRequest(t, "/api/i/notifications", string(body))
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Show(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))

	assert.NotContains(t, pub.types("alice"), "readAllNotifications",
		"upstream の全種を除外した入力で readAllNotifications を publish してはいけない")
	readID, err := svc.LatestReadID(ctx, "alice")
	require.NoError(t, err)
	assert.Empty(t, readID, "upstream の全種を除外した入力で既読位置を進めてはいけない")
}

// grouped 側も同じ入力で早期 return する。
func TestGrouped_ExcludeAllUpstreamTypesDoesNotMarkAsRead(t *testing.T) {
	h, svc := newTestHandler(t)
	ctx := context.Background()
	_, err := svc.Create(ctx, notification.CreateInput{
		NotifieeID: "alice", NotifierID: "carol", Type: notification.TypeAbuseReport,
		Extra: map[string]any{"reportId": "r1"},
	})
	require.NoError(t, err)

	pub := &stubMainPublisher{}
	svc.SetMainStreamPublisher(pub)

	body, err := json.Marshal(map[string]any{"excludeTypes": upstreamNotificationTypes})
	require.NoError(t, err)
	c, rec := newJSONRequest(t, "/api/i/notifications-grouped", string(body))
	setAuth(c, &model.User{ID: "alice"})
	require.NoError(t, h.Grouped(c))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "[]", strings.TrimSpace(rec.Body.String()))
	assert.NotContains(t, pub.types("alice"), "readAllNotifications")
}
