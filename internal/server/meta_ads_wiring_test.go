package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
)

// **有効な広告が、metaを返す5つの経路すべてに同じ形で載ること(#3967)。**
//
// 入口の画面は、HTMLに埋め込まれたmetaを採ったときは/api/metaを取り直さない。
// 組み立てで広告のrepositoryを渡し忘れた経路は、広告を常に空で埋め込み、その
// 経路から入った人には入口のタイムラインに広告が出ない。実DBに広告を1件置き、
// 各経路の`ads`が/api/metaと一致し、その広告を含むことを見る。
func TestNewServer_WiresActiveAdsToEveryMetaEmbedding(t *testing.T) {
	db := serverIntegrationDB
	if db == nil {
		t.Skip("PostgreSQL unavailable")
	}
	cleanupPluginAccountUsers(t, db, "ad_user")
	createLocalUser(t, db, "ad-user", "ad_user", "adusertokenx3967", "ad-pass", false)

	const adID = "adwiring3967"
	now := time.Now()
	require.NoError(t, db.Exec(`DELETE FROM "ad" WHERE "id" = ?`, adID).Error)
	require.NoError(t, db.Create(&model.Ad{
		ID: adID, StartsAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		Place: "horizontal", Ratio: 1, URL: "https://ad.example/", ImageURL: "https://ad.example/i.png",
	}).Error)
	t.Cleanup(func() {
		require.NoError(t, db.Exec(`DELETE FROM "ad" WHERE "id" = ?`, adID).Error)
	})

	srv := newServerWithConfig(t, func(*config.Config) {})

	var want any
	for _, tt := range everyMetaEmbedding(t, srv, "ad_user", "ad-user") {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.meta(t)["ads"].([]any)
			require.True(t, ok, "ads must be an array")
			ids := make([]any, 0, len(got))
			for _, a := range got {
				ids = append(ids, a.(map[string]any)["id"])
			}
			require.Contains(t, ids, adID, "有効な広告が載っていない(広告のrepositoryが渡っていない)")
			// 先頭は/api/metaなので、以降の経路はそれと比べる。
			if want == nil {
				want = got
				return
			}
			assert.Equal(t, want, got, "/api/metaと広告の形が違う")
		})
	}
	require.NotNil(t, want, "比較の基準(/api/meta)が取れていない")
}
