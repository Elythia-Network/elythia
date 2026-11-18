package server

import (
	"reflect"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 組み立てたサーバーで、Web Push の service が、積む前に配信されるかを見る先
// (meta と購読のキャッシュ) を持っていること (#4217)。付け忘れると、購読の無い
// 利用者への通知でも本文を組み立てて積み、worker が取り出して捨てる形に戻る。
//
// 見る先は、キャッシュの一覧に登録したもの (cachedMeta と pushSubscriptions) と
// 同じインスタンスであること。購読のキャッシュは worker と sw/register も同じ
// webPushCache を使い、別のインスタンスだと sw/register の無効化が届かず、同じ
// プロセスでも購読した直後の通知が落ちる。meta も別のインスタンスだと、管理画面で
// Service Worker を有効にしても反映されない。
func TestBuiltServerWebPushChecksDeliveryBeforeEnqueue(t *testing.T) {
	srv := newServerWithConfig(t, func(*config.Config) {})

	pusher := reflect.ValueOf(srv.builtRouteParts.chatService).Elem().FieldByName("chatPusher")
	require.True(t, pusher.IsValid())
	require.False(t, pusher.IsNil())
	svc := pusher.Elem()
	require.Equal(t, "Service", svc.Elem().Type().Name())

	// 既読の push も同じ service を通る。
	readAll := reflect.ValueOf(srv.builtCore.notificationService).Elem().FieldByName("readAllPusher")
	require.True(t, readAll.IsValid())
	require.False(t, readAll.IsNil())
	assert.Equal(t, svc.Pointer(), readAll.Elem().Pointer())

	registered := map[string]uintptr{}
	for _, p := range srv.caches.parts {
		registered[p.name] = reflect.ValueOf(p.cache).Pointer()
	}
	for field, cacheName := range map[string]string{"meta": "cachedMeta", "subs": "pushSubscriptions"} {
		f := svc.Elem().FieldByName(field)
		require.True(t, f.IsValid(), field)
		require.False(t, f.IsNil(), field)
		require.Contains(t, registered, cacheName)
		assert.Equal(t, registered[cacheName], f.Elem().Pointer(), field)
	}
}
