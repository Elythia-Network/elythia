package role_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/role"
	"github.com/stretchr/testify/assert"
)

type stubProvider struct {
	policies map[string]any
}

func (s *stubProvider) GetUserPolicies(_ string) map[string]any {
	return s.policies
}

func TestCanChatLookup_Available(t *testing.T) {
	lookup := role.NewCanChatLookup(&stubProvider{policies: map[string]any{
		"chatAvailability": "available",
	}})
	val, ok := lookup.LookupCanChat("u1")
	assert.True(t, ok)
	assert.True(t, val)
}

// "readonly" / "unavailable" は upstream の chatAvailability 列挙の残り 2 値、
// どちらも non-"available" なので canChat=false。
func TestCanChatLookup_NonAvailable(t *testing.T) {
	for _, v := range []string{"readonly", "unavailable", ""} {
		lookup := role.NewCanChatLookup(&stubProvider{policies: map[string]any{
			"chatAvailability": v,
		}})
		val, ok := lookup.LookupCanChat("u1")
		assert.True(t, ok, "value %q should resolve ok=true", v)
		assert.False(t, val, "value %q should resolve canChat=false", v)
	}
}

// chatAvailability key 自体が無い (= 壊れた policy data) のときは ok=false
// で entity 側 default に倒す。
func TestCanChatLookup_KeyMissing(t *testing.T) {
	lookup := role.NewCanChatLookup(&stubProvider{policies: map[string]any{
		"someOtherKey": true,
	}})
	_, ok := lookup.LookupCanChat("u1")
	assert.False(t, ok)
}

// 値が string でない場合も壊れた data として ok=false (= default).
func TestCanChatLookup_TypeMismatch(t *testing.T) {
	lookup := role.NewCanChatLookup(&stubProvider{policies: map[string]any{
		"chatAvailability": true, // bool, not string
	}})
	_, ok := lookup.LookupCanChat("u1")
	assert.False(t, ok)
}

// nil provider / nil adapter で panic せず ok=false を返すこと
// (= entity 側 default で fallback できる defensive guard)。
func TestCanChatLookup_NilProvider(t *testing.T) {
	lookup := role.NewCanChatLookup(nil)
	_, ok := lookup.LookupCanChat("u1")
	assert.False(t, ok)

	var nilAdapter *role.CanChatLookupAdapter
	_, ok = nilAdapter.LookupCanChat("u1")
	assert.False(t, ok)
}

// DefaultPolicies() の chatAvailability が "available" であることを検証
// (= 既定で全 user が canChat=true で動く)。
func TestDefaultPolicies_ChatAvailabilityAvailable(t *testing.T) {
	policies := role.DefaultPolicies()
	v, ok := policies["chatAvailability"]
	assert.True(t, ok, "default policies should contain chatAvailability")
	assert.Equal(t, "available", v)
}
