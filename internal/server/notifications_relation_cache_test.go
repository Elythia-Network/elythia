package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corenotification "github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// TestNotifications_MutesReadFromRelationCache checks on a whole server that
// i/notifications reads the viewer's mutes from the server's RelationCache
// (#4273), not from the muting table on every request.
//
// 配線が外れても、handler は mutingRepo を読み直すので応答は正しいまま文が 1 本
// 増えるだけになる。それを見分けるため、repository を通さずに表へ直接ミュートを
// 書く。キャッシュから読んでいれば、捨てるまでは古い一覧 (ミュート無し) のまま
// 通知が出て、捨てた後は落ちる。
func TestNotifications_MutesReadFromRelationCache(t *testing.T) {
	h := newMetaRuntimeHarness(t)
	srv := h.srv
	const viewerToken = "metartnotif00001"
	const viewerID, notifierID = "metart-notif-v", "metart-notif-n"
	h.createUser(viewerID, "metart_notif_v", viewerToken, false)
	h.createUser(notifierID, "metart_notif_n", "metartnotif00002", false)

	svc := corenotification.NewService(h.rdb, h.idGen, srv.config.Redis.KeyPrefix(), nil)
	_, err := svc.Create(context.Background(), corenotification.CreateInput{
		NotifieeID: viewerID, NotifierID: notifierID, Type: corenotification.TypeFollow,
	})
	require.NoError(t, err)

	list := func() int {
		t.Helper()
		rec := h.post("/api/i/notifications", viewerToken, map[string]any{"markAsRead": false})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp []map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		return len(resp)
	}
	require.Equal(t, 1, list(), "precondition: a notification from a user not muted is listed")

	muted, err := srv.relationCache.MutedUserIDs(viewerID)
	require.NoError(t, err)
	require.Empty(t, muted)
	muteID := h.idGen.Generate(time.Now())
	require.NoError(t, h.db.Create(&model.Muting{ID: muteID, MuterID: viewerID, MuteeID: notifierID}).Error)
	t.Cleanup(func() {
		if err := h.db.Where("id = ?", muteID).Delete(&model.Muting{}).Error; err != nil {
			t.Errorf("delete the mute: %v", err)
		}
	})

	require.Equal(t, 1, list(), "the mutes must come from the relation cache, which still holds the list without the mute")

	srv.relationCache.Invalidate(repository.RelationMuting, viewerID)
	require.Equal(t, 0, list(), "after the cache is dropped, the notifier's mute must apply")
}
