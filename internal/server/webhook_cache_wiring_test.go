package server

import (
	"reflect"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 組み立てたサーバーで、利用者の webhook を引く service が、キャッシュの一覧に
// 登録したキャッシュ (userWebhooks) を通すこと (#4218)。キャッシュを通さないと、
// ノートやリアクションのたびに受け手ごとに DB を引く形に戻る。一覧に登録しないと、
// メンテナンスやリストアのときに捨てられない。
func TestBuiltServerWebhookServiceUsesTheRegisteredCache(t *testing.T) {
	srv := newServerWithConfig(t, func(*config.Config) {})

	hook := reflect.ValueOf(srv.builtCore.webhookReactionHook).Elem()
	svc := hook.FieldByName("svc")
	require.True(t, svc.IsValid())
	require.False(t, svc.IsNil())
	repo := svc.Elem().FieldByName("userRepo")
	require.True(t, repo.IsValid())
	require.False(t, repo.IsNil())

	var cached uintptr
	for _, p := range srv.caches.parts {
		if p.name == "userWebhooks" {
			cached = reflect.ValueOf(p.cache).Pointer()
		}
	}
	require.NotZero(t, cached, "userWebhooks is not in the cache list")
	assert.Equal(t, cached, repo.Elem().Pointer())
}
