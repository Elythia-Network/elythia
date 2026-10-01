package role

import (
	"fmt"
	"log/slog"
)

// CanChatLookupAdapter wraps a role policy provider so that entity.PackUserLite
// can derive `canChat` from the upstream `chatAvailability === "available"`
// semantics (#988). Implements entity.CanChatLookup without creating a
// dependency from entity to core/role (= keeps the layering: api → core →
// entity, never the reverse).
type CanChatLookupAdapter struct {
	provider PolicyProvider
}

// NewCanChatLookup returns an entity.CanChatLookup that resolves a user's
// canChat status via role policies. Use entity.SetCanChatLookup(...) to wire.
//
// nil provider を渡された場合は nil adapter を返す代わりに usable な
// stub adapter (= 常に ok=false を返す) を返す。これにより呼び出し側
// (`router.go` の wire path) が nil チェック不要で済む。
func NewCanChatLookup(provider PolicyProvider) *CanChatLookupAdapter {
	return &CanChatLookupAdapter{provider: provider}
}

// LookupCanChat reads chatAvailability from the user's merged role policies.
// Returns (true, true) when chatAvailability == "available" (= default),
// (false, true) for other values ("readonly" / "unavailable" per upstream
// taxonomy). Returns ok=false only if the policy map is missing the key
// (= legacy data) or the value has an unexpected type, in which case
// PackUserLite falls back to default true.
func (a *CanChatLookupAdapter) LookupCanChat(userID string) (bool, bool) {
	if a == nil || a.provider == nil {
		return false, false
	}
	policies := a.provider.GetUserPolicies(userID)
	v, ok := policies["chatAvailability"]
	if !ok {
		// legacy data path: role.Service が古い persisted policy を
		// 返したとき。silent fallback (= default true) で十分。
		return false, false
	}
	s, ok := v.(string)
	if !ok {
		// マージ後 policies の chatAvailability は upstream の string enum
		// ("available" / "readonly" / "unavailable")。別型は壊れた data
		// なので運用 debug のため slog.Warn で残す (frontend は default
		// で動作可能、users/relation の transient error と同 pattern)。
		slog.Warn("canChat lookup: chatAvailability has unexpected type",
			"userID", userID, "type", fmt.Sprintf("%T", v))
		return false, false
	}
	return s == "available", true
}
