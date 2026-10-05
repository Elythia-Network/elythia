package entity

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"gorm.io/datatypes"
)

type stubCanChatLookup struct {
	results map[string]bool
}

func (s *stubCanChatLookup) LookupCanChat(userID string) (bool, bool) {
	v, ok := s.results[userID]
	return v, ok
}

// TestPackUserDetailed_CanChatDefault: lookup が unwired のとき canChat=true
// (= upstream DefaultPolicies の `chatAvailability: "available"`) で fallback
// すること。既存 unit test が SetCanChatLookup を呼ばずに通るための path。
func TestPackUserDetailed_CanChatDefault(t *testing.T) {
	t.Cleanup(func() { SetCanChatLookup(nil) })
	SetCanChatLookup(nil)

	u := &model.User{
		ID:                "u1",
		Username:          "default",
		ChatScope:         "none", // 旧実装ではこの値で canChat=false だった
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	d := PackUserDetailed(u, nil)
	assert.True(t, d.CanChat, "lookup unwired → default true (upstream available)")
}

// TestPackUserDetailed_CanChatFromLookup: lookup が wired のとき lookup の結果が
// canChat に反映されること。role policy で chat 制限された user は false。
func TestPackUserDetailed_CanChatFromLookup(t *testing.T) {
	t.Cleanup(func() { SetCanChatLookup(nil) })
	SetCanChatLookup(&stubCanChatLookup{results: map[string]bool{
		"u1": true,
		"u2": false,
	}})

	u1 := &model.User{ID: "u1", Username: "ok", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	u2 := &model.User{ID: "u2", Username: "blocked", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	assert.True(t, PackUserDetailed(u1, nil).CanChat)
	assert.False(t, PackUserDetailed(u2, nil).CanChat)
}

// TestPackUserDetailed_CanChatLookupNotFound: lookup is wired but the user is not
// in the result map (= legacy data without role assignment). Should fall back
// to default true.
func TestPackUserDetailed_CanChatLookupNotFound(t *testing.T) {
	t.Cleanup(func() { SetCanChatLookup(nil) })
	SetCanChatLookup(&stubCanChatLookup{results: map[string]bool{}})

	u := &model.User{ID: "unknown", Username: "unknown", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	assert.True(t, PackUserDetailed(u, nil).CanChat, "lookup ok=false → default true")
}

// TestPackUserDetailed_CanChatIgnoresChatScope: chatScope の値に関係なく lookup
// が支配的になること (= upstream の semantics、chatScope は受信側設定で
// canChat とは別軸)。
func TestPackUserDetailed_CanChatIgnoresChatScope(t *testing.T) {
	t.Cleanup(func() { SetCanChatLookup(nil) })
	SetCanChatLookup(&stubCanChatLookup{results: map[string]bool{"u1": false}})

	for _, scope := range []string{"all", "followers", "mutuals", "none"} {
		u := &model.User{ID: "u1", Username: "x", ChatScope: scope, AvatarDecorations: datatypes.JSON([]byte("[]"))}
		assert.False(t, PackUserDetailed(u, nil).CanChat, "scope=%s should not affect canChat", scope)
	}
}

// BenchmarkPackUserDetailed_CanChatLookup measures the per-pack overhead of the
// canChat lookup. 100 user bulk pack (= users/show userIds path 最大値) で
// 数 µs / pack を期待する。lookup が hot path 化したときの regression を
// 検出する baseline (#988)。
func BenchmarkPackUserDetailed_CanChatLookup(b *testing.B) {
	b.Cleanup(func() { SetCanChatLookup(nil) })
	SetCanChatLookup(&stubCanChatLookup{results: map[string]bool{"u1": true}})
	u := &model.User{ID: "u1", Username: "bench", AvatarDecorations: datatypes.JSON([]byte("[]"))}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = PackUserDetailed(u, nil)
	}
}
